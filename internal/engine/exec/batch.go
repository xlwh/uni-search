// Package exec implements the §3.3 Volcano-style execution engine: a
// pull-based Operator interface over pooled 4096-row ColumnBatch
// vectors. Operators compose into pipelines (Scan → Filter → TopN,
// ANN → TopN, BM25Score → TopN, Fusion) and the executor runs one
// goroutine per shard sub-plan (00 §3.3).
//
// M0 scope: this package owns the Operator abstraction, the
// ColumnBatch shape, and the §3.3 operator roster (Scan/BitmapFilter/
// PredFilter/TopN/ANN/BM25Score/SortMerge/Fusion). The optimizer
// (internal/engine/plan) produces plans over these operators.
package exec

import (
	"sync"

	"github.com/RoaringBitmap/roaring"
	"github.com/unisearch-db/unisearch/internal/meta"
)

// BatchSize is the §3.3 / 00 §3.1 batch capacity: 4096 rows per
// ColumnBatch. Operators that produce rows (Scan, ANN, BM25Score)
// must pack outputs into batches of at most this size; smaller
// tail batches are allowed at end-of-stream.
const BatchSize = 4096

// GlobalDocID packs (SegmentID, RowID) into one u64 for the §3.3
// DocIDs vector. SegmentID lives in the high 32 bits, RowID in the
// low 32 bits, so the storage layer's per-segment row id carries
// forward with no re-mapping.
func GlobalDocID(segID uint64, rowID uint32) uint64 {
	return segID<<32 | uint64(rowID)
}

// SplitDocID is the inverse of GlobalDocID.
func SplitDocID(id uint64) (segID uint64, rowID uint32) {
	return id >> 32, uint32(id & 0xFFFFFFFF)
}

// ColumnVec is one column's data inside a ColumnBatch. The
// FieldType tag picks the active slice; the other slices stay nil.
// Nulls, when non-nil, is the row-ordered bitmap of null rows
// (matching roaring's standard "bit i set = row i null").
type ColumnVec struct {
	FieldType meta.FieldType
	Name      string
	Longs     []int64
	Doubles   []float64
	Strings   []string
	Bools     []bool
	Nulls     *roaring.Bitmap
}

// ColumnBatch is the §3.3 / 00 §3.1 pooled batch: a fixed-capacity
// (≤ BatchSize) row group with per-column vectors, a selection
// bitmap (so filters do not copy rows), per-row global doc ids, and
// an optional score lane for BM25/ANN paths.
//
// Operators pull batches from their input via Next, transform them
// in place where possible, and emit a fresh batch (often pooled)
// downstream. The sync.Pool below keeps batches off the GC hot path
// for steady-state throughput.
type ColumnBatch struct {
	Rows   int
	Cols   []ColumnVec
	Sel    *roaring.Bitmap
	DocIDs []uint64
	Scores []float32
}

// batchPool recycles ColumnBatch instances across operators. The
// pool assumes a single goroutine per sub-plan (00 §3.3); concurrent
// pulls from the same operator are not supported.
var batchPool sync.Pool

// NewBatch returns a pooled ColumnBatch sized for up to BatchSize
// rows and the given column count. Callers must Release it back when
// done.
func NewBatch(numCols int) *ColumnBatch {
	if v, ok := batchPool.Get().(*ColumnBatch); ok && v != nil {
		v.Reset(numCols)
		return v
	}
	b := &ColumnBatch{
		DocIDs: make([]uint64, 0, BatchSize),
		Scores: make([]float32, 0, BatchSize),
		Cols:   make([]ColumnVec, numCols),
	}
	return b
}

// Release returns a batch to the pool. The caller must not touch the
// batch after Release.
func Release(b *ColumnBatch) {
	if b == nil {
		return
	}
	batchPool.Put(b)
}

// Reset clears the batch for reuse at the given column count. The
// underlying slice capacity is preserved.
func (b *ColumnBatch) Reset(numCols int) {
	b.Rows = 0
	b.DocIDs = b.DocIDs[:0]
	b.Scores = b.Scores[:0]
	if b.Sel != nil {
		b.Sel.Clear()
	}
	if cap(b.Cols) >= numCols {
		b.Cols = b.Cols[:numCols]
	} else {
		b.Cols = make([]ColumnVec, numCols)
	}
	for i := range b.Cols {
		c := &b.Cols[i]
		c.FieldType = ""
		c.Name = ""
		c.Longs = c.Longs[:0]
		c.Doubles = c.Doubles[:0]
		c.Strings = c.Strings[:0]
		c.Bools = c.Bools[:0]
		c.Nulls = nil
	}
}

// AppendRow is the row-append helper used by Scan/ANN/BM25Score
// operators: it pushes one (segID, rowID) and the typed value of one
// projected column. Returns false when the batch is full so the
// caller can flush and start a new batch.
func (b *ColumnBatch) AppendRow(segID uint64, rowID uint32) bool {
	if b.Rows >= BatchSize {
		return false
	}
	b.DocIDs = append(b.DocIDs, GlobalDocID(segID, rowID))
	b.Rows++
	return true
}

// AppendScore appends one score lane entry. Callers using the score
// lane must keep the score slice the same length as DocIDs.
func (b *ColumnBatch) AppendScore(s float32) {
	b.Scores = append(b.Scores, s)
}
