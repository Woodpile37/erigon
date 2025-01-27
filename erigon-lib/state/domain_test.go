/*
   Copyright 2022 Erigon contributors

   Licensed under the Apache License, VerSsion 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/

package state

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"math"
	"math/rand"
	"sort"
	"strings"
	"testing"
	"time"

	datadir2 "github.com/ledgerwatch/erigon-lib/common/datadir"
	"github.com/ledgerwatch/erigon-lib/kv/iter"
	"github.com/ledgerwatch/erigon-lib/kv/order"
	"github.com/ledgerwatch/erigon-lib/types"

	"github.com/holiman/uint256"
	"github.com/ledgerwatch/log/v3"
	"github.com/stretchr/testify/require"
	btree2 "github.com/tidwall/btree"

	"github.com/ledgerwatch/erigon-lib/common"
	"github.com/ledgerwatch/erigon-lib/common/background"
	"github.com/ledgerwatch/erigon-lib/common/length"
	"github.com/ledgerwatch/erigon-lib/kv"
	"github.com/ledgerwatch/erigon-lib/kv/mdbx"
)

func testDbAndDomain(t *testing.T, logger log.Logger) (kv.RwDB, *Domain) {
	t.Helper()
	return testDbAndDomainOfStep(t, 16, logger)
}

func testDbAndDomainOfStep(t *testing.T, aggStep uint64, logger log.Logger) (kv.RwDB, *Domain) {
	t.Helper()
	dirs := datadir2.New(t.TempDir())
	keysTable := "Keys"
	valsTable := "Vals"
	historyKeysTable := "HistoryKeys"
	historyValsTable := "HistoryVals"
	settingsTable := "Settings" //nolint
	indexTable := "Index"
	db := mdbx.NewMDBX(logger).InMem(dirs.Chaindata).WithTableCfg(func(defaultBuckets kv.TableCfg) kv.TableCfg {
		tcfg := kv.TableCfg{
			keysTable:             kv.TableCfgItem{Flags: kv.DupSort},
			valsTable:             kv.TableCfgItem{},
			historyKeysTable:      kv.TableCfgItem{Flags: kv.DupSort},
			historyValsTable:      kv.TableCfgItem{Flags: kv.DupSort},
			settingsTable:         kv.TableCfgItem{},
			indexTable:            kv.TableCfgItem{Flags: kv.DupSort},
			kv.TblPruningProgress: kv.TableCfgItem{},
		}
		return tcfg
	}).MustOpen()
	t.Cleanup(db.Close)
	salt := uint32(1)
	cfg := domainCfg{
		hist: histCfg{
			iiCfg:             iiCfg{salt: &salt, dirs: dirs},
			withLocalityIndex: false, withExistenceIndex: true, compression: CompressNone, historyLargeValues: true,
		}}
	d, err := NewDomain(cfg, aggStep, "base", keysTable, valsTable, historyKeysTable, historyValsTable, indexTable, logger)
	require.NoError(t, err)
	d.DisableFsync()
	d.compressWorkers = 1
	t.Cleanup(d.Close)
	d.DisableFsync()
	return db, d
}

func TestDomain_CollationBuild(t *testing.T) {
	t.Run("compressDomainVals=true", func(t *testing.T) {
		testCollationBuild(t, true)
	})
	t.Run("compressDomainVals=false", func(t *testing.T) {
		testCollationBuild(t, false)
	})
}

func testCollationBuild(t *testing.T, compressDomainVals bool) {
	t.Helper()

	logger := log.New()
	logEvery := time.NewTicker(30 * time.Second)
	defer logEvery.Stop()
	db, d := testDbAndDomainOfStep(t, 16, logger)
	ctx := context.Background()

	if compressDomainVals {
		d.compression = CompressKeys | CompressVals
	}

	tx, err := db.BeginRw(ctx)
	require.NoError(t, err)
	defer tx.Rollback()
	dc := d.MakeContext()
	defer dc.Close()
	writer := dc.NewWriter()
	defer writer.close()

	writer.SetTxNum(2)

	var (
		k1     = []byte("key1")
		k2     = []byte("key2")
		v1     = []byte("value1.1")
		v2     = []byte("value2.1")
		p1, p2 []byte
	)

	err = writer.PutWithPrev(k1, nil, v1, p1)
	require.NoError(t, err)

	writer.SetTxNum(3)
	err = writer.PutWithPrev(k2, nil, v2, p2)
	require.NoError(t, err)

	p1, p2 = v1, v2
	_ = p2

	v1, v2 = []byte("value1.2"), []byte("value2.2") //nolint

	writer.SetTxNum(6)
	err = writer.PutWithPrev(k1, nil, v1, p1)
	require.NoError(t, err)

	p1, v1 = v1, []byte("value1.3")
	writer.SetTxNum(d.aggregationStep + 2)
	err = writer.PutWithPrev(k1, nil, v1, p1)
	require.NoError(t, err)

	p1, v1 = v1, []byte("value1.4")
	writer.SetTxNum(d.aggregationStep + 3)
	err = writer.PutWithPrev(k1, nil, v1, p1)
	require.NoError(t, err)

	p1, v1 = v1, []byte("value1.5")
	expectedStep2 := uint64(2)
	writer.SetTxNum(expectedStep2*d.aggregationStep + 2)
	err = writer.PutWithPrev(k1, nil, v1, p1)
	require.NoError(t, err)

	err = writer.Flush(ctx, tx)
	require.NoError(t, err)
	dc.Close()
	{
		c, err := d.collate(ctx, 0, 0, 16, tx)

		require.NoError(t, err)
		require.True(t, strings.HasSuffix(c.valuesPath, "v1-base.0-1.kv"))
		require.Equal(t, 2, c.valuesCount)
		require.True(t, strings.HasSuffix(c.historyPath, "v1-base.0-1.v"))
		require.Equal(t, 3, c.historyCount)
		require.Equal(t, 2, len(c.indexBitmaps))
		require.Equal(t, []uint64{3}, c.indexBitmaps["key2"].ToArray())
		require.Equal(t, []uint64{2, 6}, c.indexBitmaps["key1"].ToArray())

		sf, err := d.buildFiles(ctx, 0, c, background.NewProgressSet())
		require.NoError(t, err)
		defer sf.CleanupOnError()
		c.Close()

		g := NewArchiveGetter(sf.valuesDecomp.MakeGetter(), d.compression)
		g.Reset(0)
		var words []string
		for g.HasNext() {
			w, _ := g.Next(nil)
			words = append(words, string(w))
		}
		require.Equal(t, []string{"key1", "value1.2", "key2", "value2.1"}, words)
		// Check index
		//require.Equal(t, 2, int(sf.valuesIdx.KeyCount()))
		require.Equal(t, 2, int(sf.valuesBt.KeyCount()))

		//r := recsplit.NewIndexReader(sf.valuesIdx)
		//defer r.Close()
		//for i := 0; i < len(words); i += 2 {
		//	offset := r.Lookup([]byte(words[i]))
		//	g.Reset(offset)
		//	w, _ := g.Next(nil)
		//	require.Equal(t, words[i], string(w))
		//	w, _ = g.Next(nil)
		//	require.Equal(t, words[i+1], string(w))
		//}

		for i := 0; i < len(words); i += 2 {
			c, _ := sf.valuesBt.SeekDeprecated([]byte(words[i]))
			require.Equal(t, words[i], string(c.Key()))
			require.Equal(t, words[i+1], string(c.Value()))
		}
	}
	{
		c, err := d.collate(ctx, 1, 1*d.aggregationStep, 2*d.aggregationStep, tx)
		require.NoError(t, err)
		sf, err := d.buildFiles(ctx, 1, c, background.NewProgressSet())
		require.NoError(t, err)
		defer sf.CleanupOnError()
		c.Close()

		g := sf.valuesDecomp.MakeGetter()
		g.Reset(0)
		var words []string
		for g.HasNext() {
			w, _ := g.Next(nil)
			words = append(words, string(w))
		}
		require.Equal(t, []string{"key1", "value1.4"}, words)
		// Check index
		require.Equal(t, 1, int(sf.valuesBt.KeyCount()))
		for i := 0; i < len(words); i += 2 {
			c, _ := sf.valuesBt.SeekDeprecated([]byte(words[i]))
			require.Equal(t, words[i], string(c.Key()))
			require.Equal(t, words[i+1], string(c.Value()))
		}

		//require.Equal(t, 1, int(sf.valuesIdx.KeyCount()))
		//r := recsplit.NewIndexReader(sf.valuesIdx)
		//defer r.Close()
		//for i := 0; i < len(words); i += 2 {
		//	offset := r.Lookup([]byte(words[i]))
		//	g.Reset(offset)
		//	w, _ := g.Next(nil)
		//	require.Equal(t, words[i], string(w))
		//	w, _ = g.Next(nil)
		//	require.Equal(t, words[i+1], string(w))
		//}
	}
}

func TestDomain_IterationBasic(t *testing.T) {
	logger := log.New()
	db, d := testDbAndDomain(t, logger)
	ctx := context.Background()
	tx, err := db.BeginRw(ctx)
	require.NoError(t, err)
	defer tx.Rollback()
	dc := d.MakeContext()
	defer dc.Close()
	writer := dc.NewWriter()
	defer writer.close()

	writer.SetTxNum(2)
	err = writer.PutWithPrev([]byte("addr1"), []byte("loc1"), []byte("value1"), nil)
	require.NoError(t, err)
	err = writer.PutWithPrev([]byte("addr1"), []byte("loc2"), []byte("value1"), nil)
	require.NoError(t, err)
	err = writer.PutWithPrev([]byte("addr1"), []byte("loc3"), []byte("value1"), nil)
	require.NoError(t, err)
	err = writer.PutWithPrev([]byte("addr2"), []byte("loc1"), []byte("value1"), nil)
	require.NoError(t, err)
	err = writer.PutWithPrev([]byte("addr2"), []byte("loc2"), []byte("value1"), nil)
	require.NoError(t, err)
	err = writer.PutWithPrev([]byte("addr3"), []byte("loc1"), []byte("value1"), nil)
	require.NoError(t, err)
	err = writer.PutWithPrev([]byte("addr3"), []byte("loc2"), []byte("value1"), nil)
	require.NoError(t, err)
	err = writer.Flush(ctx, tx)
	require.NoError(t, err)
	dc.Close()

	dc = d.MakeContext()
	defer dc.Close()

	{
		var keys, vals []string
		err = dc.IteratePrefix(tx, []byte("addr2"), func(k, v []byte) error {
			keys = append(keys, string(k))
			vals = append(vals, string(v))
			return nil
		})
		require.NoError(t, err)
		require.Equal(t, []string{"addr2loc1", "addr2loc2"}, keys)
		require.Equal(t, []string{"value1", "value1"}, vals)
	}
	{
		var keys, vals []string
		iter2, err := dc.IteratePrefix2(tx, []byte("addr2"), []byte("addr3"), -1)
		require.NoError(t, err)
		for iter2.HasNext() {
			k, v, err := iter2.Next()
			require.NoError(t, err)
			keys = append(keys, string(k))
			vals = append(vals, string(v))
		}
		require.Equal(t, []string{"addr2loc1", "addr2loc2"}, keys)
		require.Equal(t, []string{"value1", "value1"}, vals)
	}
}

func TestDomain_AfterPrune(t *testing.T) {
	logger := log.New()
	logEvery := time.NewTicker(30 * time.Second)
	defer logEvery.Stop()
	db, d := testDbAndDomain(t, logger)
	ctx := context.Background()

	tx, err := db.BeginRw(ctx)
	require.NoError(t, err)
	defer tx.Rollback()
	dc := d.MakeContext()
	defer d.Close()
	writer := dc.NewWriter()
	defer writer.close()

	var (
		k1 = []byte("key1")
		k2 = []byte("key2")
		p1 []byte
		p2 []byte

		n1, n2 = []byte("value1.1"), []byte("value2.1")
	)

	writer.SetTxNum(2)
	err = writer.PutWithPrev(k1, nil, n1, p1)
	require.NoError(t, err)

	writer.SetTxNum(3)
	err = writer.PutWithPrev(k2, nil, n2, p2)
	require.NoError(t, err)

	p1, p2 = n1, n2
	n1, n2 = []byte("value1.2"), []byte("value2.2")

	writer.SetTxNum(6)
	err = writer.PutWithPrev(k1, nil, n1, p1)
	require.NoError(t, err)

	p1, n1 = n1, []byte("value1.3")

	writer.SetTxNum(17)
	err = writer.PutWithPrev(k1, nil, n1, p1)
	require.NoError(t, err)

	p1 = n1

	writer.SetTxNum(18)
	err = writer.PutWithPrev(k2, nil, n2, p2)
	require.NoError(t, err)
	p2 = n2

	err = writer.Flush(ctx, tx)
	require.NoError(t, err)

	c, err := d.collate(ctx, 0, 0, 16, tx)
	require.NoError(t, err)

	sf, err := d.buildFiles(ctx, 0, c, background.NewProgressSet())
	require.NoError(t, err)

	d.integrateFiles(sf, 0, 16)
	var v []byte
	dc = d.MakeContext()
	defer dc.Close()
	v, found, err := dc.GetLatest(k1, nil, tx)
	require.Truef(t, found, "key1 not found")
	require.NoError(t, err)
	require.Equal(t, p1, v)
	v, found, err = dc.GetLatest(k2, nil, tx)
	require.Truef(t, found, "key2 not found")
	require.NoError(t, err)
	require.Equal(t, p2, v)

	err = dc.Prune(ctx, tx, 0, 0, 16, math.MaxUint64, logEvery)
	require.NoError(t, err)

	isEmpty, err := d.isEmpty(tx)
	require.NoError(t, err)
	require.False(t, isEmpty)

	v, found, err = dc.GetLatest(k1, nil, tx)
	require.NoError(t, err)
	require.Truef(t, found, "key1 not found")
	require.Equal(t, p1, v)

	v, found, err = dc.GetLatest(k2, nil, tx)
	require.NoError(t, err)
	require.Truef(t, found, "key2 not found")
	require.Equal(t, p2, v)
}

func filledDomain(t *testing.T, logger log.Logger) (kv.RwDB, *Domain, uint64) {
	t.Helper()
	require := require.New(t)
	db, d := testDbAndDomain(t, logger)
	ctx := context.Background()
	tx, err := db.BeginRw(ctx)
	require.NoError(err)
	defer tx.Rollback()

	txs := uint64(1000)

	dc := d.MakeContext()
	defer dc.Close()
	writer := dc.NewWriter()
	defer writer.close()

	var prev [32][]byte
	// keys are encodings of numbers 1..31
	// each key changes value on every txNum which is multiple of the key
	for txNum := uint64(1); txNum <= txs; txNum++ {
		writer.SetTxNum(txNum)
		for keyNum := uint64(1); keyNum <= uint64(31); keyNum++ {
			if txNum%keyNum == 0 {
				valNum := txNum / keyNum
				var k [8]byte
				var v [8]byte
				binary.BigEndian.PutUint64(k[:], keyNum)
				binary.BigEndian.PutUint64(v[:], valNum)
				err = writer.PutWithPrev(k[:], nil, v[:], prev[keyNum])
				prev[keyNum] = v[:]

				require.NoError(err)
			}
		}
		if txNum%10 == 0 {
			err = writer.Flush(ctx, tx)
			require.NoError(err)
		}
	}
	err = writer.Flush(ctx, tx)
	require.NoError(err)
	err = tx.Commit()
	require.NoError(err)
	return db, d, txs
}

func checkHistory(t *testing.T, db kv.RwDB, d *Domain, txs uint64) {
	t.Helper()
	fmt.Printf("txs: %d\n", txs)
	t.Helper()
	require := require.New(t)
	ctx := context.Background()
	var err error

	// Check the history
	dc := d.MakeContext()
	defer dc.Close()
	roTx, err := db.BeginRo(ctx)
	require.NoError(err)
	defer roTx.Rollback()

	for txNum := uint64(0); txNum <= txs; txNum++ {
		for keyNum := uint64(1); keyNum <= uint64(31); keyNum++ {
			valNum := txNum / keyNum
			var k [8]byte
			var v [8]byte
			binary.BigEndian.PutUint64(k[:], keyNum)
			binary.BigEndian.PutUint64(v[:], valNum)

			label := fmt.Sprintf("key %x txNum=%d, keyNum=%d", k, txNum, keyNum)

			val, err := dc.GetAsOf(k[:], txNum+1, roTx)
			require.NoError(err, label)
			if txNum >= keyNum {
				require.Equal(v[:], val, label)
			} else {
				require.Nil(val, label)
			}
			if txNum == txs {
				val, found, err := dc.GetLatest(k[:], nil, roTx)
				require.True(found, label)
				require.NoError(err)
				require.EqualValues(v[:], val, label)
			}
		}
	}
}

func TestHistory(t *testing.T) {
	logger := log.New()
	logEvery := time.NewTicker(30 * time.Second)
	defer logEvery.Stop()
	db, d, txs := filledDomain(t, logger)
	//ctx := context.Background()
	//tx, err := db.BeginRw(ctx)
	//require.NoError(t, err)
	//defer tx.Rollback()

	collateAndMerge(t, db, nil, d, txs)
	checkHistory(t, db, d, txs)
}

func TestIterationMultistep(t *testing.T) {
	logger := log.New()
	logEvery := time.NewTicker(30 * time.Second)
	defer logEvery.Stop()
	db, d := testDbAndDomain(t, logger)
	ctx := context.Background()
	tx, err := db.BeginRw(ctx)
	require.NoError(t, err)
	defer tx.Rollback()
	dc := d.MakeContext()
	defer dc.Close()
	writer := dc.NewWriter()
	defer writer.close()

	writer.SetTxNum(2)
	err = writer.PutWithPrev([]byte("addr1"), []byte("loc1"), []byte("value1"), nil)
	require.NoError(t, err)
	err = writer.PutWithPrev([]byte("addr1"), []byte("loc2"), []byte("value1"), nil)
	require.NoError(t, err)
	err = writer.PutWithPrev([]byte("addr1"), []byte("loc3"), []byte("value1"), nil)
	require.NoError(t, err)
	err = writer.PutWithPrev([]byte("addr2"), []byte("loc1"), []byte("value1"), nil)
	require.NoError(t, err)
	err = writer.PutWithPrev([]byte("addr2"), []byte("loc2"), []byte("value1"), nil)
	require.NoError(t, err)
	err = writer.PutWithPrev([]byte("addr3"), []byte("loc1"), []byte("value1"), nil)
	require.NoError(t, err)
	err = writer.PutWithPrev([]byte("addr3"), []byte("loc2"), []byte("value1"), nil)
	require.NoError(t, err)

	writer.SetTxNum(2 + 16)
	err = writer.PutWithPrev([]byte("addr2"), []byte("loc1"), []byte("value1"), nil)
	require.NoError(t, err)
	err = writer.PutWithPrev([]byte("addr2"), []byte("loc2"), []byte("value1"), nil)
	require.NoError(t, err)
	err = writer.PutWithPrev([]byte("addr2"), []byte("loc3"), []byte("value1"), nil)
	require.NoError(t, err)
	err = writer.PutWithPrev([]byte("addr2"), []byte("loc4"), []byte("value1"), nil)
	require.NoError(t, err)

	writer.SetTxNum(2 + 16 + 16)
	err = writer.DeleteWithPrev([]byte("addr2"), []byte("loc1"), nil)
	require.NoError(t, err)

	err = writer.Flush(ctx, tx)
	require.NoError(t, err)
	dc.Close()

	for step := uint64(0); step <= 2; step++ {
		func() {
			c, err := d.collate(ctx, step, step*d.aggregationStep, (step+1)*d.aggregationStep, tx)
			require.NoError(t, err)
			sf, err := d.buildFiles(ctx, step, c, background.NewProgressSet())
			require.NoError(t, err)
			d.integrateFiles(sf, step*d.aggregationStep, (step+1)*d.aggregationStep)

			dc := d.MakeContext()
			err = dc.Prune(ctx, tx, step, step*d.aggregationStep, (step+1)*d.aggregationStep, math.MaxUint64, logEvery)
			dc.Close()
			require.NoError(t, err)
		}()
	}
	dc.Close()

	dc = d.MakeContext()
	defer dc.Close()

	{
		var keys, vals []string
		err = dc.IteratePrefix(tx, []byte("addr2"), func(k, v []byte) error {
			keys = append(keys, string(k))
			vals = append(vals, string(v))
			return nil
		})
		require.NoError(t, err)
		require.Equal(t, []string{"addr2loc2", "addr2loc3", "addr2loc4"}, keys)
		require.Equal(t, []string{"value1", "value1", "value1"}, vals)
	}
	{
		var keys, vals []string
		iter2, err := dc.IteratePrefix2(tx, []byte("addr2"), []byte("addr3"), -1)
		require.NoError(t, err)
		for iter2.HasNext() {
			k, v, err := iter2.Next()
			require.NoError(t, err)
			keys = append(keys, string(k))
			vals = append(vals, string(v))
		}
		require.Equal(t, []string{"addr2loc2", "addr2loc3", "addr2loc4"}, keys)
		require.Equal(t, []string{"value1", "value1", "value1"}, vals)
	}
}

func collateAndMerge(t *testing.T, db kv.RwDB, tx kv.RwTx, d *Domain, txs uint64) {
	t.Helper()

	logEvery := time.NewTicker(30 * time.Second)
	defer logEvery.Stop()
	ctx := context.Background()
	var err error
	useExternalTx := tx != nil
	if !useExternalTx {
		tx, err = db.BeginRwNosync(ctx)
		require.NoError(t, err)
		defer tx.Rollback()
	}
	// Leave the last 2 aggregation steps un-collated
	for step := uint64(0); step < txs/d.aggregationStep-1; step++ {
		c, err := d.collate(ctx, step, step*d.aggregationStep, (step+1)*d.aggregationStep, tx)
		require.NoError(t, err)
		sf, err := d.buildFiles(ctx, step, c, background.NewProgressSet())
		require.NoError(t, err)
		d.integrateFiles(sf, step*d.aggregationStep, (step+1)*d.aggregationStep)

		dc := d.MakeContext()
		err = dc.Prune(ctx, tx, step, step*d.aggregationStep, (step+1)*d.aggregationStep, math.MaxUint64, logEvery)
		dc.Close()
		require.NoError(t, err)
	}
	var r DomainRanges
	maxEndTxNum := d.endTxNumMinimax()
	maxSpan := d.aggregationStep * StepsInColdFile

	for {
		if stop := func() bool {
			dc := d.MakeContext()
			defer dc.Close()
			r = dc.findMergeRange(maxEndTxNum, maxSpan)
			if !r.any() {
				return true
			}
			valuesOuts, indexOuts, historyOuts, _ := dc.staticFilesInRange(r)
			valuesIn, indexIn, historyIn, err := dc.mergeFiles(ctx, valuesOuts, indexOuts, historyOuts, r, background.NewProgressSet())
			require.NoError(t, err)
			if valuesIn != nil && valuesIn.decompressor != nil {
				fmt.Printf("merge: %s\n", valuesIn.decompressor.FileName())
			}
			d.integrateMergedFiles(valuesOuts, indexOuts, historyOuts, valuesIn, indexIn, historyIn)
			return false
		}(); stop {
			break
		}
	}
	if !useExternalTx {
		err := tx.Commit()
		require.NoError(t, err)
	}
}

func collateAndMergeOnce(t *testing.T, d *Domain, tx kv.RwTx, step uint64) {
	t.Helper()
	logEvery := time.NewTicker(30 * time.Second)
	defer logEvery.Stop()
	ctx := context.Background()
	txFrom, txTo := (step)*d.aggregationStep, (step+1)*d.aggregationStep

	c, err := d.collate(ctx, step, txFrom, txTo, tx)
	require.NoError(t, err)

	sf, err := d.buildFiles(ctx, step, c, background.NewProgressSet())
	require.NoError(t, err)
	d.integrateFiles(sf, txFrom, txTo)

	dc := d.MakeContext()
	err = dc.Prune(ctx, tx, step, txFrom, txTo, math.MaxUint64, logEvery)
	dc.Close()
	require.NoError(t, err)

	maxEndTxNum := d.endTxNumMinimax()
	maxSpan := d.aggregationStep * StepsInColdFile
	for {
		dc := d.MakeContext()
		r := dc.findMergeRange(maxEndTxNum, maxSpan)
		if !r.any() {
			dc.Close()
			break
		}
		valuesOuts, indexOuts, historyOuts, _ := dc.staticFilesInRange(r)
		valuesIn, indexIn, historyIn, err := dc.mergeFiles(ctx, valuesOuts, indexOuts, historyOuts, r, background.NewProgressSet())
		require.NoError(t, err)

		d.integrateMergedFiles(valuesOuts, indexOuts, historyOuts, valuesIn, indexIn, historyIn)
		dc.Close()
	}
}

func TestDomain_MergeFiles(t *testing.T) {

	logger := log.New()
	db, d, txs := filledDomain(t, logger)
	rwTx, err := db.BeginRw(context.Background())
	require.NoError(t, err)

	collateAndMerge(t, db, rwTx, d, txs)
	err = rwTx.Commit()
	require.NoError(t, err)
	checkHistory(t, db, d, txs)
}

func TestDomain_ScanFiles(t *testing.T) {

	logger := log.New()
	db, d, txs := filledDomain(t, logger)
	collateAndMerge(t, db, nil, d, txs)
	// Recreate domain and re-scan the files
	dc := d.MakeContext()
	defer dc.Close()
	d.closeWhatNotInList([]string{})
	require.NoError(t, d.OpenFolder(false))

	// Check the history
	checkHistory(t, db, d, txs)
}

func TestDomain_Delete(t *testing.T) {

	logger := log.New()
	db, d := testDbAndDomain(t, logger)
	ctx, require := context.Background(), require.New(t)
	tx, err := db.BeginRw(ctx)
	require.NoError(err)
	defer tx.Rollback()
	dc := d.MakeContext()
	defer dc.Close()
	writer := dc.NewWriter()
	defer writer.close()

	// Put on even txNum, delete on odd txNum
	for txNum := uint64(0); txNum < uint64(1000); txNum++ {
		writer.SetTxNum(txNum)
		original, _, err := dc.GetLatest([]byte("key1"), nil, tx)
		require.NoError(err)
		if txNum%2 == 0 {
			err = writer.PutWithPrev([]byte("key1"), nil, []byte("value1"), original)
		} else {
			err = writer.DeleteWithPrev([]byte("key1"), nil, original)
		}
		require.NoError(err)
	}
	err = writer.Flush(ctx, tx)
	require.NoError(err)
	collateAndMerge(t, db, tx, d, 1000)
	dc.Close()

	// Check the history
	dc = d.MakeContext()
	defer dc.Close()
	for txNum := uint64(0); txNum < 1000; txNum++ {
		label := fmt.Sprintf("txNum=%d", txNum)
		//val, ok, err := dc.GetLatestBeforeTxNum([]byte("key1"), txNum+1, tx)
		//require.NoError(err)
		//require.True(ok)
		//if txNum%2 == 0 {
		//	require.Equal([]byte("value1"), val, label)
		//} else {
		//	require.Nil(val, label)
		//}
		//if txNum == 976 {
		val, err := dc.GetAsOf([]byte("key2"), txNum+1, tx)
		require.NoError(err)
		//require.False(ok, label)
		require.Nil(val, label)
		//}
	}
}

func filledDomainFixedSize(t *testing.T, keysCount, txCount, aggStep uint64, logger log.Logger) (kv.RwDB, *Domain, map[uint64][]bool) {
	t.Helper()
	db, d := testDbAndDomainOfStep(t, aggStep, logger)
	ctx := context.Background()
	tx, err := db.BeginRw(ctx)
	require.NoError(t, err)
	defer tx.Rollback()
	dc := d.MakeContext()
	defer dc.Close()
	writer := dc.NewWriter()
	defer writer.close()

	// keys are encodings of numbers 1..31
	// each key changes value on every txNum which is multiple of the key
	dat := make(map[uint64][]bool) // K:V is key -> list of bools. If list[i] == true, i'th txNum should persists

	var k [8]byte
	var v [8]byte
	maxFrozenFiles := (txCount / d.aggregationStep) / StepsInColdFile
	prev := map[string]string{}

	// key 0: only in frozen file 0
	// key 1: only in frozen file 1 and file 2
	// key 2: in frozen file 2 and in warm files
	// other keys: only in warm files
	for txNum := uint64(1); txNum <= txCount; txNum++ {
		writer.SetTxNum(txNum)
		step := txNum / d.aggregationStep
		frozenFileNum := step / 32
		for keyNum := uint64(0); keyNum < keysCount; keyNum++ {
			if frozenFileNum < maxFrozenFiles { // frozen data
				allowInsert := (keyNum == 0 && frozenFileNum == 0) ||
					(keyNum == 1 && (frozenFileNum == 1 || frozenFileNum == 2)) ||
					(keyNum == 2 && frozenFileNum == 2)
				if !allowInsert {
					continue
				}
				//fmt.Printf("put frozen: %d, step=%d, %d\n", keyNum, step, frozenFileNum)
			} else { //warm data
				if keyNum == 0 || keyNum == 1 {
					continue
				}
				if keyNum == txNum%d.aggregationStep {
					continue
				}
				//fmt.Printf("put: %d, step=%d\n", keyNum, step)
			}

			binary.BigEndian.PutUint64(k[:], keyNum)
			binary.BigEndian.PutUint64(v[:], txNum)
			//v[0] = 3 // value marker
			err = writer.PutWithPrev(k[:], nil, v[:], []byte(prev[string(k[:])]))
			require.NoError(t, err)
			if _, ok := dat[keyNum]; !ok {
				dat[keyNum] = make([]bool, txCount+1)
			}
			dat[keyNum][txNum] = true

			prev[string(k[:])] = string(v[:])
		}
		if txNum%d.aggregationStep == 0 {
			err = writer.Flush(ctx, tx)
			require.NoError(t, err)
		}
	}
	err = tx.Commit()
	require.NoError(t, err)
	return db, d, dat
}

// firstly we write all the data to domain
// then we collate-merge-prune
// then check.
// in real life we periodically do collate-merge-prune without stopping adding data
func TestDomain_Prune_AfterAllWrites(t *testing.T) {

	logger := log.New()
	keyCount, txCount := uint64(4), uint64(64)
	db, dom, data := filledDomainFixedSize(t, keyCount, txCount, 16, logger)
	collateAndMerge(t, db, nil, dom, txCount)
	maxFrozenFiles := (txCount / dom.aggregationStep) / StepsInColdFile

	ctx := context.Background()
	roTx, err := db.BeginRo(ctx)
	require.NoError(t, err)
	defer roTx.Rollback()

	// Check the history
	dc := dom.MakeContext()
	defer dc.Close()
	var k, v [8]byte

	for txNum := uint64(1); txNum <= txCount; txNum++ {
		for keyNum := uint64(0); keyNum < keyCount; keyNum++ {
			step := txNum / dom.aggregationStep
			frozenFileNum := step / 32
			if frozenFileNum < maxFrozenFiles { // frozen data
				if keyNum != frozenFileNum {
					continue
				}
				continue
				//fmt.Printf("put frozen: %d, step=%d, %d\n", keyNum, step, frozenFileNum)
			} else { //warm data
				if keyNum == 0 || keyNum == 1 {
					continue
				}
				if keyNum == txNum%dom.aggregationStep {
					continue
				}
				//fmt.Printf("put: %d, step=%d\n", keyNum, step)
			}

			label := fmt.Sprintf("txNum=%d, keyNum=%d\n", txNum, keyNum)
			binary.BigEndian.PutUint64(k[:], keyNum)
			binary.BigEndian.PutUint64(v[:], txNum)

			val, err := dc.GetAsOf(k[:], txNum+1, roTx)
			// during generation such keys are skipped so value should be nil for this call
			require.NoError(t, err, label)
			if !data[keyNum][txNum] {
				if txNum > 1 {
					binary.BigEndian.PutUint64(v[:], txNum-1)
				} else {
					require.Nil(t, val, label)
					continue
				}
			}
			require.EqualValues(t, v[:], val)
		}
	}

	//warm keys
	binary.BigEndian.PutUint64(v[:], txCount)
	for keyNum := uint64(2); keyNum < keyCount; keyNum++ {
		label := fmt.Sprintf("txNum=%d, keyNum=%d\n", txCount-1, keyNum)
		binary.BigEndian.PutUint64(k[:], keyNum)

		storedV, found, err := dc.GetLatest(k[:], nil, roTx)
		require.Truef(t, found, label)
		require.NoError(t, err, label)
		require.EqualValues(t, v[:], storedV, label)
	}
}

func TestDomain_PruneOnWrite(t *testing.T) {

	logger := log.New()
	keysCount, txCount := uint64(16), uint64(64)

	db, d := testDbAndDomain(t, logger)
	ctx := context.Background()

	tx, err := db.BeginRw(ctx)
	require.NoError(t, err)
	defer tx.Rollback()
	dc := d.MakeContext()
	defer dc.Close()
	writer := dc.NewWriter()
	defer writer.close()

	// keys are encodings of numbers 1..31
	// each key changes value on every txNum which is multiple of the key
	data := make(map[string][]uint64)

	prev := map[string]string{}

	for txNum := uint64(1); txNum <= txCount; txNum++ {
		writer.SetTxNum(txNum)
		for keyNum := uint64(1); keyNum <= keysCount; keyNum++ {
			if keyNum == txNum%d.aggregationStep {
				continue
			}
			var k [8]byte
			var v [8]byte
			binary.BigEndian.PutUint64(k[:], keyNum)
			binary.BigEndian.PutUint64(v[:], txNum)
			err = writer.PutWithPrev(k[:], nil, v[:], []byte(prev[string(k[:])]))
			require.NoError(t, err)

			prev[string(k[:])] = string(v[:])

			list, ok := data[fmt.Sprintf("%d", keyNum)]
			if !ok {
				data[fmt.Sprintf("%d", keyNum)] = make([]uint64, 0)
			}
			data[fmt.Sprintf("%d", keyNum)] = append(list, txNum)
		}
		if txNum%d.aggregationStep == 0 {
			step := txNum/d.aggregationStep - 1
			if step == 0 {
				continue
			}
			step--
			err = writer.Flush(ctx, tx)
			require.NoError(t, err)

			collateAndMergeOnce(t, d, tx, step)
		}
	}
	err = writer.Flush(ctx, tx)
	require.NoError(t, err)
	dc.Close()

	// Check the history
	dc = d.MakeContext()
	defer dc.Close()
	for txNum := uint64(1); txNum <= txCount; txNum++ {
		for keyNum := uint64(1); keyNum <= keysCount; keyNum++ {
			valNum := txNum
			var k [8]byte
			var v [8]byte
			label := fmt.Sprintf("txNum=%d, keyNum=%d\n", txNum, keyNum)
			binary.BigEndian.PutUint64(k[:], keyNum)
			binary.BigEndian.PutUint64(v[:], valNum)

			val, err := dc.GetAsOf(k[:], txNum+1, tx)
			require.NoError(t, err)
			if keyNum == txNum%d.aggregationStep {
				if txNum > 1 {
					binary.BigEndian.PutUint64(v[:], txNum-1)
					require.EqualValues(t, v[:], val)
					continue
				} else {
					require.Nil(t, val, label)
					continue
				}
			}
			require.NoError(t, err, label)
			require.EqualValues(t, v[:], val, label)
		}
	}

	var v [8]byte
	binary.BigEndian.PutUint64(v[:], txCount)

	for keyNum := uint64(1); keyNum <= keysCount; keyNum++ {
		var k [8]byte
		label := fmt.Sprintf("txNum=%d, keyNum=%d\n", txCount, keyNum)
		binary.BigEndian.PutUint64(k[:], keyNum)

		storedV, found, err := dc.GetLatest(k[:], nil, tx)
		require.Truef(t, found, label)
		require.NoErrorf(t, err, label)
		require.EqualValues(t, v[:], storedV, label)
	}

	from, to := d.stepsRangeInDB(tx)
	require.Equal(t, 3, int(from))
	require.Equal(t, 4, int(to))

}

func TestScanStaticFilesD(t *testing.T) {

	ii := &Domain{History: &History{InvertedIndex: emptyTestInvertedIndex(1)},
		files: btree2.NewBTreeG[*filesItem](filesItemLess),
	}
	files := []string{
		"v1-test.0-1.kv",
		"v1-test.1-2.kv",
		"v1-test.0-4.kv",
		"v1-test.2-3.kv",
		"v1-test.3-4.kv",
		"v1-test.4-5.kv",
	}
	ii.scanStateFiles(files)
	var found []string
	ii.files.Walk(func(items []*filesItem) bool {
		for _, item := range items {
			found = append(found, fmt.Sprintf("%d-%d", item.startTxNum, item.endTxNum))
		}
		return true
	})
	require.Equal(t, 6, len(found))
}

func TestDomain_CollationBuildInMem(t *testing.T) {
	logEvery := time.NewTicker(30 * time.Second)
	defer logEvery.Stop()
	db, d := testDbAndDomain(t, log.New())
	ctx := context.Background()
	defer d.Close()

	tx, err := db.BeginRw(ctx)
	require.NoError(t, err)
	defer tx.Rollback()
	dc := d.MakeContext()
	defer dc.Close()
	maxTx := uint64(10000)
	d.aggregationStep = maxTx

	writer := dc.NewWriter()
	defer writer.close()

	var preval1, preval2, preval3 []byte

	l := []byte("asd9s9af0afa9sfh9afha")

	for i := 0; i < int(maxTx); i++ {
		v1 := []byte(fmt.Sprintf("value1.%d", i))
		v2 := []byte(fmt.Sprintf("value2.%d", i))
		s := []byte(fmt.Sprintf("longstorage2.%d", i))

		writer.SetTxNum(uint64(i))
		err = writer.PutWithPrev([]byte("key1"), nil, v1, preval1)
		require.NoError(t, err)

		err = writer.PutWithPrev([]byte("key2"), nil, v2, preval2)
		require.NoError(t, err)

		err = writer.PutWithPrev([]byte("key3"), l, s, preval3)
		require.NoError(t, err)

		preval1, preval2, preval3 = v1, v2, s
	}

	err = writer.Flush(ctx, tx)
	require.NoError(t, err)

	c, err := d.collate(ctx, 0, 0, maxTx, tx)

	require.NoError(t, err)
	require.True(t, strings.HasSuffix(c.valuesPath, "v1-base.0-1.kv"))
	require.Equal(t, 3, c.valuesCount)
	require.True(t, strings.HasSuffix(c.historyPath, "v1-base.0-1.v"))
	require.EqualValues(t, 3*maxTx, c.historyCount)
	require.Equal(t, 3, len(c.indexBitmaps))
	require.Len(t, c.indexBitmaps["key2"].ToArray(), int(maxTx))
	require.Len(t, c.indexBitmaps["key1"].ToArray(), int(maxTx))
	require.Len(t, c.indexBitmaps["key3"+string(l)].ToArray(), int(maxTx))

	sf, err := d.buildFiles(ctx, 0, c, background.NewProgressSet())
	require.NoError(t, err)
	defer sf.CleanupOnError()
	c.Close()

	g := sf.valuesDecomp.MakeGetter()
	g.Reset(0)
	var words []string
	for g.HasNext() {
		w, _ := g.Next(nil)
		words = append(words, string(w))
	}
	require.EqualValues(t, []string{"key1", string(preval1), "key2", string(preval2), "key3" + string(l), string(preval3)}, words)
	// Check index
	require.Equal(t, 3, int(sf.valuesBt.KeyCount()))
	for i := 0; i < len(words); i += 2 {
		c, _ := sf.valuesBt.SeekDeprecated([]byte(words[i]))
		require.Equal(t, words[i], string(c.Key()))
		require.Equal(t, words[i+1], string(c.Value()))
	}

	//require.Equal(t, 3, int(sf.valuesIdx.KeyCount()))
	//
	//r := recsplit.NewIndexReader(sf.valuesIdx)
	//defer r.Close()
	//for i := 0; i < len(words); i += 2 {
	//	offset := r.Lookup([]byte(words[i]))
	//	g.Reset(offset)
	//	w, _ := g.Next(nil)
	//	require.Equal(t, words[i], string(w))
	//	w, _ = g.Next(nil)
	//	require.Equal(t, words[i+1], string(w))
	//}
}

func TestDomainContext_IteratePrefixAgain(t *testing.T) {

	db, d := testDbAndDomain(t, log.New())

	tx, err := db.BeginRw(context.Background())
	require.NoError(t, err)
	defer tx.Rollback()

	d.historyLargeValues = true
	dc := d.MakeContext()
	defer dc.Close()
	writer := dc.NewWriter()
	defer writer.close()

	rnd := rand.New(rand.NewSource(time.Now().UnixNano()))
	key := make([]byte, 20)
	var loc []byte
	value := make([]byte, 32)
	first := []byte{0xab, 0xff}
	other := []byte{0xcc, 0xfe}
	copy(key[:], first)

	values := make(map[string][]byte)
	for i := 0; i < 30; i++ {
		rnd.Read(key[2:])
		if i == 15 {
			copy(key[:2], other)
		}
		loc = make([]byte, 32)
		rnd.Read(loc)
		rnd.Read(value)
		if i%5 == 0 {
			writer.SetTxNum(uint64(i))
		}

		if i == 0 || i == 15 {
			loc = nil
			copy(key[2:], make([]byte, 18))
		}

		values[hex.EncodeToString(common.Append(key, loc))] = common.Copy(value)
		err := writer.PutWithPrev(key, loc, value, nil)
		require.NoError(t, err)
	}
	err = writer.Flush(context.Background(), tx)
	require.NoError(t, err)
	dc.Close()

	dc = d.MakeContext()
	defer dc.Close()

	counter := 0
	err = dc.IteratePrefix(tx, other, func(kx, vx []byte) error {
		if !bytes.HasPrefix(kx, other) {
			return nil
		}
		fmt.Printf("%x \n", kx)
		counter++
		v, ok := values[hex.EncodeToString(kx)]
		require.True(t, ok)
		require.Equal(t, v, vx)
		return nil
	})
	require.NoError(t, err)
	err = dc.IteratePrefix(tx, first, func(kx, vx []byte) error {
		if !bytes.HasPrefix(kx, first) {
			return nil
		}
		fmt.Printf("%x \n", kx)
		counter++
		v, ok := values[hex.EncodeToString(kx)]
		require.True(t, ok)
		require.Equal(t, v, vx)
		return nil
	})
	require.NoError(t, err)
	require.EqualValues(t, len(values), counter)
}

func TestDomainContext_IteratePrefix(t *testing.T) {
	t.Parallel()

	db, d := testDbAndDomain(t, log.New())

	tx, err := db.BeginRw(context.Background())
	require.NoError(t, err)
	defer tx.Rollback()

	d.historyLargeValues = true
	dc := d.MakeContext()
	defer dc.Close()
	writer := dc.NewWriter()
	defer writer.close()

	rnd := rand.New(rand.NewSource(time.Now().UnixNano()))
	key := make([]byte, 20)
	value := make([]byte, 32)
	copy(key[:], []byte{0xff, 0xff})

	dctx := d.MakeContext()
	defer dctx.Close()

	values := make(map[string][]byte)
	for i := 0; i < 3000; i++ {
		rnd.Read(key[2:])
		rnd.Read(value)

		values[hex.EncodeToString(key)] = common.Copy(value)

		writer.SetTxNum(uint64(i))
		err := writer.PutWithPrev(key, nil, value, nil)
		require.NoError(t, err)
	}
	err = writer.Flush(context.Background(), tx)
	require.NoError(t, err)

	{
		counter := 0
		err = dctx.IteratePrefix(tx, key[:2], func(kx, vx []byte) error {
			if !bytes.HasPrefix(kx, key[:2]) {
				return nil
			}
			counter++
			v, ok := values[hex.EncodeToString(kx)]
			require.True(t, ok)
			require.Equal(t, v, vx)
			return nil
		})
		require.NoError(t, err)
		require.EqualValues(t, len(values), counter)
	}
	{
		counter := 0
		iter2, err := dctx.IteratePrefix2(tx, []byte("addr2"), []byte("addr3"), -1)
		require.NoError(t, err)
		for iter2.HasNext() {
			kx, vx, err := iter2.Next()
			require.NoError(t, err)
			if !bytes.HasPrefix(kx, key[:2]) {
				return
			}
			counter++
			v, ok := values[hex.EncodeToString(kx)]
			require.True(t, ok)
			require.Equal(t, v, vx)
		}
	}
}

func TestDomainContext_getFromFiles(t *testing.T) {
	db, d := testDbAndDomain(t, log.New())
	defer db.Close()
	defer d.Close()

	tx, err := db.BeginRw(context.Background())
	require.NoError(t, err)
	defer tx.Rollback()

	d.aggregationStep = 20

	keys, vals := generateInputData(t, 8, 16, 100)
	keys = keys[:20]

	var i int
	values := make(map[string][][]byte)

	dc := d.MakeContext()
	defer dc.Close()
	writer := dc.NewWriter()
	defer writer.close()

	var prev []byte
	for i = 0; i < len(vals); i++ {
		writer.SetTxNum(uint64(i))

		for j := 0; j < len(keys); j++ {
			buf := types.EncodeAccountBytesV3(uint64(i), uint256.NewInt(uint64(i*100_000)), nil, 0)

			err = writer.PutWithPrev(keys[j], nil, buf, prev)
			require.NoError(t, err)
			prev = buf

			if i > 0 && i+1%int(d.aggregationStep) == 0 {
				values[hex.EncodeToString(keys[j])] = append(values[hex.EncodeToString(keys[j])], buf)
			}
		}
	}
	err = writer.Flush(context.Background(), tx)
	require.NoError(t, err)
	defer dc.Close()

	ctx := context.Background()
	ps := background.NewProgressSet()
	for step := uint64(0); step < uint64(len(vals))/d.aggregationStep; step++ {
		dc := d.MakeContext()

		txFrom := step * d.aggregationStep
		txTo := (step + 1) * d.aggregationStep

		fmt.Printf("Step %d [%d,%d)\n", step, txFrom, txTo)

		collation, err := d.collate(ctx, step, txFrom, txTo, tx)
		require.NoError(t, err)

		sf, err := d.buildFiles(ctx, step, collation, ps)
		require.NoError(t, err)

		d.integrateFiles(sf, txFrom, txTo)
		collation.Close()

		logEvery := time.NewTicker(time.Second * 30)

		err = dc.Prune(ctx, tx, step, txFrom, txTo, math.MaxUint64, logEvery)
		require.NoError(t, err)

		ranges := dc.findMergeRange(txFrom, txTo)
		vl, il, hl, _ := dc.staticFilesInRange(ranges)

		dv, di, dh, err := dc.mergeFiles(ctx, vl, il, hl, ranges, ps)
		require.NoError(t, err)

		d.integrateMergedFiles(vl, il, hl, dv, di, dh)

		logEvery.Stop()

		dc.Close()
	}

	dc = d.MakeContext()
	defer dc.Close()

	for key, bufs := range values {
		var i int

		beforeTx := d.aggregationStep
		for i = 0; i < len(bufs); i++ {
			ks, _ := hex.DecodeString(key)
			val, err := dc.GetAsOf(ks, beforeTx, tx)
			require.NoError(t, err)
			require.EqualValuesf(t, bufs[i], val, "key %s, tx %d", key, beforeTx)
			beforeTx += d.aggregationStep
		}
	}
}

type upd struct {
	txNum uint64
	value []byte
}

func generateTestData(tb testing.TB, keySize1, keySize2, totalTx, keyTxsLimit, keyLimit uint64) map[string][]upd {
	tb.Helper()

	data := make(map[string][]upd)
	//seed := time.Now().Unix()
	seed := 31
	defer tb.Logf("generated data with seed %d, keys %d", seed, keyLimit)

	r := rand.New(rand.NewSource(0))
	if keyLimit == 1 {
		key1 := generateRandomKey(r, keySize1)
		data[key1] = generateUpdates(r, totalTx, keyTxsLimit)
		return data
	}

	for i := uint64(0); i < keyLimit/2; i++ {
		key1 := generateRandomKey(r, keySize1)
		data[key1] = generateUpdates(r, totalTx, keyTxsLimit)
		key2 := key1 + generateRandomKey(r, keySize2-keySize1)
		data[key2] = generateUpdates(r, totalTx, keyTxsLimit)
	}

	return data
}

func generateRandomKey(r *rand.Rand, size uint64) string {
	key := make([]byte, size)
	r.Read(key)
	return string(key)
}

func generateUpdates(r *rand.Rand, totalTx, keyTxsLimit uint64) []upd {
	updates := make([]upd, 0)
	usedTxNums := make(map[uint64]bool)

	for i := uint64(0); i < keyTxsLimit; i++ {
		txNum := generateRandomTxNum(r, totalTx, usedTxNums)
		value := make([]byte, 10)
		r.Read(value)

		updates = append(updates, upd{txNum: txNum, value: value})
		usedTxNums[txNum] = true
	}
	sort.Slice(updates, func(i, j int) bool { return updates[i].txNum < updates[j].txNum })

	return updates
}

func generateRandomTxNum(r *rand.Rand, maxTxNum uint64, usedTxNums map[uint64]bool) uint64 {
	txNum := uint64(r.Intn(int(maxTxNum)))
	for usedTxNums[txNum] {
		txNum = uint64(r.Intn(int(maxTxNum)))
	}

	return txNum
}

func TestDomain_GetAfterAggregation(t *testing.T) {

	db, d := testDbAndDomainOfStep(t, 25, log.New())

	tx, err := db.BeginRw(context.Background())
	require.NoError(t, err)
	defer tx.Rollback()

	d.historyLargeValues = false
	d.History.compression = CompressKeys | CompressVals
	d.compression = CompressKeys | CompressVals
	d.withLocalityIndex = true

	dc := d.MakeContext()
	defer d.Close()
	writer := dc.NewWriter()
	defer writer.close()

	keySize1 := uint64(length.Addr)
	keySize2 := uint64(length.Addr + length.Hash)
	totalTx := uint64(3000)
	keyTxsLimit := uint64(50)
	keyLimit := uint64(200)

	// put some kvs
	data := generateTestData(t, keySize1, keySize2, totalTx, keyTxsLimit, keyLimit)
	for key, updates := range data {
		p := []byte{}
		for i := 0; i < len(updates); i++ {
			writer.SetTxNum(updates[i].txNum)
			writer.PutWithPrev([]byte(key), nil, updates[i].value, p)
			p = common.Copy(updates[i].value)
		}
	}
	writer.SetTxNum(totalTx)

	err = writer.Flush(context.Background(), tx)
	require.NoError(t, err)

	// aggregate
	collateAndMerge(t, db, tx, d, totalTx)
	require.NoError(t, tx.Commit())

	tx, err = db.BeginRw(context.Background())
	require.NoError(t, err)
	defer tx.Rollback()
	dc.Close()

	dc = d.MakeContext()
	defer dc.Close()

	kc := 0
	for key, updates := range data {
		kc++
		for i := 1; i < len(updates); i++ {
			v, err := dc.GetAsOf([]byte(key), updates[i].txNum, tx)
			require.NoError(t, err)
			require.EqualValuesf(t, updates[i-1].value, v, "(%d/%d) key %x, tx %d", kc, len(data), []byte(key), updates[i-1].txNum)
		}
		if len(updates) == 0 {
			continue
		}
		v, ok, err := dc.GetLatest([]byte(key), nil, tx)
		require.NoError(t, err)
		require.EqualValuesf(t, updates[len(updates)-1].value, v, "key %x latest", []byte(key))
		require.True(t, ok)
	}
}

func TestDomain_PruneAfterAggregation(t *testing.T) {
	db, d := testDbAndDomainOfStep(t, 25, log.New())
	defer db.Close()
	defer d.Close()

	tx, err := db.BeginRw(context.Background())
	require.NoError(t, err)
	defer tx.Rollback()

	d.historyLargeValues = false
	d.History.compression = CompressKeys | CompressVals
	d.compression = CompressKeys | CompressVals
	d.withLocalityIndex = true

	dc := d.MakeContext()
	defer dc.Close()
	writer := dc.NewWriter()
	defer writer.close()

	keySize1 := uint64(length.Addr)
	keySize2 := uint64(length.Addr + length.Hash)
	totalTx := uint64(5000)
	keyTxsLimit := uint64(50)
	keyLimit := uint64(200)

	// put some kvs
	data := generateTestData(t, keySize1, keySize2, totalTx, keyTxsLimit, keyLimit)
	for key, updates := range data {
		p := []byte{}
		for i := 0; i < len(updates); i++ {
			writer.SetTxNum(updates[i].txNum)
			writer.PutWithPrev([]byte(key), nil, updates[i].value, p)
			p = common.Copy(updates[i].value)
		}
	}
	writer.SetTxNum(totalTx)

	err = writer.Flush(context.Background(), tx)
	require.NoError(t, err)

	// aggregate
	collateAndMerge(t, db, tx, d, totalTx) // expected to left 2 latest steps in db

	require.NoError(t, tx.Commit())

	tx, err = db.BeginRw(context.Background())
	require.NoError(t, err)
	defer tx.Rollback()
	dc.Close()

	dc = d.MakeContext()
	defer dc.Close()

	prefixes := 0
	err = dc.IteratePrefix(tx, nil, func(k, v []byte) error {
		upds, ok := data[string(k)]
		require.True(t, ok)
		prefixes++
		latest := upds[len(upds)-1]
		if string(latest.value) != string(v) {
			fmt.Printf("opanki %x\n", k)
			for li := len(upds) - 1; li >= 0; li-- {
				latest := upds[li]
				if bytes.Equal(latest.value, v) {
					t.Logf("returned value was set with nonce %d/%d (tx %d, step %d)", li+1, len(upds), latest.txNum, latest.txNum/d.aggregationStep)
				} else {
					continue
				}
				require.EqualValuesf(t, latest.value, v, "key %x txNum %d", k, latest.txNum)
				break
			}
		}

		require.EqualValuesf(t, latest.value, v, "key %x txnum %d", k, latest.txNum)
		return nil
	})
	require.NoError(t, err)
	require.EqualValues(t, len(data), prefixes, "seen less keys than expected")

	kc := 0
	for key, updates := range data {
		kc++
		for i := 1; i < len(updates); i++ {
			v, err := dc.GetAsOf([]byte(key), updates[i].txNum, tx)
			require.NoError(t, err)
			require.EqualValuesf(t, updates[i-1].value, v, "(%d/%d) key %x, tx %d", kc, len(data), []byte(key), updates[i-1].txNum)
		}
		if len(updates) == 0 {
			continue
		}
		v, ok, err := dc.GetLatest([]byte(key), nil, tx)
		require.NoError(t, err)
		require.EqualValuesf(t, updates[len(updates)-1].value, v, "key %x latest", []byte(key))
		require.True(t, ok)
	}
}

func TestPruneProgress(t *testing.T) {
	db, d := testDbAndDomainOfStep(t, 25, log.New())
	defer db.Close()
	defer d.Close()

	latestKey := []byte("682c02b93b63aeb260eccc33705d584ffb5f0d4c")
	latestStep := uint64(1337)

	t.Run("reset", func(t *testing.T) {
		tx, err := db.BeginRw(context.Background())
		require.NoError(t, err)
		defer tx.Rollback()
		err = SaveExecV3PruneProgress(tx, kv.TblAccountKeys, latestStep, latestKey)
		require.NoError(t, err)

		err = SaveExecV3PruneProgress(tx, kv.TblAccountKeys, 0, nil)
		require.NoError(t, err)

		step, key, err := GetExecV3PruneProgress(tx, kv.TblAccountKeys)
		require.NoError(t, err)
		require.Zero(t, step)
		require.Nil(t, key)
	})

	t.Run("someKey and reset", func(t *testing.T) {
		tx, err := db.BeginRw(context.Background())
		require.NoError(t, err)
		defer tx.Rollback()
		err = SaveExecV3PruneProgress(tx, kv.TblAccountKeys, latestStep, latestKey)
		require.NoError(t, err)

		step, key, err := GetExecV3PruneProgress(tx, kv.TblAccountKeys)
		require.NoError(t, err)
		require.EqualValues(t, latestStep, step)
		require.EqualValues(t, latestKey, key)

		err = SaveExecV3PruneProgress(tx, kv.TblAccountKeys, 0, nil)
		require.NoError(t, err)

		step, key, err = GetExecV3PruneProgress(tx, kv.TblAccountKeys)
		require.NoError(t, err)
		require.Zero(t, step)
		require.Nil(t, key)
	})
}

func TestDomain_PruneProgress(t *testing.T) {
	aggStep := uint64(1000)
	db, d := testDbAndDomainOfStep(t, aggStep, log.New())
	defer db.Close()
	defer d.Close()

	rwTx, err := db.BeginRw(context.Background())
	require.NoError(t, err)
	defer rwTx.Rollback()

	d.historyLargeValues = false
	d.History.compression = CompressKeys | CompressVals
	d.compression = CompressKeys | CompressVals
	d.withLocalityIndex = true

	dc := d.MakeContext()
	defer dc.Close()
	writer := dc.NewWriter()
	defer writer.close()

	keySize1 := uint64(length.Addr)
	keySize2 := uint64(length.Addr + length.Hash)
	totalTx := uint64(5000)
	keyTxsLimit := uint64(150)
	keyLimit := uint64(2000)

	// put some kvs
	data := generateTestData(t, keySize1, keySize2, totalTx, keyTxsLimit, keyLimit)
	for key, updates := range data {
		p := []byte{}
		for i := 0; i < len(updates); i++ {
			writer.SetTxNum(updates[i].txNum)
			err = writer.PutWithPrev([]byte(key), nil, updates[i].value, p)
			require.NoError(t, err)
			p = common.Copy(updates[i].value)
		}
	}
	writer.SetTxNum(totalTx)

	err = writer.Flush(context.Background(), rwTx)
	require.NoError(t, err)

	logEvery := time.NewTicker(30 * time.Second)
	defer logEvery.Stop()
	// aggregate
	for step := uint64(0); step < totalTx/aggStep; step++ {
		ctx := context.Background()
		txFrom, txTo := (step)*d.aggregationStep, (step+1)*d.aggregationStep

		c, err := d.collate(ctx, step, txFrom, txTo, rwTx)
		require.NoError(t, err)

		sf, err := d.buildFiles(ctx, step, c, background.NewProgressSet())
		require.NoError(t, err)
		d.integrateFiles(sf, txFrom, txTo)
	}
	require.NoError(t, rwTx.Commit())

	rwTx, err = db.BeginRw(context.Background())
	require.NoError(t, err)
	defer rwTx.Rollback()
	dc.Close()

	dc = d.MakeContext()
	defer dc.Close()

	ct, cancel := context.WithTimeout(context.Background(), time.Millisecond*1)
	err = dc.Prune(ct, rwTx, 0, 0, aggStep, math.MaxUint64, time.NewTicker(time.Second))
	require.ErrorIs(t, err, context.DeadlineExceeded)
	cancel()

	step, key, err := GetExecV3PruneProgress(rwTx, dc.d.keysTable)
	require.NoError(t, err)
	require.EqualValues(t, ^0, step)

	keysCursor, err := rwTx.RwCursorDupSort(dc.d.keysTable)
	require.NoError(t, err)

	k, istep, err := keysCursor.Seek(key)
	require.NoError(t, err)
	require.GreaterOrEqual(t, k, key)
	require.NotEqualValues(t, 0, ^binary.BigEndian.Uint64(istep))
	keysCursor.Close()

	var i int
	for step := uint64(0); ; step++ {
		// step changing should not affect pruning. Prune should finish step 0 first.
		i++
		ct, cancel := context.WithTimeout(context.Background(), time.Millisecond*2)
		err = dc.Prune(ct, rwTx, step, step*aggStep, (aggStep*step)+1, math.MaxUint64, time.NewTicker(time.Second))
		if err != nil {
			require.ErrorIs(t, err, context.DeadlineExceeded)
		} else {
			require.NoError(t, err)
		}
		cancel()

		step, key, err := GetExecV3PruneProgress(rwTx, dc.d.keysTable)
		require.NoError(t, err)
		if step == 0 && key == nil {

			fmt.Printf("pruned in %d iterations\n", i)

			keysCursor, err := rwTx.RwCursorDupSort(dc.d.keysTable)
			require.NoError(t, err)

			// check there are no keys with 0 step left
			for k, v, err := keysCursor.First(); k != nil && err == nil; k, v, err = keysCursor.Next() {
				require.NotEqualValues(t, 0, ^binary.BigEndian.Uint64(v))
			}

			keysCursor.Close()
			break
		}

	}
	fmt.Printf("exitiig after %d iterations\n", i)
}

func TestDomain_Unwind(t *testing.T) {
	db, d := testDbAndDomain(t, log.New())
	defer d.Close()
	defer db.Close()
	ctx := context.Background()

	d.aggregationStep = 16
	//maxTx := uint64(float64(d.aggregationStep) * 1.5)
	maxTx := d.aggregationStep - 2

	writeKeys := func(t *testing.T, d *Domain, db kv.RwDB, maxTx uint64) {
		t.Helper()
		dc := d.MakeContext()
		defer dc.Close()
		tx, err := db.BeginRw(ctx)
		require.NoError(t, err)
		defer tx.Rollback()
		writer := dc.NewWriter()
		defer writer.close()
		var preval1, preval2, preval3, preval4 []byte
		for i := uint64(0); i < maxTx; i++ {
			writer.SetTxNum(i)
			if i%3 == 0 && i > 0 { // once in 3 tx put key3 -> value3.i and skip other keys update
				if i%12 == 0 { // once in 12 tx delete key3 before update
					err = writer.DeleteWithPrev([]byte("key3"), nil, preval3)
					require.NoError(t, err)
					preval3 = nil

					continue
				}
				v3 := []byte(fmt.Sprintf("value3.%d", i))
				err = writer.PutWithPrev([]byte("key3"), nil, v3, preval3)
				require.NoError(t, err)
				preval3 = v3
				continue
			}

			v1 := []byte(fmt.Sprintf("value1.%d", i))
			v2 := []byte(fmt.Sprintf("value2.%d", i))
			nv3 := []byte(fmt.Sprintf("valuen3.%d", i))

			err = writer.PutWithPrev([]byte("key1"), nil, v1, preval1)
			require.NoError(t, err)
			err = writer.PutWithPrev([]byte("key2"), nil, v2, preval2)
			require.NoError(t, err)
			err = writer.PutWithPrev([]byte("k4"), nil, nv3, preval4)
			require.NoError(t, err)

			preval1, preval2, preval4 = v1, v2, nv3
		}
		err = writer.Flush(ctx, tx)
		require.NoError(t, err)
		err = tx.Commit()
		require.NoError(t, err)
	}

	unwindAndCompare := func(t *testing.T, d *Domain, db kv.RwDB, unwindTo uint64) {
		t.Helper()
		tx, err := db.BeginRw(ctx)
		require.NoError(t, err)
		defer tx.Rollback()

		dc := d.MakeContext()
		defer dc.Close()
		writer := dc.NewWriter()
		defer writer.close()

		err = dc.Unwind(ctx, tx, unwindTo/d.aggregationStep, unwindTo)
		require.NoError(t, err)
		dc.Close()
		tx.Commit()

		tmpDb, expected := testDbAndDomain(t, log.New())
		defer expected.Close()
		defer tmpDb.Close()
		writeKeys(t, expected, tmpDb, unwindTo)

		suf := fmt.Sprintf(";unwindTo=%d", unwindTo)
		t.Run("DomainRangeLatest"+suf, func(t *testing.T) {
			t.Helper()

			etx, err := tmpDb.BeginRo(ctx)
			defer etx.Rollback()
			require.NoError(t, err)

			utx, err := db.BeginRo(ctx)
			defer utx.Rollback()
			require.NoError(t, err)

			ectx := expected.MakeContext()
			defer ectx.Close()
			uc := d.MakeContext()
			defer uc.Close()
			et, err := ectx.DomainRangeLatest(etx, nil, nil, -1)
			require.NoError(t, err)

			ut, err := uc.DomainRangeLatest(utx, nil, nil, -1)
			require.NoError(t, err)

			compareIterators(t, et, ut)

		})
		t.Run("DomainRange"+suf, func(t *testing.T) {
			t.Helper()

			etx, err := tmpDb.BeginRo(ctx)
			defer etx.Rollback()
			require.NoError(t, err)

			utx, err := db.BeginRo(ctx)
			defer utx.Rollback()
			require.NoError(t, err)

			ectx := expected.MakeContext()
			defer ectx.Close()
			uc := d.MakeContext()
			defer uc.Close()
			et, err := ectx.DomainRange(etx, nil, nil, unwindTo, order.Asc, -1)
			require.NoError(t, err)

			ut, err := uc.DomainRange(etx, nil, nil, unwindTo, order.Asc, -1)
			require.NoError(t, err)

			compareIterators(t, et, ut)

		})
		t.Run("WalkAsOf"+suf, func(t *testing.T) {
			t.Helper()

			etx, err := tmpDb.BeginRo(ctx)
			defer etx.Rollback()
			require.NoError(t, err)

			utx, err := db.BeginRo(ctx)
			defer utx.Rollback()
			require.NoError(t, err)

			ectx := expected.MakeContext()
			defer ectx.Close()
			uc := d.MakeContext()
			defer uc.Close()

			et, err := ectx.hc.WalkAsOf(unwindTo-1, nil, nil, etx, -1)
			require.NoError(t, err)

			ut, err := uc.hc.WalkAsOf(unwindTo-1, nil, nil, utx, -1)
			require.NoError(t, err)

			compareIterators(t, et, ut)
		})
		t.Run("HistoryRange"+suf, func(t *testing.T) {
			t.Helper()

			tmpDb2, expected2 := testDbAndDomain(t, log.New())
			defer expected2.Close()
			defer tmpDb2.Close()
			writeKeys(t, expected2, tmpDb2, unwindTo)

			etx, err := tmpDb2.BeginRo(ctx)
			defer etx.Rollback()
			require.NoError(t, err)

			utx, err := db.BeginRo(ctx)
			defer utx.Rollback()
			require.NoError(t, err)

			ectx := expected2.MakeContext()
			defer ectx.Close()
			uc := d.MakeContext()
			defer uc.Close()

			et, err := ectx.hc.HistoryRange(int(unwindTo)-1, -1, order.Asc, -1, etx)
			require.NoError(t, err)

			ut, err := uc.hc.HistoryRange(int(unwindTo)-1, -1, order.Asc, -1, utx)
			require.NoError(t, err)

			compareIterators(t, et, ut)
		})
		t.Run("IteratePrefix2"+suf, func(t *testing.T) {
			t.Helper()

			etx, err := tmpDb.BeginRo(ctx)
			defer etx.Rollback()
			require.NoError(t, err)

			utx, err := db.BeginRo(ctx)
			defer utx.Rollback()
			require.NoError(t, err)

			ectx := expected.MakeContext()
			defer ectx.Close()
			uc := d.MakeContext()
			defer uc.Close()
			et, err := ectx.IteratePrefix2(etx, nil, nil, -1)
			require.NoError(t, err)

			ut, err := uc.IteratePrefix2(utx, nil, nil, -1)
			require.NoError(t, err)

			for {
				ek, ev, err1 := et.Next()
				uk, uv, err2 := ut.Next()
				require.EqualValues(t, err1, err2)
				require.EqualValues(t, ek, uk)
				require.EqualValues(t, ev, uv)
				if !et.HasNext() {
					require.False(t, ut.HasNext())
					break
				}
			}

		})
	}

	writeKeys(t, d, db, maxTx)
	unwindAndCompare(t, d, db, 14)
	unwindAndCompare(t, d, db, 11)
	unwindAndCompare(t, d, db, 10)
	unwindAndCompare(t, d, db, 8)
	unwindAndCompare(t, d, db, 6)
	unwindAndCompare(t, d, db, 5)
	unwindAndCompare(t, d, db, 2)
	unwindAndCompare(t, d, db, 0)

	return
}

func compareIterators(t *testing.T, et, ut iter.KV) {
	t.Helper()

	//i := 0
	//for {
	//	ek, ev, err1 := et.Next()
	//	fmt.Printf("ei=%d %s %s %v\n", i, ek, ev, err1)
	//	if !et.HasNext() {
	//		break
	//	}
	//}
	//
	//i = 0
	//for {
	//	uk, uv, err2 := ut.Next()
	//	fmt.Printf("ui=%d %s %s %v\n", i, string(uk), string(uv), err2)
	//	i++
	//	if !ut.HasNext() {
	//		break
	//	}
	//}
	for {
		ek, ev, err1 := et.Next()
		uk, uv, err2 := ut.Next()
		require.EqualValues(t, err1, err2)
		require.EqualValues(t, ek, uk)
		require.EqualValues(t, ev, uv)
		if !et.HasNext() {
			require.False(t, ut.HasNext(), "unwindedIterhas extra keys\n")
			break
		}
	}
}
