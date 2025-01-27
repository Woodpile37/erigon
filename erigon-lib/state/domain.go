/*
   Copyright 2022 Erigon contributors

   Licensed under the Apache License, Version 2.0 (the "License");
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
	"container/heap"
	"context"
	"encoding/binary"
	"fmt"
	"hash"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"sync/atomic"
	"time"

	bloomfilter "github.com/holiman/bloomfilter/v2"
	"github.com/ledgerwatch/erigon-lib/recsplit/eliasfano32"
	"github.com/ledgerwatch/log/v3"
	btree2 "github.com/tidwall/btree"
	"golang.org/x/sync/errgroup"

	"github.com/ledgerwatch/erigon-lib/common"
	"github.com/ledgerwatch/erigon-lib/common/background"
	"github.com/ledgerwatch/erigon-lib/common/dbg"
	"github.com/ledgerwatch/erigon-lib/common/dir"
	"github.com/ledgerwatch/erigon-lib/compress"
	"github.com/ledgerwatch/erigon-lib/etl"
	"github.com/ledgerwatch/erigon-lib/kv"
	"github.com/ledgerwatch/erigon-lib/kv/bitmapdb"
	"github.com/ledgerwatch/erigon-lib/kv/iter"
	"github.com/ledgerwatch/erigon-lib/kv/order"
	"github.com/ledgerwatch/erigon-lib/metrics"
	"github.com/ledgerwatch/erigon-lib/recsplit"
)

var (
	LatestStateReadWarm          = metrics.GetOrCreateSummary(`latest_state_read{type="warm",found="yes"}`)  //nolint
	LatestStateReadWarmNotFound  = metrics.GetOrCreateSummary(`latest_state_read{type="warm",found="no"}`)   //nolint
	LatestStateReadGrind         = metrics.GetOrCreateSummary(`latest_state_read{type="grind",found="yes"}`) //nolint
	LatestStateReadGrindNotFound = metrics.GetOrCreateSummary(`latest_state_read{type="grind",found="no"}`)  //nolint
	LatestStateReadCold          = metrics.GetOrCreateSummary(`latest_state_read{type="cold",found="yes"}`)  //nolint
	LatestStateReadColdNotFound  = metrics.GetOrCreateSummary(`latest_state_read{type="cold",found="no"}`)   //nolint

	mxRunningMerges        = metrics.GetOrCreateGauge("domain_running_merges")
	mxRunningFilesBuilding = metrics.GetOrCreateGauge("domain_running_files_building")
	mxCollateTook          = metrics.GetOrCreateHistogram("domain_collate_took")
	mxPruneTookDomain      = metrics.GetOrCreateHistogram(`domain_prune_took{type="domain"}`)
	mxPruneTookHistory     = metrics.GetOrCreateHistogram(`domain_prune_took{type="history"}`)
	mxPruneTookIndex       = metrics.GetOrCreateHistogram(`domain_prune_took{type="index"}`)
	mxPruneInProgress      = metrics.GetOrCreateGauge("domain_pruning_progress")
	mxCollationSize        = metrics.GetOrCreateGauge("domain_collation_size")
	mxCollationSizeHist    = metrics.GetOrCreateGauge("domain_collation_hist_size")
	mxPruneSizeDomain      = metrics.GetOrCreateCounter(`domain_prune_size{type="domain"}`)
	mxPruneSizeHistory     = metrics.GetOrCreateCounter(`domain_prune_size{type="history"}`)
	mxPruneSizeIndex       = metrics.GetOrCreateCounter(`domain_prune_size{type="index"}`)
	mxBuildTook            = metrics.GetOrCreateSummary("domain_build_files_took")
	mxStepTook             = metrics.GetOrCreateHistogram("domain_step_took")
	mxFlushTook            = metrics.GetOrCreateSummary("domain_flush_took")
	mxCommitmentRunning    = metrics.GetOrCreateGauge("domain_running_commitment")
	mxCommitmentTook       = metrics.GetOrCreateSummary("domain_commitment_took")
)

// StepsInColdFile - files of this size are completely frozen/immutable.
// files of smaller size are also immutable, but can be removed after merge to bigger files.
const StepsInColdFile = 64

var (
	asserts          = dbg.EnvBool("AGG_ASSERTS", false)
	traceFileLife    = dbg.EnvString("AGG_TRACE_FILE_LIFE", "")
	traceGetLatest   = dbg.EnvString("AGG_TRACE_GET_LATEST", "")
	traceGetAsOf     = dbg.EnvString("AGG_TRACE_GET_AS_OF", "")
	traceFileLevel   = dbg.EnvInt("AGG_TRACE_FILE_LEVEL", -1)
	tracePutWithPrev = dbg.EnvString("AGG_TRACE_PUT_WITH_PREV", "")
)

// filesItem corresponding to a pair of files (.dat and .idx)
type filesItem struct {
	decompressor         *compress.Decompressor
	index                *recsplit.Index
	bindex               *BtIndex
	bm                   *bitmapdb.FixedSizeBitmaps
	existence            *ExistenceFilter
	startTxNum, endTxNum uint64 //[startTxNum, endTxNum)

	// Frozen: file of size StepsInColdFile. Completely immutable.
	// Cold: file of size < StepsInColdFile. Immutable, but can be closed/removed after merge to bigger file.
	// Hot: Stored in DB. Providing Snapshot-Isolation by CopyOnWrite.
	frozen   bool         // immutable, don't need atomic
	refcount atomic.Int32 // only for `frozen=false`

	// file can be deleted in 2 cases: 1. when `refcount == 0 && canDelete == true` 2. on app startup when `file.isSubsetOfFrozenFile()`
	// other processes (which also reading files, may have same logic)
	canDelete atomic.Bool
}

type ExistenceFilter struct {
	filter             *bloomfilter.Filter
	empty              bool
	FileName, FilePath string
	f                  *os.File
	noFsync            bool // fsync is enabled by default, but tests can manually disable
}

func NewExistenceFilter(keysCount uint64, filePath string) (*ExistenceFilter, error) {

	m := bloomfilter.OptimalM(keysCount, 0.01)
	//TODO: make filters compatible by usinig same seed/keys
	_, fileName := filepath.Split(filePath)
	e := &ExistenceFilter{FilePath: filePath, FileName: fileName}
	if keysCount < 2 {
		e.empty = true
	} else {
		var err error
		e.filter, err = bloomfilter.New(m)
		if err != nil {
			return nil, fmt.Errorf("%w, %s", err, fileName)
		}
	}
	return e, nil
}

func (b *ExistenceFilter) AddHash(hash uint64) {
	if b.empty {
		return
	}
	b.filter.AddHash(hash)
}
func (b *ExistenceFilter) ContainsHash(v uint64) bool {
	if b.empty {
		return true
	}
	return b.filter.ContainsHash(v)
}
func (b *ExistenceFilter) Contains(v hash.Hash64) bool {
	if b.empty {
		return true
	}
	return b.filter.Contains(v)
}
func (b *ExistenceFilter) Build() error {
	if b.empty {
		cf, err := os.Create(b.FilePath)
		if err != nil {
			return err
		}
		defer cf.Close()
		return nil
	}

	log.Trace("[agg] write file", "file", b.FileName)
	tmpFilePath := b.FilePath + ".tmp"
	cf, err := os.Create(tmpFilePath)
	if err != nil {
		return err
	}
	defer cf.Close()

	if _, err := b.filter.WriteTo(cf); err != nil {
		return err
	}
	if err = b.fsync(cf); err != nil {
		return err
	}
	if err = cf.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpFilePath, b.FilePath); err != nil {
		return err
	}
	return nil
}

func (b *ExistenceFilter) DisableFsync() { b.noFsync = true }

// fsync - other processes/goroutines must see only "fully-complete" (valid) files. No partial-writes.
// To achieve it: write to .tmp file then `rename` when file is ready.
// Machine may power-off right after `rename` - it means `fsync` must be before `rename`
func (b *ExistenceFilter) fsync(f *os.File) error {
	if b.noFsync {
		return nil
	}
	if err := f.Sync(); err != nil {
		log.Warn("couldn't fsync", "err", err)
		return err
	}
	return nil
}

func OpenExistenceFilter(filePath string) (*ExistenceFilter, error) {
	_, fileName := filepath.Split(filePath)
	f := &ExistenceFilter{FilePath: filePath, FileName: fileName}
	if !dir.FileExist(filePath) {
		return nil, fmt.Errorf("file doesn't exists: %s", fileName)
	}
	{
		ff, err := os.Open(filePath)
		if err != nil {
			return nil, err
		}
		defer ff.Close()
		stat, err := ff.Stat()
		if err != nil {
			return nil, err
		}
		f.empty = stat.Size() == 0
	}

	if !f.empty {
		var err error
		f.filter, _, err = bloomfilter.ReadFile(filePath)
		if err != nil {
			return nil, fmt.Errorf("OpenExistenceFilter: %w, %s", err, fileName)
		}
	}
	return f, nil
}
func (b *ExistenceFilter) Close() {
	if b.f != nil {
		b.f.Close()
		b.f = nil
	}
}

func newFilesItem(startTxNum, endTxNum, stepSize uint64) *filesItem {
	startStep := startTxNum / stepSize
	endStep := endTxNum / stepSize
	frozen := endStep-startStep == StepsInColdFile
	return &filesItem{startTxNum: startTxNum, endTxNum: endTxNum, frozen: frozen}
}

func (i *filesItem) isSubsetOf(j *filesItem) bool {
	return (j.startTxNum <= i.startTxNum && i.endTxNum <= j.endTxNum) && (j.startTxNum != i.startTxNum || i.endTxNum != j.endTxNum)
}

func filesItemLess(i, j *filesItem) bool {
	if i.endTxNum == j.endTxNum {
		return i.startTxNum > j.startTxNum
	}
	return i.endTxNum < j.endTxNum
}
func (i *filesItem) closeFilesAndRemove() {
	if i.decompressor != nil {
		i.decompressor.Close()
		// paranoic-mode on: don't delete frozen files
		if !i.frozen {
			if err := os.Remove(i.decompressor.FilePath()); err != nil {
				log.Trace("remove after close", "err", err, "file", i.decompressor.FileName())
			}
			if err := os.Remove(i.decompressor.FilePath() + ".torrent"); err != nil {
				log.Trace("remove after close", "err", err, "file", i.decompressor.FileName()+".torrent")
			}
		}
		i.decompressor = nil
	}
	if i.index != nil {
		i.index.Close()
		// paranoic-mode on: don't delete frozen files
		if !i.frozen {
			if err := os.Remove(i.index.FilePath()); err != nil {
				log.Trace("remove after close", "err", err, "file", i.index.FileName())
			}
		}
		i.index = nil
	}
	if i.bindex != nil {
		i.bindex.Close()
		if err := os.Remove(i.bindex.FilePath()); err != nil {
			log.Trace("remove after close", "err", err, "file", i.bindex.FileName())
		}
		i.bindex = nil
	}
	if i.bm != nil {
		i.bm.Close()
		if err := os.Remove(i.bm.FilePath()); err != nil {
			log.Trace("remove after close", "err", err, "file", i.bm.FileName())
		}
		i.bm = nil
	}
	if i.existence != nil {
		i.existence.Close()
		if err := os.Remove(i.existence.FilePath); err != nil {
			log.Trace("remove after close", "err", err, "file", i.existence.FileName)
		}
		i.existence = nil
	}
}

type DomainStats struct {
	MergesCount          uint64
	LastCollationTook    time.Duration
	LastPruneTook        time.Duration
	LastPruneHistTook    time.Duration
	LastFileBuildingTook time.Duration
	LastCollationSize    uint64
	LastPruneSize        uint64

	FilesQueries *atomic.Uint64
	TotalQueries *atomic.Uint64
	EfSearchTime time.Duration
	DataSize     uint64
	IndexSize    uint64
	FilesCount   uint64
}

func (ds *DomainStats) Accumulate(other DomainStats) {
	if other.FilesQueries != nil {
		ds.FilesQueries.Add(other.FilesQueries.Load())
	}
	if other.TotalQueries != nil {
		ds.TotalQueries.Add(other.TotalQueries.Load())
	}
	ds.EfSearchTime += other.EfSearchTime
	ds.IndexSize += other.IndexSize
	ds.DataSize += other.DataSize
	ds.FilesCount += other.FilesCount
}

// Domain is a part of the state (examples are Accounts, Storage, Code)
// Domain should not have any go routines or locks
//
// Data-Existence in .kv vs .v files:
//  1. key doesn’t exists, then create: .kv - yes, .v - yes
//  2. acc exists, then update/delete:  .kv - yes, .v - yes
//  3. acc doesn’t exists, then delete: .kv - no,  .v - no
type Domain struct {
	*History
	files     *btree2.BTreeG[*filesItem] // thread-safe, but maybe need 1 RWLock for all trees in AggregatorV3
	indexList idxList

	// roFiles derivative from field `file`, but without garbage:
	//  - no files with `canDelete=true`
	//  - no overlaps
	//  - no un-indexed files (`power-off` may happen between .ef and .efi creation)
	//
	// MakeContext() using this field in zero-copy way
	roFiles   atomic.Pointer[[]ctxItem]
	keysTable string // key -> invertedStep , invertedStep = ^(txNum / aggregationStep), Needs to be table with DupSort
	valsTable string // key + invertedStep -> values
	stats     DomainStats

	garbageFiles []*filesItem // files that exist on disk, but ignored on opening folder - because they are garbage

	compression FileCompression
}

type domainCfg struct {
	hist     histCfg
	compress FileCompression
}

func NewDomain(cfg domainCfg, aggregationStep uint64, filenameBase, keysTable, valsTable, indexKeysTable, historyValsTable, indexTable string, logger log.Logger) (*Domain, error) {
	if cfg.hist.iiCfg.dirs.SnapDomain == "" {
		panic("empty `dirs` varialbe")
	}
	d := &Domain{
		keysTable:   keysTable,
		valsTable:   valsTable,
		compression: cfg.compress,
		files:       btree2.NewBTreeGOptions[*filesItem](filesItemLess, btree2.Options{Degree: 128, NoLocks: false}),
		stats:       DomainStats{FilesQueries: &atomic.Uint64{}, TotalQueries: &atomic.Uint64{}},

		indexList: withBTree,
	}
	d.roFiles.Store(&[]ctxItem{})

	var err error
	if d.History, err = NewHistory(cfg.hist, aggregationStep, filenameBase, indexKeysTable, indexTable, historyValsTable, nil, logger); err != nil {
		return nil, err
	}
	if d.withExistenceIndex {
		d.indexList |= withExistence
	}

	return d, nil
}
func (d *Domain) kvFilePath(fromStep, toStep uint64) string {
	return filepath.Join(d.dirs.SnapDomain, fmt.Sprintf("v1-%s.%d-%d.kv", d.filenameBase, fromStep, toStep))
}
func (d *Domain) kvAccessorFilePath(fromStep, toStep uint64) string {
	return filepath.Join(d.dirs.SnapDomain, fmt.Sprintf("v1-%s.%d-%d.kvi", d.filenameBase, fromStep, toStep))
}
func (d *Domain) kvExistenceIdxFilePath(fromStep, toStep uint64) string {
	return filepath.Join(d.dirs.SnapDomain, fmt.Sprintf("v1-%s.%d-%d.kvei", d.filenameBase, fromStep, toStep))
}
func (d *Domain) kvBtFilePath(fromStep, toStep uint64) string {
	return filepath.Join(d.dirs.SnapDomain, fmt.Sprintf("v1-%s.%d-%d.bt", d.filenameBase, fromStep, toStep))
}

// LastStepInDB - return the latest available step in db (at-least 1 value in such step)
func (d *Domain) LastStepInDB(tx kv.Tx) (lstInDb uint64) {
	lstIdx, _ := kv.LastKey(tx, d.History.indexKeysTable)
	if len(lstIdx) == 0 {
		return 0
	}
	return binary.BigEndian.Uint64(lstIdx) / d.aggregationStep
}
func (d *Domain) FirstStepInDB(tx kv.Tx) (lstInDb uint64) {
	lstIdx, _ := kv.FirstKey(tx, d.History.indexKeysTable)
	if len(lstIdx) == 0 {
		return 0
	}
	return binary.BigEndian.Uint64(lstIdx) / d.aggregationStep
}

func (dc *DomainContext) NewWriter() *domainBufferedWriter { return dc.newWriter(dc.d.dirs.Tmp, false) }

// OpenList - main method to open list of files.
// It's ok if some files was open earlier.
// If some file already open: noop.
// If some file already open but not in provided list: close and remove from `files` field.
func (d *Domain) OpenList(idxFiles, histFiles, domainFiles []string, readonly bool) error {
	if err := d.History.OpenList(idxFiles, histFiles, readonly); err != nil {
		return err
	}
	if err := d.openList(domainFiles, readonly); err != nil {
		return fmt.Errorf("Domain(%s).OpenFolder: %w", d.filenameBase, err)
	}
	return nil
}

func (d *Domain) openList(names []string, readonly bool) error {
	d.closeWhatNotInList(names)
	d.garbageFiles = d.scanStateFiles(names)
	if err := d.openFiles(); err != nil {
		return fmt.Errorf("Domain.OpenList: %s, %w", d.filenameBase, err)
	}
	d.protectFromHistoryFilesAheadOfDomainFiles(readonly)
	d.reCalcRoFiles()
	return nil
}

// protectFromHistoryFilesAheadOfDomainFiles - in some corner-cases app may see more .ef/.v files than .kv:
//   - `kill -9` in the middle of `buildFiles()`, then `rm -f db` (restore from backup)
//   - `kill -9` in the middle of `buildFiles()`, then `stage_exec --reset` (drop progress - as a hot-fix)
func (d *Domain) protectFromHistoryFilesAheadOfDomainFiles(readonly bool) {
	d.removeFilesAfterStep(d.endTxNumMinimax()/d.aggregationStep, readonly)
}

func (d *Domain) OpenFolder(readonly bool) error {
	idx, histFiles, domainFiles, err := d.fileNamesOnDisk()
	if err != nil {
		return fmt.Errorf("Domain(%s).OpenFolder: %w", d.filenameBase, err)
	}
	if err := d.OpenList(idx, histFiles, domainFiles, readonly); err != nil {
		return err
	}
	return nil
}

func (d *Domain) GetAndResetStats() DomainStats {
	r := d.stats
	r.DataSize, r.IndexSize, r.FilesCount = d.collectFilesStats()

	d.stats = DomainStats{FilesQueries: &atomic.Uint64{}, TotalQueries: &atomic.Uint64{}}
	return r
}

func (d *Domain) removeFilesAfterStep(lowerBound uint64, readonly bool) {
	var toDelete []*filesItem
	d.files.Scan(func(item *filesItem) bool {
		if item.startTxNum/d.aggregationStep >= lowerBound {
			toDelete = append(toDelete, item)
		}
		return true
	})
	for _, item := range toDelete {
		d.files.Delete(item)
		if !readonly {
			log.Debug(fmt.Sprintf("[snapshots] delete %s, because step %d has not enough files (was not complete). stack: %s", item.decompressor.FileName(), lowerBound, dbg.Stack()))
			item.closeFilesAndRemove()
		} else {
			log.Debug(fmt.Sprintf("[snapshots] closing %s, because step %d has not enough files (was not complete). stack: %s", item.decompressor.FileName(), lowerBound, dbg.Stack()))

		}
	}

	toDelete = toDelete[:0]
	d.History.files.Scan(func(item *filesItem) bool {
		if item.startTxNum/d.aggregationStep >= lowerBound {
			toDelete = append(toDelete, item)
		}
		return true
	})
	for _, item := range toDelete {
		d.History.files.Delete(item)
		if !readonly {
			log.Debug(fmt.Sprintf("[snapshots] delete %s, because step %d has not enough files (was not complete)", item.decompressor.FileName(), lowerBound))
			item.closeFilesAndRemove()
		} else {
			log.Debug(fmt.Sprintf("[snapshots] closing %s, because step %d has not enough files (was not complete)", item.decompressor.FileName(), lowerBound))
		}
	}

	toDelete = toDelete[:0]
	d.History.InvertedIndex.files.Scan(func(item *filesItem) bool {
		if item.startTxNum/d.aggregationStep >= lowerBound {
			toDelete = append(toDelete, item)
		}
		return true
	})
	for _, item := range toDelete {
		d.History.InvertedIndex.files.Delete(item)
		if !readonly {
			log.Debug(fmt.Sprintf("[snapshots] delete %s, because step %d has not enough files (was not complete)", item.decompressor.FileName(), lowerBound))
			item.closeFilesAndRemove()
		} else {
			log.Debug(fmt.Sprintf("[snapshots] closing %s, because step %d has not enough files (was not complete)", item.decompressor.FileName(), lowerBound))
		}
	}
}

func (d *Domain) scanStateFiles(fileNames []string) (garbageFiles []*filesItem) {
	re := regexp.MustCompile("^v([0-9]+)-" + d.filenameBase + ".([0-9]+)-([0-9]+).kv$")
	var err error

	for _, name := range fileNames {
		subs := re.FindStringSubmatch(name)
		if len(subs) != 4 {
			if len(subs) != 0 {
				d.logger.Warn("File ignored by domain scan, more than 4 submatches", "name", name, "submatches", len(subs))
			}
			continue
		}
		var startStep, endStep uint64
		if startStep, err = strconv.ParseUint(subs[2], 10, 64); err != nil {
			d.logger.Warn("File ignored by domain scan, parsing startTxNum", "error", err, "name", name)
			continue
		}
		if endStep, err = strconv.ParseUint(subs[3], 10, 64); err != nil {
			d.logger.Warn("File ignored by domain scan, parsing endTxNum", "error", err, "name", name)
			continue
		}
		if startStep > endStep {
			d.logger.Warn("File ignored by domain scan, startTxNum > endTxNum", "name", name)
			continue
		}

		// Semantic: [startTxNum, endTxNum)
		// Example:
		//   stepSize = 4
		//   0-1.kv: [0, 8)
		//   0-2.kv: [0, 16)
		//   1-2.kv: [8, 16)
		startTxNum, endTxNum := startStep*d.aggregationStep, endStep*d.aggregationStep

		var newFile = newFilesItem(startTxNum, endTxNum, d.aggregationStep)
		newFile.frozen = false

		//for _, ext := range d.integrityFileExtensions {
		//	requiredFile := fmt.Sprintf("%s.%d-%d.%s", d.filenameBase, startStep, endStep, ext)
		//	if !dir.FileExist(filepath.Join(d.dir, requiredFile)) {
		//		d.logger.Debug(fmt.Sprintf("[snapshots] skip %s because %s doesn't exists", name, requiredFile))
		//		garbageFiles = append(garbageFiles, newFile)
		//		continue Loop
		//	}
		//}

		if _, has := d.files.Get(newFile); has {
			continue
		}

		addNewFile := true
		var subSets []*filesItem
		d.files.Walk(func(items []*filesItem) bool {
			for _, item := range items {
				if item.isSubsetOf(newFile) {
					subSets = append(subSets, item)
					continue
				}

				if newFile.isSubsetOf(item) {
					if item.frozen {
						addNewFile = false
						garbageFiles = append(garbageFiles, newFile)
					}
					continue
				}
			}
			return true
		})
		if addNewFile {
			d.files.Set(newFile)
		}
	}
	return garbageFiles
}

func (d *Domain) openFiles() (err error) {
	invalidFileItems := make([]*filesItem, 0)
	d.files.Walk(func(items []*filesItem) bool {
		for _, item := range items {
			fromStep, toStep := item.startTxNum/d.aggregationStep, item.endTxNum/d.aggregationStep
			if item.decompressor == nil {
				fPath := d.kvFilePath(fromStep, toStep)
				if !dir.FileExist(fPath) {
					_, fName := filepath.Split(fPath)
					d.logger.Debug("[agg] Domain.openFiles: file does not exists", "f", fName)
					invalidFileItems = append(invalidFileItems, item)
					continue
				}

				if item.decompressor, err = compress.NewDecompressor(fPath); err != nil {
					_, fName := filepath.Split(fPath)
					d.logger.Warn("[agg] Domain.openFiles", "err", err, "f", fName)
					invalidFileItems = append(invalidFileItems, item)
					// don't interrupt on error. other files may be good. but skip indices open.
					continue
				}
			}

			if item.index == nil && !UseBpsTree {
				fPath := d.kvAccessorFilePath(fromStep, toStep)
				if dir.FileExist(fPath) {
					if item.index, err = recsplit.OpenIndex(fPath); err != nil {
						_, fName := filepath.Split(fPath)
						d.logger.Warn("[agg] Domain.openFiles", "err", err, "f", fName)
						// don't interrupt on error. other files may be good
					}
				}
			}
			if item.bindex == nil {
				fPath := d.kvBtFilePath(fromStep, toStep)
				if dir.FileExist(fPath) {
					if item.bindex, err = OpenBtreeIndexWithDecompressor(fPath, DefaultBtreeM, item.decompressor, d.compression); err != nil {
						_, fName := filepath.Split(fPath)
						d.logger.Warn("[agg] Domain.openFiles", "err", err, "f", fName)
						// don't interrupt on error. other files may be good
					}
				}
			}
			if item.existence == nil {
				fPath := d.kvExistenceIdxFilePath(fromStep, toStep)
				if dir.FileExist(fPath) {
					if item.existence, err = OpenExistenceFilter(fPath); err != nil {
						_, fName := filepath.Split(fPath)
						d.logger.Warn("[agg] Domain.openFiles", "err", err, "f", fName)
						// don't interrupt on error. other files may be good
					}
				}
			}
		}
		return true
	})
	if err != nil {
		return err
	}
	for _, item := range invalidFileItems {
		d.files.Delete(item)
	}

	d.reCalcRoFiles()
	return nil
}

func (d *Domain) closeWhatNotInList(fNames []string) {
	var toDelete []*filesItem
	d.files.Walk(func(items []*filesItem) bool {
	Loop1:
		for _, item := range items {
			for _, protectName := range fNames {
				if item.decompressor != nil && item.decompressor.FileName() == protectName {
					continue Loop1
				}
			}
			toDelete = append(toDelete, item)
		}
		return true
	})
	for _, item := range toDelete {
		if item.decompressor != nil {
			item.decompressor.Close()
			item.decompressor = nil
		}
		if item.index != nil {
			item.index.Close()
			item.index = nil
		}
		if item.bindex != nil {
			item.bindex.Close()
			item.bindex = nil
		}
		if item.existence != nil {
			item.existence.Close()
			item.existence = nil
		}
		d.files.Delete(item)
	}
}

func (d *Domain) reCalcRoFiles() {
	roFiles := ctxFiles(d.files, d.indexList, false)
	d.roFiles.Store(&roFiles)
}

func (d *Domain) Close() {
	d.History.Close()
	d.closeWhatNotInList([]string{})
	d.reCalcRoFiles()
}

func (w *domainBufferedWriter) PutWithPrev(key1, key2, val, preval []byte) error {
	// This call to update needs to happen before d.tx.Put() later, because otherwise the content of `preval`` slice is invalidated
	if tracePutWithPrev != "" && tracePutWithPrev == w.h.ii.filenameBase {
		fmt.Printf("PutWithPrev(%s, tx %d, key[%x][%x] value[%x] preval[%x])\n", w.h.ii.filenameBase, w.h.ii.txNum, key1, key2, val, preval)
	}
	if err := w.h.AddPrevValue(key1, key2, preval); err != nil {
		return err
	}
	return w.addValue(key1, key2, val)
}

func (w *domainBufferedWriter) DeleteWithPrev(key1, key2, prev []byte) (err error) {
	// This call to update needs to happen before d.tx.Delete() later, because otherwise the content of `original`` slice is invalidated
	if tracePutWithPrev != "" && tracePutWithPrev == w.h.ii.filenameBase {
		fmt.Printf("DeleteWithPrev(%s, tx %d, key[%x][%x] preval[%x])\n", w.h.ii.filenameBase, w.h.ii.txNum, key1, key2, prev)
	}
	if err := w.h.AddPrevValue(key1, key2, prev); err != nil {
		return err
	}
	return w.addValue(key1, key2, nil)
}

func (w *domainBufferedWriter) SetTxNum(v uint64) {
	w.setTxNumOnce = true
	w.h.SetTxNum(v)
	binary.BigEndian.PutUint64(w.stepBytes[:], ^(v / w.h.ii.aggregationStep))
}

func (dc *DomainContext) newWriter(tmpdir string, discard bool) *domainBufferedWriter {
	w := &domainBufferedWriter{
		discard:   discard,
		aux:       make([]byte, 0, 128),
		keysTable: dc.d.keysTable,
		valsTable: dc.d.valsTable,
		keys:      etl.NewCollector(dc.d.keysTable, tmpdir, etl.NewSortableBuffer(WALCollectorRAM), dc.d.logger),
		values:    etl.NewCollector(dc.d.valsTable, tmpdir, etl.NewSortableBuffer(WALCollectorRAM), dc.d.logger),

		h: dc.hc.newWriter(tmpdir, discard),
	}
	w.keys.LogLvl(log.LvlTrace)
	w.values.LogLvl(log.LvlTrace)
	return w
}

type domainBufferedWriter struct {
	keys, values *etl.Collector

	setTxNumOnce bool
	discard      bool

	keysTable, valsTable string

	stepBytes [8]byte // current inverted step representation
	aux       []byte

	h *historyBufferedWriter
}

func (w *domainBufferedWriter) close() {
	if w == nil { // allow dobule-close
		return
	}
	w.h.close()
	if w.keys != nil {
		w.keys.Close()
	}
	if w.values != nil {
		w.values.Close()
	}
}

// nolint
func loadSkipFunc() etl.LoadFunc {
	var preKey, preVal []byte
	return func(k, v []byte, table etl.CurrentTableReader, next etl.LoadNextFunc) error {
		if bytes.Equal(k, preKey) {
			preVal = v
			return nil
		}
		if err := next(nil, preKey, preVal); err != nil {
			return err
		}
		if err := next(k, k, v); err != nil {
			return err
		}
		preKey, preVal = k, v
		return nil
	}
}
func (w *domainBufferedWriter) Flush(ctx context.Context, tx kv.RwTx) error {
	if w.discard {
		return nil
	}
	if err := w.h.Flush(ctx, tx); err != nil {
		return err
	}

	if err := w.keys.Load(tx, w.keysTable, loadFunc, etl.TransformArgs{Quit: ctx.Done()}); err != nil {
		return err
	}
	if err := w.values.Load(tx, w.valsTable, loadFunc, etl.TransformArgs{Quit: ctx.Done()}); err != nil {
		return err
	}
	return nil
}

func (w *domainBufferedWriter) addValue(key1, key2, value []byte) error {
	if w.discard {
		return nil
	}
	if !w.setTxNumOnce {
		panic("you forgot to call SetTxNum")
	}

	kl := len(key1) + len(key2)
	w.aux = append(append(append(w.aux[:0], key1...), key2...), w.stepBytes[:]...)
	fullkey := w.aux[:kl+8]
	if asserts && (w.h.ii.txNum/w.h.ii.aggregationStep) != ^binary.BigEndian.Uint64(w.stepBytes[:]) {
		panic(fmt.Sprintf("assert: %d != %d", w.h.ii.txNum/w.h.ii.aggregationStep, ^binary.BigEndian.Uint64(w.stepBytes[:])))
	}

	//defer func() {
	//	fmt.Printf("addValue @%w %x->%x buffered %t largeVals %t file %s\n", w.dc.hc.ic.txNum, fullkey, value, w.buffered, w.largeValues, w.dc.w.filenameBase)
	//}()

	if err := w.keys.Collect(fullkey[:kl], fullkey[kl:]); err != nil {
		return err
	}
	if err := w.values.Collect(fullkey, value); err != nil {
		return err
	}
	return nil
}

type CursorType uint8

const (
	FILE_CURSOR CursorType = iota
	DB_CURSOR
	RAM_CURSOR
)

// CursorItem is the item in the priority queue used to do merge interation
// over storage of a given account
type CursorItem struct {
	c            kv.CursorDupSort
	iter         btree2.MapIter[string, []byte]
	dg           ArchiveGetter
	dg2          ArchiveGetter
	btCursor     *Cursor
	key          []byte
	val          []byte
	endTxNum     uint64
	latestOffset uint64     // offset of the latest value in the file
	t            CursorType // Whether this item represents state file or DB record, or tree
	reverse      bool
}

type CursorHeap []*CursorItem

func (ch CursorHeap) Len() int {
	return len(ch)
}

func (ch CursorHeap) Less(i, j int) bool {
	cmp := bytes.Compare(ch[i].key, ch[j].key)
	if cmp == 0 {
		// when keys match, the items with later blocks are preferred
		if ch[i].reverse {
			return ch[i].endTxNum > ch[j].endTxNum
		}
		return ch[i].endTxNum < ch[j].endTxNum
	}
	return cmp < 0
}

func (ch *CursorHeap) Swap(i, j int) {
	(*ch)[i], (*ch)[j] = (*ch)[j], (*ch)[i]
}

func (ch *CursorHeap) Push(x interface{}) {
	*ch = append(*ch, x.(*CursorItem))
}

func (ch *CursorHeap) Pop() interface{} {
	old := *ch
	n := len(old)
	x := old[n-1]
	old[n-1] = nil
	*ch = old[0 : n-1]
	return x
}

// filesItem corresponding to a pair of files (.dat and .idx)
type ctxItem struct {
	getter     *compress.Getter
	reader     *recsplit.IndexReader
	startTxNum uint64
	endTxNum   uint64

	i   int
	src *filesItem
}

func (i *ctxItem) isSubSetOf(j *ctxItem) bool { return i.src.isSubsetOf(j.src) } //nolint
func (i *ctxItem) isSubsetOf(j *ctxItem) bool { return i.src.isSubsetOf(j.src) } //nolint

type ctxLocalityIdx struct {
	reader          *recsplit.IndexReader
	file            *ctxItem
	aggregationStep uint64
}

// DomainContext allows accesing the same domain from multiple go-routines
type DomainContext struct {
	hc         *HistoryContext
	d          *Domain
	files      []ctxItem
	getters    []ArchiveGetter
	readers    []*BtIndex
	idxReaders []*recsplit.IndexReader

	keyBuf    [60]byte // 52b key and 8b for inverted step
	valKeyBuf [60]byte // 52b key and 8b for inverted step

	keysC kv.CursorDupSort
	valsC kv.Cursor
}

// getFromFile returns exact match for the given key from the given file
func (dc *DomainContext) getFromFileOld(i int, filekey []byte) ([]byte, bool, error) {
	g := dc.statelessGetter(i)
	if UseBtree || UseBpsTree {
		if dc.d.withExistenceIndex && dc.files[i].src.existence != nil {
			hi, _ := dc.hc.ic.hashKey(filekey)
			if !dc.files[i].src.existence.ContainsHash(hi) {
				return nil, false, nil
			}
		}

		_, v, ok, err := dc.statelessBtree(i).Get(filekey, g)
		if err != nil || !ok {
			return nil, false, err
		}
		//fmt.Printf("getLatestFromBtreeColdFiles key %x shard %d %x\n", filekey, exactColdShard, v)
		return v, true, nil
	}

	reader := dc.statelessIdxReader(i)
	if reader.Empty() {
		return nil, false, nil
	}
	offset := reader.Lookup(filekey)
	g.Reset(offset)

	k, _ := g.Next(nil)
	if !bytes.Equal(filekey, k) {
		return nil, false, nil
	}
	v, _ := g.Next(nil)
	return v, true, nil
}

func (dc *DomainContext) getFromFile(i int, filekey []byte) ([]byte, bool, error) {
	g := dc.statelessGetter(i)
	if !(UseBtree || UseBpsTree) {
		reader := dc.statelessIdxReader(i)
		if reader.Empty() {
			return nil, false, nil
		}
		offset := reader.Lookup(filekey)
		g.Reset(offset)

		k, _ := g.Next(nil)
		if !bytes.Equal(filekey, k) {
			return nil, false, nil
		}
		v, _ := g.Next(nil)
		return v, true, nil
	}

	_, v, ok, err := dc.statelessBtree(i).Get(filekey, g)
	if err != nil || !ok {
		return nil, false, err
	}
	//fmt.Printf("getLatestFromBtreeColdFiles key %x shard %d %x\n", filekey, exactColdShard, v)
	return v, true, nil
}
func (dc *DomainContext) DebugKVFilesWithKey(k []byte) (res []string, err error) {
	for i := len(dc.files) - 1; i >= 0; i-- {
		_, ok, err := dc.getFromFile(i, k)
		if err != nil {
			return res, err
		}
		if ok {
			res = append(res, dc.files[i].src.decompressor.FileName())
		}
	}
	return res, nil
}
func (dc *DomainContext) DebugEFKey(k []byte) error {
	dc.hc.ic.ii.files.Walk(func(items []*filesItem) bool {
		for _, item := range items {
			if item.decompressor == nil {
				continue
			}
			idx := item.index
			if idx == nil {
				fPath := dc.d.efAccessorFilePath(item.startTxNum/dc.d.aggregationStep, item.endTxNum/dc.d.aggregationStep)
				if dir.FileExist(fPath) {
					var err error
					idx, err = recsplit.OpenIndex(fPath)
					if err != nil {
						_, fName := filepath.Split(fPath)
						dc.d.logger.Warn("[agg] InvertedIndex.openFiles", "err", err, "f", fName)
						continue
					}
					defer idx.Close()
				} else {
					continue
				}
			}

			offset := idx.GetReaderFromPool().Lookup(k)
			g := item.decompressor.MakeGetter()
			g.Reset(offset)
			key, _ := g.NextUncompressed()
			if !bytes.Equal(k, key) {
				continue
			}
			eliasVal, _ := g.NextUncompressed()
			ef, _ := eliasfano32.ReadEliasFano(eliasVal)

			last2 := uint64(0)
			if ef.Count() > 2 {
				last2 = ef.Get(ef.Count() - 2)
			}
			log.Warn(fmt.Sprintf("[dbg] see1: %s, min=%d,max=%d, before_max=%d, all: %d\n", item.decompressor.FileName(), ef.Min(), ef.Max(), last2, iter.ToArrU64Must(ef.Iterator())))
		}
		return true
	})
	return nil
}

func (d *Domain) collectFilesStats() (datsz, idxsz, files uint64) {
	d.History.files.Walk(func(items []*filesItem) bool {
		for _, item := range items {
			if item.index == nil {
				return false
			}
			datsz += uint64(item.decompressor.Size())
			idxsz += uint64(item.index.Size())
			idxsz += uint64(item.bindex.Size())
			files += 3
		}
		return true
	})

	d.files.Walk(func(items []*filesItem) bool {
		for _, item := range items {
			if item.index == nil {
				return false
			}
			datsz += uint64(item.decompressor.Size())
			idxsz += uint64(item.index.Size())
			idxsz += uint64(item.bindex.Size())
			files += 3
		}
		return true
	})

	fcnt, fsz, isz := d.History.InvertedIndex.collectFilesStat()
	datsz += fsz
	files += fcnt
	idxsz += isz
	return
}

func (d *Domain) MakeContext() *DomainContext {
	files := *d.roFiles.Load()
	for i := 0; i < len(files); i++ {
		if !files[i].src.frozen {
			files[i].src.refcount.Add(1)
		}
	}
	return &DomainContext{
		d:     d,
		hc:    d.History.MakeContext(),
		files: files,
	}
}

// Collation is the set of compressors created after aggregation
type Collation struct {
	HistoryCollation
	valuesComp  *compress.Compressor
	valuesPath  string
	valuesCount int
}

func (c Collation) Close() {
	if c.valuesComp != nil {
		c.valuesComp.Close()
	}
	c.HistoryCollation.Close()
}

// collate gathers domain changes over the specified step, using read-only transaction,
// and returns compressors, elias fano, and bitmaps
// [txFrom; txTo)
func (d *Domain) collate(ctx context.Context, step, txFrom, txTo uint64, roTx kv.Tx) (coll Collation, err error) {
	{ //assert
		if txFrom%d.aggregationStep != 0 {
			panic(fmt.Errorf("assert: unexpected txFrom=%d", txFrom))
		}
		if txTo%d.aggregationStep != 0 {
			panic(fmt.Errorf("assert: unexpected txTo=%d", txTo))
		}
	}

	started := time.Now()
	defer func() {
		d.stats.LastCollationTook = time.Since(started)
		mxCollateTook.ObserveDuration(started)
	}()

	coll.HistoryCollation, err = d.History.collate(ctx, step, txFrom, txTo, roTx)
	if err != nil {
		return Collation{}, err
	}

	closeCollation := true
	defer func() {
		if closeCollation {
			coll.Close()
		}
	}()

	coll.valuesPath = d.kvFilePath(step, step+1)
	if coll.valuesComp, err = compress.NewCompressor(ctx, "collate values", coll.valuesPath, d.dirs.Tmp, compress.MinPatternScore, d.compressWorkers, log.LvlTrace, d.logger); err != nil {
		return Collation{}, fmt.Errorf("create %s values compressor: %w", d.filenameBase, err)
	}
	comp := NewArchiveWriter(coll.valuesComp, d.compression)

	keysCursor, err := roTx.CursorDupSort(d.keysTable)
	if err != nil {
		return Collation{}, fmt.Errorf("create %s keys cursor: %w", d.filenameBase, err)
	}
	defer keysCursor.Close()

	var (
		stepBytes = make([]byte, 8)
		keySuffix = make([]byte, 256+8)
		v         []byte

		valsDup kv.CursorDupSort
	)
	binary.BigEndian.PutUint64(stepBytes, ^step)
	valsDup, err = roTx.CursorDupSort(d.valsTable)
	if err != nil {
		return Collation{}, fmt.Errorf("create %s values cursorDupsort: %w", d.filenameBase, err)
	}
	defer valsDup.Close()

	for k, stepInDB, err := keysCursor.First(); k != nil; k, stepInDB, err = keysCursor.Next() {
		if err != nil {
			return coll, err
		}
		if !bytes.Equal(stepBytes, stepInDB) { // [txFrom; txTo)
			continue
		}

		copy(keySuffix, k)
		copy(keySuffix[len(k):], stepInDB)

		v, err = roTx.GetOne(d.valsTable, keySuffix[:len(k)+8])
		if err != nil {
			return coll, fmt.Errorf("find last %s value for aggregation step k=[%x]: %w", d.filenameBase, k, err)
		}

		if err = comp.AddWord(k); err != nil {
			return coll, fmt.Errorf("add %s values key [%x]: %w", d.filenameBase, k, err)
		}
		if err = comp.AddWord(v); err != nil {
			return coll, fmt.Errorf("add %s values [%x]=>[%x]: %w", d.filenameBase, k, v, err)
		}
	}

	closeCollation = false
	coll.valuesCount = coll.valuesComp.Count() / 2
	mxCollationSize.SetUint64(uint64(coll.valuesCount))
	return coll, nil
}

type StaticFiles struct {
	HistoryFiles
	valuesDecomp *compress.Decompressor
	valuesIdx    *recsplit.Index
	valuesBt     *BtIndex
	bloom        *ExistenceFilter
}

// CleanupOnError - call it on collation fail. It closing all files
func (sf StaticFiles) CleanupOnError() {
	if sf.valuesDecomp != nil {
		sf.valuesDecomp.Close()
	}
	if sf.valuesIdx != nil {
		sf.valuesIdx.Close()
	}
	if sf.valuesBt != nil {
		sf.valuesBt.Close()
	}
	if sf.bloom != nil {
		sf.bloom.Close()
	}
	sf.HistoryFiles.CleanupOnError()
}

// buildFiles performs potentially resource intensive operations of creating
// static files and their indices
func (d *Domain) buildFiles(ctx context.Context, step uint64, collation Collation, ps *background.ProgressSet) (StaticFiles, error) {
	if d.filenameBase == traceFileLife {
		d.logger.Warn("[snapshots] buildFiles", "step", step, "domain", d.filenameBase)
	}

	start := time.Now()
	defer func() {
		d.stats.LastFileBuildingTook = time.Since(start)
		mxBuildTook.ObserveDuration(start)
	}()

	hStaticFiles, err := d.History.buildFiles(ctx, step, collation.HistoryCollation, ps)
	if err != nil {
		return StaticFiles{}, err
	}
	valuesComp := collation.valuesComp

	var (
		valuesDecomp *compress.Decompressor
		valuesIdx    *recsplit.Index
		bt           *BtIndex
		bloom        *ExistenceFilter
	)
	closeComp := true
	defer func() {
		if closeComp {
			hStaticFiles.CleanupOnError()
			if valuesComp != nil {
				valuesComp.Close()
			}
			if valuesDecomp != nil {
				valuesDecomp.Close()
			}
			if valuesIdx != nil {
				valuesIdx.Close()
			}
			if bt != nil {
				bt.Close()
			}
			if bloom != nil {
				bloom.Close()
			}
		}
	}()
	if d.noFsync {
		valuesComp.DisableFsync()
	}
	if err = valuesComp.Compress(); err != nil {
		return StaticFiles{}, fmt.Errorf("compress %s values: %w", d.filenameBase, err)
	}
	valuesComp.Close()
	valuesComp = nil
	if valuesDecomp, err = compress.NewDecompressor(collation.valuesPath); err != nil {
		return StaticFiles{}, fmt.Errorf("open %s values decompressor: %w", d.filenameBase, err)
	}

	if !UseBpsTree {
		valuesIdxPath := d.kvAccessorFilePath(step, step+1)
		if valuesIdx, err = buildIndexThenOpen(ctx, valuesDecomp, d.compression, valuesIdxPath, d.dirs.Tmp, false, d.salt, ps, d.logger, d.noFsync); err != nil {
			return StaticFiles{}, fmt.Errorf("build %s values idx: %w", d.filenameBase, err)
		}
	}

	{
		btPath := d.kvBtFilePath(step, step+1)
		bt, err = CreateBtreeIndexWithDecompressor(btPath, DefaultBtreeM, valuesDecomp, d.compression, *d.salt, ps, d.dirs.Tmp, d.logger, d.noFsync)
		if err != nil {
			return StaticFiles{}, fmt.Errorf("build %s .bt idx: %w", d.filenameBase, err)
		}
	}
	{
		fPath := d.kvExistenceIdxFilePath(step, step+1)
		if dir.FileExist(fPath) {
			bloom, err = OpenExistenceFilter(fPath)
			if err != nil {
				return StaticFiles{}, fmt.Errorf("build %s .kvei: %w", d.filenameBase, err)
			}
		}
	}
	closeComp = false
	return StaticFiles{
		HistoryFiles: hStaticFiles,
		valuesDecomp: valuesDecomp,
		valuesIdx:    valuesIdx,
		valuesBt:     bt,
		bloom:        bloom,
	}, nil
}

func (d *Domain) missedBtreeIdxFiles() (l []*filesItem) {
	d.files.Walk(func(items []*filesItem) bool { // don't run slow logic while iterating on btree
		for _, item := range items {
			fromStep, toStep := item.startTxNum/d.aggregationStep, item.endTxNum/d.aggregationStep
			fPath := d.kvBtFilePath(fromStep, toStep)
			if !dir.FileExist(fPath) {
				l = append(l, item)
				continue
			}
			fPath = d.kvExistenceIdxFilePath(fromStep, toStep)
			if !dir.FileExist(fPath) {
				l = append(l, item)
				continue
			}
		}
		return true
	})
	return l
}
func (d *Domain) missedKviIdxFiles() (l []*filesItem) {
	d.files.Walk(func(items []*filesItem) bool { // don't run slow logic while iterating on btree
		for _, item := range items {
			fromStep, toStep := item.startTxNum/d.aggregationStep, item.endTxNum/d.aggregationStep
			fPath := d.kvAccessorFilePath(fromStep, toStep)
			if !dir.FileExist(fPath) {
				l = append(l, item)
			}
		}
		return true
	})
	return l
}

//func (d *Domain) missedExistenceFilter() (l []*filesItem) {
//	d.files.Walk(func(items []*filesItem) bool { // don't run slow logic while iterating on btree
//		for _, item := range items {
//			fromStep, toStep := item.startTxNum/d.aggregationStep, item.endTxNum/d.aggregationStep
//      bloomPath := d.kvExistenceIdxFilePath(fromStep, toStep)
//      if !dir.FileExist(bloomPath) {
//				l = append(l, item)
//			}
//		}
//		return true
//	})
//	return l
//}

// BuildMissedIndices - produce .efi/.vi/.kvi from .ef/.v/.kv
func (d *Domain) BuildMissedIndices(ctx context.Context, g *errgroup.Group, ps *background.ProgressSet) {
	d.History.BuildMissedIndices(ctx, g, ps)
	for _, item := range d.missedBtreeIdxFiles() {
		if !UseBpsTree {
			continue
		}
		if item.decompressor == nil {
			log.Warn(fmt.Sprintf("[dbg] BuildMissedIndices: item with nil decompressor %s %d-%d", d.filenameBase, item.startTxNum/d.aggregationStep, item.endTxNum/d.aggregationStep))
		}
		item := item

		g.Go(func() error {
			fromStep, toStep := item.startTxNum/d.aggregationStep, item.endTxNum/d.aggregationStep
			idxPath := d.kvBtFilePath(fromStep, toStep)
			if err := BuildBtreeIndexWithDecompressor(idxPath, item.decompressor, CompressNone, ps, d.dirs.Tmp, *d.salt, d.logger, d.noFsync); err != nil {
				return fmt.Errorf("failed to build btree index for %s:  %w", item.decompressor.FileName(), err)
			}
			return nil
		})
	}
	for _, item := range d.missedKviIdxFiles() {
		if UseBpsTree {
			continue
		}
		if item.decompressor == nil {
			log.Warn(fmt.Sprintf("[dbg] BuildMissedIndices: item with nil decompressor %s %d-%d", d.filenameBase, item.startTxNum/d.aggregationStep, item.endTxNum/d.aggregationStep))
		}
		item := item
		g.Go(func() error {
			if UseBpsTree {
				return nil
			}

			fromStep, toStep := item.startTxNum/d.aggregationStep, item.endTxNum/d.aggregationStep
			idxPath := d.kvAccessorFilePath(fromStep, toStep)
			ix, err := buildIndexThenOpen(ctx, item.decompressor, d.compression, idxPath, d.dirs.Tmp, false, d.salt, ps, d.logger, d.noFsync)
			if err != nil {
				return fmt.Errorf("build %s values recsplit index: %w", d.filenameBase, err)
			}
			ix.Close()
			return nil
		})
	}
}

func buildIndexThenOpen(ctx context.Context, d *compress.Decompressor, compressed FileCompression, idxPath, tmpdir string, values bool, salt *uint32, ps *background.ProgressSet, logger log.Logger, noFsync bool) (*recsplit.Index, error) {
	if err := buildIndex(ctx, d, compressed, idxPath, tmpdir, values, salt, ps, logger, noFsync); err != nil {
		return nil, err
	}
	return recsplit.OpenIndex(idxPath)
}
func buildIndexFilterThenOpen(ctx context.Context, d *compress.Decompressor, compressed FileCompression, idxPath, tmpdir string, salt *uint32, ps *background.ProgressSet, logger log.Logger, noFsync bool) (*ExistenceFilter, error) {
	if err := buildIdxFilter(ctx, d, compressed, idxPath, salt, ps, logger, noFsync); err != nil {
		return nil, err
	}
	if !dir.FileExist(idxPath) {
		return nil, nil
	}
	return OpenExistenceFilter(idxPath)
}
func buildIndex(ctx context.Context, d *compress.Decompressor, compressed FileCompression, idxPath, tmpdir string, values bool, salt *uint32, ps *background.ProgressSet, logger log.Logger, noFsync bool) error {
	_, fileName := filepath.Split(idxPath)
	count := d.Count()
	if !values {
		count = d.Count() / 2
	}
	p := ps.AddNew(fileName, uint64(count))
	defer ps.Delete(p)

	defer d.EnableReadAhead().DisableReadAhead()

	g := NewArchiveGetter(d.MakeGetter(), compressed)
	var rs *recsplit.RecSplit
	var err error
	if rs, err = recsplit.NewRecSplit(recsplit.RecSplitArgs{
		KeyCount:    count,
		Enums:       false,
		BucketSize:  2000,
		LeafSize:    8,
		TmpDir:      tmpdir,
		IndexFile:   idxPath,
		Salt:        salt,
		EtlBufLimit: etl.BufferOptimalSize / 2,
	}, logger); err != nil {
		return fmt.Errorf("create recsplit: %w", err)
	}
	defer rs.Close()
	rs.LogLvl(log.LvlTrace)
	if noFsync {
		rs.DisableFsync()
	}

	word := make([]byte, 0, 256)
	var keyPos, valPos uint64
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		g.Reset(0)
		for g.HasNext() {
			word, valPos = g.Next(word[:0])
			if values {
				if err = rs.AddKey(word, valPos); err != nil {
					return fmt.Errorf("add idx key [%x]: %w", word, err)
				}
			} else {
				if err = rs.AddKey(word, keyPos); err != nil {
					return fmt.Errorf("add idx key [%x]: %w", word, err)
				}
			}

			// Skip value
			keyPos, _ = g.Skip()

			p.Processed.Add(1)
		}
		if err = rs.Build(ctx); err != nil {
			if rs.Collision() {
				logger.Info("Building recsplit. Collision happened. It's ok. Restarting...")
				rs.ResetNextSalt()
			} else {
				return fmt.Errorf("build idx: %w", err)
			}
		} else {
			break
		}
	}
	return nil
}

func (d *Domain) integrateFiles(sf StaticFiles, txNumFrom, txNumTo uint64) {
	d.History.integrateFiles(sf.HistoryFiles, txNumFrom, txNumTo)

	fi := newFilesItem(txNumFrom, txNumTo, d.aggregationStep)
	fi.frozen = false
	fi.decompressor = sf.valuesDecomp
	fi.index = sf.valuesIdx
	fi.bindex = sf.valuesBt
	fi.existence = sf.bloom
	d.files.Set(fi)

	d.reCalcRoFiles()
}

// unwind is similar to prune but the difference is that it restores domain values from the history as of txFrom
// context Flush should be managed by caller.
func (dc *DomainContext) Unwind(ctx context.Context, rwTx kv.RwTx, step, txNumUnindTo uint64) error {
	d := dc.d
	//fmt.Printf("[domain][%s] unwinding to txNum=%d, step %d\n", d.filenameBase, txNumUnindTo, step)
	histRng, err := dc.hc.HistoryRange(int(txNumUnindTo), -1, order.Asc, -1, rwTx)
	if err != nil {
		return fmt.Errorf("historyRange %s: %w", dc.hc.h.filenameBase, err)
	}

	seen := make(map[string]struct{})
	restored := dc.NewWriter()

	for histRng.HasNext() && txNumUnindTo > 0 {
		k, v, err := histRng.Next()
		if err != nil {
			return err
		}

		ic, err := dc.hc.IdxRange(k, int(txNumUnindTo)-1, 0, order.Desc, -1, rwTx)
		if err != nil {
			return err
		}
		if ic.HasNext() {
			nextTxn, err := ic.Next()
			if err != nil {
				return err
			}
			restored.SetTxNum(nextTxn) // todo what if we actually had to decrease current step to provide correct update?
		} else {
			restored.SetTxNum(txNumUnindTo - 1)
		}
		//fmt.Printf("[%s]unwinding %x ->'%x' {%v}\n", dc.d.filenameBase, k, v, dc.TxNum())
		if err := restored.addValue(k, nil, v); err != nil {
			return err
		}
		seen[string(k)] = struct{}{}
	}

	keysCursor, err := dc.keysCursor(rwTx)
	if err != nil {
		return err
	}
	keysCursorForDeletes, err := rwTx.RwCursorDupSort(d.keysTable)
	if err != nil {
		return fmt.Errorf("create %s domain delete cursor: %w", d.filenameBase, err)
	}
	defer keysCursorForDeletes.Close()

	var valsC kv.RwCursor
	valsC, err = rwTx.RwCursor(d.valsTable)
	if err != nil {
		return err
	}
	defer valsC.Close()

	stepBytes := make([]byte, 8)
	binary.BigEndian.PutUint64(stepBytes, ^step)
	var k, v []byte

	for k, v, err = keysCursor.First(); k != nil; k, v, err = keysCursor.Next() {
		if err != nil {
			return fmt.Errorf("iterate over %s domain keys: %w", d.filenameBase, err)
		}
		if !bytes.Equal(v, stepBytes) {
			continue
		}
		if _, replaced := seen[string(k)]; !replaced && txNumUnindTo > 0 {
			continue
		}

		kk, _, err := valsC.SeekExact(common.Append(k, stepBytes))
		if err != nil {
			return err
		}
		if kk != nil {
			//fmt.Printf("[domain][%s] rm large value %x v %x\n", d.filenameBase, kk, vv)
			if err = valsC.DeleteCurrent(); err != nil {
				return err
			}
		}

		// This DeleteCurrent needs to the last in the loop iteration, because it invalidates k and v
		if _, _, err = keysCursorForDeletes.SeekBothExact(k, v); err != nil {
			return err
		}
		if err = keysCursorForDeletes.DeleteCurrent(); err != nil {
			return err
		}
	}

	logEvery := time.NewTicker(time.Second * 30)
	defer logEvery.Stop()
	if err := dc.hc.Prune(ctx, rwTx, txNumUnindTo, math.MaxUint64, math.MaxUint64, true, true, logEvery); err != nil {
		return fmt.Errorf("[domain][%s] unwinding, prune history to txNum=%d, step %d: %w", dc.d.filenameBase, txNumUnindTo, step, err)
	}
	return restored.Flush(ctx, rwTx)
}

func (d *Domain) isEmpty(tx kv.Tx) (bool, error) {
	k, err := kv.FirstKey(tx, d.keysTable)
	if err != nil {
		return false, err
	}
	k2, err := kv.FirstKey(tx, d.valsTable)
	if err != nil {
		return false, err
	}
	isEmptyHist, err := d.History.isEmpty(tx)
	if err != nil {
		return false, err
	}
	return k == nil && k2 == nil && isEmptyHist, nil
}

var (
	UseBtree = true // if true, will use btree for all files
)

func (dc *DomainContext) getLatestFromFiles(filekey []byte) (v []byte, found bool, err error) {
	if !dc.d.withExistenceIndex {
		return dc.getLatestFromFilesWithoutExistenceIndex(filekey)
	}

	hi, _ := dc.hc.ic.hashKey(filekey)

	for i := len(dc.files) - 1; i >= 0; i-- {
		if dc.d.withExistenceIndex {
			//if dc.files[i].src.existence == nil {
			//	panic(dc.files[i].src.decompressor.FileName())
			//}
			if dc.files[i].src.existence != nil {
				if !dc.files[i].src.existence.ContainsHash(hi) {
					//if traceGetLatest == dc.d.filenameBase {
					//	fmt.Printf("GetLatest(%s, %x) -> existence index %s -> false\n", dc.d.filenameBase, filekey, dc.files[i].src.existence.FileName)
					//}
					continue
				} else {
					//if traceGetLatest == dc.d.filenameBase {
					//	fmt.Printf("GetLatest(%s, %x) -> existence index %s -> true\n", dc.d.filenameBase, filekey, dc.files[i].src.existence.FileName)
					//}
				}
			} else {
				//if traceGetLatest == dc.d.filenameBase {
				//	fmt.Printf("GetLatest(%s, %x) -> existence index is nil %s\n", dc.d.filenameBase, filekey, dc.files[i].src.decompressor.FileName())
				//}
			}
		}

		//t := time.Now()
		v, found, err = dc.getFromFile(i, filekey)
		if err != nil {
			return nil, false, err
		}
		if !found {
			//if traceGetLatest == dc.d.filenameBase && i == 0 {
			if i == traceFileLevel {
				fmt.Printf("GetLatest(%s, %x) -> not found in file %s (false positive existence idx)\n", dc.d.filenameBase, filekey, dc.files[i].src.decompressor.FileName())
				//fmt.Printf("bloom false-positive probability: %s, %f, a-b=%d-%d\n", dc.files[i].src.existence.FileName, dc.files[i].src.existence.filter.FalsePosititveProbability(), A, B)

				//m := bloomfilter.OptimalM(dc.files[i].src.existence.filter.N()*10, 0.01)
				//k := bloomfilter.OptimalK(m, dc.files[i].src.existence.filter.N()*10)
				//fmt.Printf("recommended: m=%d,k=%d, have m=%d,k=%d\n", m, k, dc.files[i].src.existence.filter.M(), dc.files[i].src.existence.filter.K())
			}
			//	LatestStateReadGrindNotFound.ObserveDuration(t)
			continue
		}

		if i == traceFileLevel {
			fmt.Printf("GetLatest(%s, %x) -> found in file %s, %s\n", dc.d.filenameBase, filekey, dc.files[i].src.decompressor.FileName(), dbg.Stack()[:200])
		}

		//if traceGetLatest == dc.d.filenameBase {
		//	fmt.Printf("GetLatest(%s, %x) -> found in file %s\n", dc.d.filenameBase, filekey, dc.files[i].src.decompressor.FileName())
		//}
		//LatestStateReadGrind.ObserveDuration(t)
		return v, true, nil
	}
	//if traceGetLatest == dc.d.filenameBase {
	//	fmt.Printf("GetLatest(%s, %x) -> not found in %d files\n", dc.d.filenameBase, filekey, len(dc.files))
	//}

	return nil, false, nil
}

// GetAsOf does not always require usage of roTx. If it is possible to determine
// historical value based only on static files, roTx will not be used.
func (dc *DomainContext) GetAsOf(key []byte, txNum uint64, roTx kv.Tx) ([]byte, error) {
	v, hOk, err := dc.hc.GetNoStateWithRecent(key, txNum, roTx)
	if err != nil {
		return nil, err
	}
	if hOk {
		// if history returned marker of key creation
		// domain must return nil
		if len(v) == 0 {
			if traceGetAsOf == dc.d.filenameBase {
				fmt.Printf("GetAsOf(%s, %x, %d) -> not found in history\n", dc.d.filenameBase, key, txNum)
			}
			return nil, nil
		}
		if traceGetAsOf == dc.d.filenameBase {
			fmt.Printf("GetAsOf(%s, %x, %d) -> found in history\n", dc.d.filenameBase, key, txNum)
		}
		return v, nil
	}
	v, _, err = dc.GetLatest(key, nil, roTx)
	if err != nil {
		return nil, err
	}
	return v, nil
}

func (dc *DomainContext) Close() {
	if dc.files == nil { // invariant: it's safe to call Close multiple times
		return
	}
	files := dc.files
	dc.files = nil
	for i := 0; i < len(files); i++ {
		if files[i].src.frozen {
			continue
		}
		refCnt := files[i].src.refcount.Add(-1)
		//GC: last reader responsible to remove useles files: close it and delete
		if refCnt == 0 && files[i].src.canDelete.Load() {
			files[i].src.closeFilesAndRemove()
		}
	}
	//for _, r := range dc.readers {
	//	r.Close()
	//}
	dc.hc.Close()
}

func (dc *DomainContext) statelessGetter(i int) ArchiveGetter {
	if dc.getters == nil {
		dc.getters = make([]ArchiveGetter, len(dc.files))
	}
	r := dc.getters[i]
	if r == nil {
		r = NewArchiveGetter(dc.files[i].src.decompressor.MakeGetter(), dc.d.compression)
		dc.getters[i] = r
	}
	return r
}

func (dc *DomainContext) statelessIdxReader(i int) *recsplit.IndexReader {
	if dc.idxReaders == nil {
		dc.idxReaders = make([]*recsplit.IndexReader, len(dc.files))
	}
	r := dc.idxReaders[i]
	if r == nil {
		r = dc.files[i].src.index.GetReaderFromPool()
		dc.idxReaders[i] = r
	}
	return r
}

func (dc *DomainContext) statelessBtree(i int) *BtIndex {
	if dc.readers == nil {
		dc.readers = make([]*BtIndex, len(dc.files))
	}
	r := dc.readers[i]
	if r == nil {
		r = dc.files[i].src.bindex
		dc.readers[i] = r
	}
	return r
}

func (dc *DomainContext) valsCursor(tx kv.Tx) (c kv.Cursor, err error) {
	if dc.valsC != nil {
		return dc.valsC, nil
	}
	dc.valsC, err = tx.Cursor(dc.d.valsTable)
	if err != nil {
		return nil, err
	}
	return dc.valsC, nil
}

func (dc *DomainContext) keysCursor(tx kv.Tx) (c kv.CursorDupSort, err error) {
	if dc.keysC != nil {
		return dc.keysC, nil
	}
	dc.keysC, err = tx.CursorDupSort(dc.d.keysTable)
	if err != nil {
		return nil, err
	}
	return dc.keysC, nil
}

func (dc *DomainContext) GetLatest(key1, key2 []byte, roTx kv.Tx) ([]byte, bool, error) {
	//t := time.Now()
	key := key1
	if len(key2) > 0 {
		key = append(append(dc.keyBuf[:0], key1...), key2...)
	}

	keysC, err := dc.keysCursor(roTx)
	if err != nil {
		return nil, false, err
	}

	_, foundInvStep, err := keysC.SeekExact(key) // reads first DupSort value
	if err != nil {
		return nil, false, err
	}
	if foundInvStep != nil {
		copy(dc.valKeyBuf[:], key)
		copy(dc.valKeyBuf[len(key):], foundInvStep)

		valsC, err := dc.valsCursor(roTx)
		if err != nil {
			return nil, false, err
		}
		_, v, err := valsC.SeekExact(dc.valKeyBuf[:len(key)+8])
		if err != nil {
			return nil, false, fmt.Errorf("GetLatest value: %w", err)
		}
		//if traceGetLatest == dc.d.filenameBase {
		//	fmt.Printf("GetLatest(%s, %x) -> found in db\n", dc.d.filenameBase, key)
		//}
		//LatestStateReadDB.ObserveDuration(t)

		//if traceGetLatest == dc.d.filenameBase {
		//	fmt.Printf("GetLatest(%s, '%x') (found in db=%t)\n", dc.d.filenameBase, key, foundInvStep != nil)
		//}
		return v, true, nil
		//} else {
		//if traceGetLatest == dc.d.filenameBase {
		//it, err := dc.hc.IdxRange(common.FromHex("0x105083929bF9bb22C26cB1777Ec92661170D4285"), 1390000, -1, order.Asc, -1, roTx) //[from, to)
		//if err != nil {
		//	panic(err)
		//}
		//l := iter.ToArrU64Must(it)
		//fmt.Printf("L: %d\n", l)
		//it2, err := dc.hc.IdxRange(common.FromHex("0x105083929bF9bb22C26cB1777Ec92661170D4285"), -1, 1390000, order.Desc, -1, roTx) //[from, to)
		//if err != nil {
		//	panic(err)
		//}
		//l2 := iter.ToArrU64Must(it2)
		//fmt.Printf("K: %d\n", l2)
		//panic(1)
		//
		//	fmt.Printf("GetLatest(%s, %x) -> not found in db\n", dc.d.filenameBase, key)
		//}
	}
	//LatestStateReadDBNotFound.ObserveDuration(t)

	v, found, err := dc.getLatestFromFiles(key)
	if err != nil {
		return nil, false, err
	}
	return v, found, nil
}

func (dc *DomainContext) IteratePrefix(roTx kv.Tx, prefix []byte, it func(k []byte, v []byte) error) error {
	// Implementation:
	//     File endTxNum  = last txNum of file step
	//     DB endTxNum    = first txNum of step in db
	//     RAM endTxNum   = current txnum
	//  Example: stepSize=8, file=0-2.kv, db has key of step 2, current tx num is 17
	//     File endTxNum  = 15, because `0-2.kv` has steps 0 and 1, last txNum of step 1 is 15
	//     DB endTxNum    = 16, because db has step 2, and first txNum of step 2 is 16.
	//     RAM endTxNum   = 17, because current tcurrent txNum is 17

	var cp CursorHeap
	heap.Init(&cp)
	var k, v []byte
	var err error

	keysCursor, err := roTx.CursorDupSort(dc.d.keysTable)
	if err != nil {
		return err
	}
	defer keysCursor.Close()
	if k, v, err = keysCursor.Seek(prefix); err != nil {
		return err
	}
	if k != nil && bytes.HasPrefix(k, prefix) {
		step := ^binary.BigEndian.Uint64(v)
		endTxNum := step * dc.d.aggregationStep // DB can store not-finished step, it means - then set first txn in step - it anyway will be ahead of files

		keySuffix := make([]byte, len(k)+8)
		copy(keySuffix, k)
		copy(keySuffix[len(k):], v)
		if v, err = roTx.GetOne(dc.d.valsTable, keySuffix); err != nil {
			return err
		}
		heap.Push(&cp, &CursorItem{t: DB_CURSOR, key: k, val: v, c: keysCursor, endTxNum: endTxNum, reverse: true})
	}

	for i, item := range dc.files {
		if UseBtree || UseBpsTree {
			cursor, err := dc.statelessBtree(i).Seek(dc.statelessGetter(i), prefix)
			if err != nil {
				return err
			}
			if cursor == nil {
				continue
			}
			dc.d.stats.FilesQueries.Add(1)
			key := cursor.Key()
			if key != nil && bytes.HasPrefix(key, prefix) {
				val := cursor.Value()
				txNum := item.endTxNum - 1 // !important: .kv files have semantic [from, t)
				heap.Push(&cp, &CursorItem{t: FILE_CURSOR, dg: dc.statelessGetter(i), key: key, val: val, btCursor: cursor, endTxNum: txNum, reverse: true})
			}
		} else {
			ir := dc.statelessIdxReader(i)
			offset := ir.Lookup(prefix)
			g := dc.statelessGetter(i)
			g.Reset(offset)
			if !g.HasNext() {
				continue
			}
			key, _ := g.Next(nil)
			dc.d.stats.FilesQueries.Add(1)
			if key != nil && bytes.HasPrefix(key, prefix) {
				val, lofft := g.Next(nil)
				txNum := item.endTxNum - 1 // !important: .kv files have semantic [from, t)
				heap.Push(&cp, &CursorItem{t: FILE_CURSOR, dg: g, latestOffset: lofft, key: key, val: val, endTxNum: txNum, reverse: true})
			}
		}
	}

	for cp.Len() > 0 {
		lastKey := common.Copy(cp[0].key)
		lastVal := common.Copy(cp[0].val)
		// Advance all the items that have this key (including the top)
		for cp.Len() > 0 && bytes.Equal(cp[0].key, lastKey) {
			ci1 := heap.Pop(&cp).(*CursorItem)
			switch ci1.t {
			//case RAM_CURSOR:
			//	if ci1.iter.Next() {
			//		k = []byte(ci1.iter.Key())
			//		if k != nil && bytes.HasPrefix(k, prefix) {
			//			ci1.key = common.Copy(k)
			//			ci1.val = common.Copy(ci1.iter.Value())
			//		}
			//	}
			//	heap.Push(&cp, ci1)
			case FILE_CURSOR:
				if UseBtree || UseBpsTree {
					if ci1.btCursor.Next() {
						ci1.key = ci1.btCursor.Key()
						if ci1.key != nil && bytes.HasPrefix(ci1.key, prefix) {
							ci1.val = ci1.btCursor.Value()
							heap.Push(&cp, ci1)
						}
					}
				} else {
					ci1.dg.Reset(ci1.latestOffset)
					if !ci1.dg.HasNext() {
						break
					}
					key, _ := ci1.dg.Next(nil)
					if key != nil && bytes.HasPrefix(key, prefix) {
						ci1.key = key
						ci1.val, ci1.latestOffset = ci1.dg.Next(nil)
						heap.Push(&cp, ci1)
					}
				}
			case DB_CURSOR:
				k, v, err = ci1.c.NextNoDup()
				if err != nil {
					return err
				}
				if k != nil && bytes.HasPrefix(k, prefix) {
					ci1.key = k
					step := ^binary.BigEndian.Uint64(v)
					endTxNum := step * dc.d.aggregationStep // DB can store not-finished step, it means - then set first txn in step - it anyway will be ahead of files
					ci1.endTxNum = endTxNum

					keySuffix := make([]byte, len(k)+8)
					copy(keySuffix, k)
					copy(keySuffix[len(k):], v)
					if v, err = roTx.GetOne(dc.d.valsTable, keySuffix); err != nil {
						return err
					}
					ci1.val = v
					heap.Push(&cp, ci1)
				}
			}
		}
		if len(lastVal) > 0 {
			if err := it(lastKey, lastVal); err != nil {
				return err
			}
		}
	}
	return nil
}

func (dc *DomainContext) DomainRange(tx kv.Tx, fromKey, toKey []byte, ts uint64, asc order.By, limit int) (it iter.KV, err error) {
	if !asc {
		panic("implement me")
	}
	//histStateIt, err := tx.aggCtx.AccountHistoricalStateRange(asOfTs, fromKey, toKey, limit, tx.MdbxTx)
	//if err != nil {
	//	return nil, err
	//}
	//lastestStateIt, err := tx.aggCtx.DomainRangeLatest(tx.MdbxTx, kv.AccountDomain, fromKey, toKey, limit)
	//if err != nil {
	//	return nil, err
	//}
	histStateIt, err := dc.hc.WalkAsOf(ts, fromKey, toKey, tx, limit)
	if err != nil {
		return nil, err
	}
	lastestStateIt, err := dc.DomainRangeLatest(tx, fromKey, toKey, limit)
	if err != nil {
		return nil, err
	}
	return iter.UnionKV(histStateIt, lastestStateIt, limit), nil
}

func (dc *DomainContext) IteratePrefix2(roTx kv.Tx, fromKey, toKey []byte, limit int) (iter.KV, error) {
	return dc.DomainRangeLatest(roTx, fromKey, toKey, limit)
}

func (dc *DomainContext) DomainRangeLatest(roTx kv.Tx, fromKey, toKey []byte, limit int) (iter.KV, error) {
	fit := &DomainLatestIterFile{from: fromKey, to: toKey, limit: limit, dc: dc,
		roTx:         roTx,
		idxKeysTable: dc.d.keysTable,
		h:            &CursorHeap{},
	}
	if err := fit.init(dc); err != nil {
		return nil, err
	}
	return fit, nil
}

func (dc *DomainContext) CanPrune(tx kv.Tx) bool {
	return dc.hc.ic.CanPruneFrom(tx) < dc.maxTxNumInDomainFiles(false)
}

// history prunes keys in range [txFrom; txTo), domain prunes any records with rStep <= step.
// In case of context cancellation pruning stops and returns error, but simply could be started again straight away.
func (dc *DomainContext) Prune(ctx context.Context, rwTx kv.RwTx, step, txFrom, txTo, limit uint64, logEvery *time.Ticker) error {
	if !dc.CanPrune(rwTx) {
		return nil
	}

	st := time.Now()
	mxPruneInProgress.Inc()
	defer mxPruneInProgress.Dec()

	keysCursorForDeletes, err := rwTx.RwCursorDupSort(dc.d.keysTable)
	if err != nil {
		return fmt.Errorf("create %s domain cursor: %w", dc.d.filenameBase, err)
	}
	defer keysCursorForDeletes.Close()
	keysCursor, err := rwTx.RwCursorDupSort(dc.d.keysTable)
	if err != nil {
		return fmt.Errorf("create %s domain cursor: %w", dc.d.filenameBase, err)
	}
	defer keysCursor.Close()

	var (
		prunedKeys    uint64
		prunedMaxStep uint64
		prunedMinStep = uint64(math.MaxUint64)
		seek          = make([]byte, 0, 256)
	)

	prunedStep, _, err := GetExecV3PruneProgress(rwTx, dc.d.keysTable)
	if err != nil {
		dc.d.logger.Error("get domain pruning progress", "name", dc.d.filenameBase, "error", err)
	}

	if prunedStep != 0 {
		step = ^prunedStep
	}

	k, v, err := keysCursor.Last()
	//fmt.Printf("prune domain %s from %x|%x step %d\n", dc.d.filenameBase, k, v, step)

	for ; k != nil; k, v, err = keysCursor.Prev() {
		if err != nil {
			return fmt.Errorf("iterate over %s domain keys: %w", dc.d.filenameBase, err)
		}
		is := ^binary.BigEndian.Uint64(v)
		if is > step {
			continue
		}
		if limit == 0 {
			return nil
		}
		limit--

		seek = append(append(seek[:0], k...), v...)
		//fmt.Printf("prune key: %x->%x [%x] step %d dom %s\n", k, v, seek, ^binary.BigEndian.Uint64(v), dc.d.filenameBase)

		mxPruneSizeDomain.Inc()
		prunedKeys++

		err = rwTx.Delete(dc.d.valsTable, seek)
		if err != nil {
			return fmt.Errorf("prune domain value: %w", err)
		}

		// This DeleteCurrent needs to the last in the loop iteration, because it invalidates k and v
		if _, _, err = keysCursorForDeletes.SeekBothExact(k, v); err != nil {
			return err
		}
		if err = keysCursorForDeletes.DeleteCurrent(); err != nil {
			return err
		}

		if is < prunedMinStep {
			prunedMinStep = is
		}
		if is > prunedMaxStep {
			prunedMaxStep = is
		}

		select {
		case <-ctx.Done():
			if err := SaveExecV3PruneProgress(rwTx, dc.d.keysTable, ^step, nil); err != nil {
				dc.d.logger.Error("save domain pruning progress", "name", dc.d.filenameBase, "error", err)
			}
			return ctx.Err()
		case <-logEvery.C:
			if err := SaveExecV3PruneProgress(rwTx, dc.d.keysTable, ^step, nil); err != nil {
				dc.d.logger.Error("save domain pruning progress", "name", dc.d.filenameBase, "error", err)
			}
			dc.d.logger.Info("[snapshots] prune domain", "name", dc.d.filenameBase, "step", step,
				"steps", fmt.Sprintf("%.2f-%.2f", float64(txFrom)/float64(dc.d.aggregationStep), float64(txTo)/float64(dc.d.aggregationStep)))
		default:
		}
	}
	if prunedMinStep == math.MaxUint64 {
		prunedMinStep = 0
	} // minMax pruned step doesn't mean that we pruned all kv pairs for those step - we just pruned some keys of those steps.

	if err := SaveExecV3PruneProgress(rwTx, dc.d.keysTable, 0, nil); err != nil {
		dc.d.logger.Error("reset domain pruning progress", "name", dc.d.filenameBase, "error", err)
	}

	dc.d.logger.Info("[snapshots] prune domain", "name", dc.d.filenameBase, "step range", fmt.Sprintf("[%d, %d] requested %d", prunedMinStep, prunedMaxStep, step), "pruned keys", prunedKeys)
	mxPruneTookDomain.ObserveDuration(st)

	if err := dc.hc.Prune(ctx, rwTx, txFrom, txTo, limit, false, false, logEvery); err != nil {
		return fmt.Errorf("prune history at step %d [%d, %d): %w", step, txFrom, txTo, err)
	}
	return nil
}

type DomainLatestIterFile struct {
	dc *DomainContext

	roTx         kv.Tx
	idxKeysTable string

	limit int

	from, to []byte
	nextVal  []byte
	nextKey  []byte

	h *CursorHeap

	k, v, kBackup, vBackup []byte
}

func (hi *DomainLatestIterFile) Close() {
}
func (hi *DomainLatestIterFile) init(dc *DomainContext) error {
	// Implementation:
	//     File endTxNum  = last txNum of file step
	//     DB endTxNum    = first txNum of step in db
	//     RAM endTxNum   = current txnum
	//  Example: stepSize=8, file=0-2.kv, db has key of step 2, current tx num is 17
	//     File endTxNum  = 15, because `0-2.kv` has steps 0 and 1, last txNum of step 1 is 15
	//     DB endTxNum    = 16, because db has step 2, and first txNum of step 2 is 16.
	//     RAM endTxNum   = 17, because current tcurrent txNum is 17

	heap.Init(hi.h)
	var k, v []byte
	var err error

	keysCursor, err := hi.roTx.CursorDupSort(dc.d.keysTable)
	if err != nil {
		return err
	}
	if k, v, err = keysCursor.Seek(hi.from); err != nil {
		return err
	}
	if k != nil && (hi.to == nil || bytes.Compare(k, hi.to) < 0) {
		step := ^binary.BigEndian.Uint64(v)
		endTxNum := step * dc.d.aggregationStep // DB can store not-finished step, it means - then set first txn in step - it anyway will be ahead of files

		keySuffix := make([]byte, len(k)+8)
		copy(keySuffix, k)
		copy(keySuffix[len(k):], v)
		if v, err = hi.roTx.GetOne(dc.d.valsTable, keySuffix); err != nil {
			return err
		}
		heap.Push(hi.h, &CursorItem{t: DB_CURSOR, key: common.Copy(k), val: common.Copy(v), c: keysCursor, endTxNum: endTxNum, reverse: true})
	}

	for i, item := range dc.files {
		btCursor, err := dc.statelessBtree(i).Seek(dc.statelessGetter(i), hi.from)
		if err != nil {
			return err
		}
		if btCursor == nil {
			continue
		}

		key := btCursor.Key()
		if key != nil && (hi.to == nil || bytes.Compare(key, hi.to) < 0) {
			val := btCursor.Value()
			txNum := item.endTxNum - 1 // !important: .kv files have semantic [from, t)
			heap.Push(hi.h, &CursorItem{t: FILE_CURSOR, key: key, val: val, btCursor: btCursor, endTxNum: txNum, reverse: true})
		}
	}
	return hi.advanceInFiles()
}

func (hi *DomainLatestIterFile) advanceInFiles() error {
	for hi.h.Len() > 0 {
		lastKey := (*hi.h)[0].key
		lastVal := (*hi.h)[0].val

		// Advance all the items that have this key (including the top)
		for hi.h.Len() > 0 && bytes.Equal((*hi.h)[0].key, lastKey) {
			ci1 := heap.Pop(hi.h).(*CursorItem)
			switch ci1.t {
			case FILE_CURSOR:
				if ci1.btCursor.Next() {
					ci1.key = ci1.btCursor.Key()
					ci1.val = ci1.btCursor.Value()
					if ci1.key != nil && (hi.to == nil || bytes.Compare(ci1.key, hi.to) < 0) {
						heap.Push(hi.h, ci1)
					}
				}
			case DB_CURSOR:
				k, v, err := ci1.c.NextNoDup()
				if err != nil {
					return err
				}
				if k != nil && (hi.to == nil || bytes.Compare(k, hi.to) < 0) {
					ci1.key = common.Copy(k)
					step := ^binary.BigEndian.Uint64(v)
					endTxNum := step * hi.dc.d.aggregationStep // DB can store not-finished step, it means - then set first txn in step - it anyway will be ahead of files
					ci1.endTxNum = endTxNum

					keySuffix := make([]byte, len(k)+8)
					copy(keySuffix, k)
					copy(keySuffix[len(k):], v)
					if v, err = hi.roTx.GetOne(hi.dc.d.valsTable, keySuffix); err != nil {
						return err
					}
					ci1.val = common.Copy(v)
					heap.Push(hi.h, ci1)
				}
			}
		}
		if len(lastVal) > 0 {
			hi.nextKey, hi.nextVal = lastKey, lastVal
			return nil // founc
		}
	}
	hi.nextKey = nil
	return nil
}

func (hi *DomainLatestIterFile) HasNext() bool {
	return hi.limit != 0 && hi.nextKey != nil
}

func (hi *DomainLatestIterFile) Next() ([]byte, []byte, error) {
	hi.limit--
	hi.k, hi.v = append(hi.k[:0], hi.nextKey...), append(hi.v[:0], hi.nextVal...)

	// Satisfy iter.Dual Invariant 2
	hi.k, hi.kBackup, hi.v, hi.vBackup = hi.kBackup, hi.k, hi.vBackup, hi.v
	if err := hi.advanceInFiles(); err != nil {
		return nil, nil, err
	}
	return hi.kBackup, hi.vBackup, nil
}

func (d *Domain) stepsRangeInDBAsStr(tx kv.Tx) string {
	a1, a2 := d.History.InvertedIndex.stepsRangeInDB(tx)
	//ad1, ad2 := d.stepsRangeInDB(tx)
	//if ad2-ad1 < 0 {
	//	fmt.Printf("aaa: %f, %f\n", ad1, ad2)
	//}
	return fmt.Sprintf("%s:%.1f", d.filenameBase, a2-a1)
}
func (d *Domain) stepsRangeInDB(tx kv.Tx) (from, to float64) {
	fst, _ := kv.FirstKey(tx, d.valsTable)
	if len(fst) > 0 {
		to = float64(^binary.BigEndian.Uint64(fst[len(fst)-8:]))
	}
	lst, _ := kv.LastKey(tx, d.valsTable)
	if len(lst) > 0 {
		from = float64(^binary.BigEndian.Uint64(lst[len(lst)-8:]))
	}
	if to == 0 {
		to = from
	}
	return from, to
}

func (dc *DomainContext) Files() (res []string) {
	for _, item := range dc.files {
		if item.src.decompressor != nil {
			res = append(res, item.src.decompressor.FileName())
		}
	}
	return append(res, dc.hc.Files()...)
}

type SelectedStaticFiles struct {
	accounts       []*filesItem
	accountsIdx    []*filesItem
	accountsHist   []*filesItem
	storage        []*filesItem
	storageIdx     []*filesItem
	storageHist    []*filesItem
	code           []*filesItem
	codeIdx        []*filesItem
	codeHist       []*filesItem
	commitment     []*filesItem
	commitmentIdx  []*filesItem
	commitmentHist []*filesItem
	//codeI          int
	//storageI       int
	//accountsI      int
	//commitmentI    int
}

//func (sf SelectedStaticFiles) FillV3(s *SelectedStaticFilesV3) SelectedStaticFiles {
//	sf.accounts, sf.accountsIdx, sf.accountsHist = s.accounts, s.accountsIdx, s.accountsHist
//	sf.storage, sf.storageIdx, sf.storageHist = s.storage, s.storageIdx, s.storageHist
//	sf.code, sf.codeIdx, sf.codeHist = s.code, s.codeIdx, s.codeHist
//	sf.commitment, sf.commitmentIdx, sf.commitmentHist = s.commitment, s.commitmentIdx, s.commitmentHist
//	sf.codeI, sf.accountsI, sf.storageI, sf.commitmentI = s.codeI, s.accountsI, s.storageI, s.commitmentI
//	return sf
//}

func (sf SelectedStaticFiles) Close() {
	for _, group := range [][]*filesItem{
		sf.accounts, sf.accountsIdx, sf.accountsHist,
		sf.storage, sf.storageIdx, sf.storageHist,
		sf.code, sf.codeIdx, sf.codeHist,
		sf.commitment, sf.commitmentIdx, sf.commitmentHist,
	} {
		for _, item := range group {
			if item != nil {
				if item.decompressor != nil {
					item.decompressor.Close()
				}
				if item.index != nil {
					item.index.Close()
				}
				if item.bindex != nil {
					item.bindex.Close()
				}
			}
		}
	}
}

type MergedFiles struct {
	accounts                      *filesItem
	accountsIdx, accountsHist     *filesItem
	storage                       *filesItem
	storageIdx, storageHist       *filesItem
	code                          *filesItem
	codeIdx, codeHist             *filesItem
	commitment                    *filesItem
	commitmentIdx, commitmentHist *filesItem
}

func (mf MergedFiles) FillV3(m *MergedFilesV3) MergedFiles {
	mf.accounts, mf.accountsIdx, mf.accountsHist = m.accounts, m.accountsIdx, m.accountsHist
	mf.storage, mf.storageIdx, mf.storageHist = m.storage, m.storageIdx, m.storageHist
	mf.code, mf.codeIdx, mf.codeHist = m.code, m.codeIdx, m.codeHist
	mf.commitment, mf.commitmentIdx, mf.commitmentHist = m.commitment, m.commitmentIdx, m.commitmentHist
	return mf
}

func (mf MergedFiles) Close() {
	for _, item := range []*filesItem{
		mf.accounts, mf.accountsIdx, mf.accountsHist,
		mf.storage, mf.storageIdx, mf.storageHist,
		mf.code, mf.codeIdx, mf.codeHist,
		mf.commitment, mf.commitmentIdx, mf.commitmentHist,
		//mf.logAddrs, mf.logTopics, mf.tracesFrom, mf.tracesTo,
	} {
		if item != nil {
			if item.decompressor != nil {
				item.decompressor.Close()
			}
			if item.decompressor != nil {
				item.index.Close()
			}
			if item.bindex != nil {
				item.bindex.Close()
			}
		}
	}
}

// ---- deprecated area START ---

func (dc *DomainContext) getLatestFromFilesWithoutExistenceIndex(filekey []byte) (v []byte, found bool, err error) {
	if v, found, err = dc.getLatestFromWarmFiles(filekey); err != nil {
		return nil, false, err
	} else if found {
		return v, true, nil
	}

	if v, found, err = dc.getLatestFromColdFilesGrind(filekey); err != nil {
		return nil, false, err
	} else if found {
		return v, true, nil
	}

	// still not found, search in indexed cold shards
	return dc.getLatestFromColdFiles(filekey)
}

func (dc *DomainContext) getLatestFromWarmFiles(filekey []byte) ([]byte, bool, error) {
	exactWarmStep, ok, err := dc.hc.ic.warmLocality.lookupLatest(filekey)
	if err != nil {
		return nil, false, err
	}
	// _ = ok
	if !ok {
		return nil, false, nil
	}

	t := time.Now()
	exactTxNum := exactWarmStep * dc.d.aggregationStep
	for i := len(dc.files) - 1; i >= 0; i-- {
		isUseful := dc.files[i].startTxNum <= exactTxNum && dc.files[i].endTxNum > exactTxNum
		if !isUseful {
			continue
		}

		v, found, err := dc.getFromFileOld(i, filekey)
		if err != nil {
			return nil, false, err
		}
		if !found {
			LatestStateReadWarmNotFound.ObserveDuration(t)
			t = time.Now()
			continue
		}
		// fmt.Printf("warm [%d] want %x keys i idx %v %v\n", i, filekey, bt.ef.Count(), bt.decompressor.FileName())

		LatestStateReadWarm.ObserveDuration(t)
		return v, found, nil
	}
	return nil, false, nil
}

func (dc *DomainContext) getLatestFromColdFilesGrind(filekey []byte) (v []byte, found bool, err error) {
	// sometimes there is a gap between indexed cold files and indexed warm files. just grind them.
	// possible reasons:
	// - no locality indices at all
	// - cold locality index is "lazy"-built
	// corner cases:
	// - cold and warm segments can overlap
	lastColdIndexedTxNum := dc.hc.ic.coldLocality.indexedTo()
	firstWarmIndexedTxNum, haveWarmIdx := dc.hc.ic.warmLocality.indexedFrom()
	if !haveWarmIdx && len(dc.files) > 0 {
		firstWarmIndexedTxNum = dc.files[len(dc.files)-1].endTxNum
	}

	if firstWarmIndexedTxNum <= lastColdIndexedTxNum {
		return nil, false, nil
	}

	t := time.Now()
	//if firstWarmIndexedTxNum/dc.d.aggregationStep-lastColdIndexedTxNum/dc.d.aggregationStep > 0 && dc.d.withLocalityIndex {
	//	if dc.d.filenameBase != "commitment" {
	//		log.Warn("[dbg] gap between warm and cold locality", "cold", lastColdIndexedTxNum/dc.d.aggregationStep, "warm", firstWarmIndexedTxNum/dc.d.aggregationStep, "nil", dc.hc.ic.coldLocality == nil, "name", dc.d.filenameBase)
	//		if dc.hc.ic.coldLocality != nil && dc.hc.ic.coldLocality.file != nil {
	//			log.Warn("[dbg] gap", "cold_f", dc.hc.ic.coldLocality.file.src.bm.FileName())
	//		}
	//		if dc.hc.ic.warmLocality != nil && dc.hc.ic.warmLocality.file != nil {
	//			log.Warn("[dbg] gap", "warm_f", dc.hc.ic.warmLocality.file.src.bm.FileName())
	//		}
	//	}
	//}

	for i := len(dc.files) - 1; i >= 0; i-- {
		isUseful := dc.files[i].startTxNum >= lastColdIndexedTxNum && dc.files[i].endTxNum <= firstWarmIndexedTxNum
		if !isUseful {
			continue
		}
		v, ok, err := dc.getFromFileOld(i, filekey)
		if err != nil {
			return nil, false, err
		}
		if !ok {
			LatestStateReadGrindNotFound.ObserveDuration(t)
			t = time.Now()
			continue
		}
		LatestStateReadGrind.ObserveDuration(t)
		return v, true, nil
	}
	return nil, false, nil
}

func (dc *DomainContext) getLatestFromColdFiles(filekey []byte) (v []byte, found bool, err error) {
	// exactColdShard, ok, err := dc.hc.ic.coldLocality.lookupLatest(filekey)
	// if err != nil {
	// 	return nil, false, err
	// }
	// _ = ok
	// if !ok {
	// 	return nil, false, nil
	// }
	//dc.d.stats.FilesQuerie.Add(1)
	t := time.Now()
	// exactTxNum := exactColdShard * StepsInColdFile * dc.d.aggregationStep
	// fmt.Printf("exactColdShard: %d, exactTxNum=%d\n", exactColdShard, exactTxNum)
	for i := len(dc.files) - 1; i >= 0; i-- {
		// isUseful := dc.files[i].startTxNum <= exactTxNum && dc.files[i].endTxNum > exactTxNum
		//fmt.Printf("read3: %s, %t, %d-%d\n", dc.files[i].src.decompressor.FileName(), isUseful, dc.files[i].startTxNum, dc.files[i].endTxNum)
		// if !isUseful {
		// 	continue
		// }
		v, found, err = dc.getFromFileOld(i, filekey)
		if err != nil {
			return nil, false, err
		}
		if !found {
			LatestStateReadColdNotFound.ObserveDuration(t)
			t = time.Now()
			continue
		}
		LatestStateReadCold.ObserveDuration(t)
		return v, true, nil
	}
	return nil, false, nil
}
