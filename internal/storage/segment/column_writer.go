package segment

import (
	"fmt"

	"github.com/unisearch-db/unisearch/internal/meta"
	"github.com/unisearch-db/unisearch/pkg/types"
)

// DefaultBlockSize is the default number of rows per block
// (design 01 §4.2: "Block 为 8192 行（可配）").
const DefaultBlockSize = 8192

// Option customizes writer behavior.
type Option func(*options)

type options struct {
	blockSize int
}

// WithBlockSize overrides the rows-per-block threshold (the last block of a
// column may be short). Zero or negative resets to DefaultBlockSize.
func WithBlockSize(n int) Option {
	return func(o *options) {
		if n > 0 {
			o.blockSize = n
		} else {
			o.blockSize = DefaultBlockSize
		}
	}
}

// ColumnFile is the finished col_{field_id}.bin content plus parsed metadata.
type ColumnFile struct {
	Data   []byte // complete file bytes (container, header, blocks)
	Header *ColumnHeader
	Stats  ColumnStats // whole-column min/max/null/cardinality for the footer
}

// ColumnWriter buffers one column's values, splits them into fixed-size
// blocks, selects the encoding per block via the selection matrix
// (design 01 §4.2), records per-block zone maps, and serializes the
// col_{field_id}.bin layout at Finish.
//
// A single ColumnWriter must not be used from multiple goroutines, but
// distinct ColumnWriters (distinct fields) may run in parallel inside one
// flush, matching the "并行编码各列" step of the flush pipeline (01 §5).
type ColumnWriter struct {
	fieldID   types.FieldID
	name      string
	ft        meta.FieldType
	blockSize int

	cur    *BlockData // current (not yet flushed) block buffer
	rows   int        // total rows appended so far
	data   []byte     // block data region: framed encoded blocks
	blocks []BlockIndex
	acc    columnAccumulator
	err    error // sticky error from wrong-typed appends
}

// NewColumnWriter returns a writer for one typed column. Supported types:
// keyword/text (string vector), long/date/ip (int64 vector), double
// (float64 vector), boolean (bool vector).
func NewColumnWriter(fieldID types.FieldID, name string, ft meta.FieldType, opts ...Option) *ColumnWriter {
	o := options{blockSize: DefaultBlockSize}
	for _, opt := range opts {
		opt(&o)
	}
	w := &ColumnWriter{
		fieldID:   fieldID,
		name:      name,
		ft:        ft,
		blockSize: o.blockSize,
	}
	switch ft {
	case meta.TypeKeyword, meta.TypeText:
	case meta.TypeLong, meta.TypeDate, meta.TypeIP:
	case meta.TypeDouble:
	case meta.TypeBoolean:
	default:
		w.err = fmt.Errorf("segment: column type %s is not storable as a column block file", ft)
		return w
	}
	w.cur = newBlockData(ft, w.blockSize)
	w.acc.init()
	return w
}

// NumRows returns the number of rows appended so far.
func (w *ColumnWriter) NumRows() int { return w.rows }

// Err returns the first sticky error (type-mismatched append) if any.
func (w *ColumnWriter) Err() error { return w.err }

// AppendNull appends a null row (valid for every column type).
func (w *ColumnWriter) AppendNull() {
	if w.err == nil {
		w.cur.appendNull()
		w.afterAppend()
	}
}

// AppendString appends a keyword/text value.
func (w *ColumnWriter) AppendString(s string) {
	if w.check(fullString) {
		w.cur.strs = append(w.cur.strs, s)
		w.cur.Rows++
		w.afterAppend()
	}
}

// AppendLong appends a long/date/ip value.
func (w *ColumnWriter) AppendLong(v int64) {
	if w.check(fullLong) {
		w.cur.longs = append(w.cur.longs, v)
		w.cur.Rows++
		w.afterAppend()
	}
}

// AppendDouble appends a double value.
func (w *ColumnWriter) AppendDouble(v float64) {
	if w.check(fullDouble) {
		w.cur.doubles = append(w.cur.doubles, v)
		w.cur.Rows++
		w.afterAppend()
	}
}

// AppendBool appends a boolean value.
func (w *ColumnWriter) AppendBool(v bool) {
	if w.check(fullBool) {
		w.cur.bools = append(w.cur.bools, v)
		w.cur.Rows++
		w.afterAppend()
	}
}

// check validates the append against the column type and returns false when
// a sticky error already exists.
func (w *ColumnWriter) check(what string) bool {
	if w.err != nil {
		return false
	}
	var want string
	switch w.ft {
	case meta.TypeKeyword, meta.TypeText:
		want = fullString
	case meta.TypeLong, meta.TypeDate, meta.TypeIP:
		want = fullLong
	case meta.TypeDouble:
		want = fullDouble
	case meta.TypeBoolean:
		want = fullBool
	default:
		want = fullString
	}
	if what != want {
		w.err = fmt.Errorf("segment: cannot append %s value to column %q of type %s", what, w.name, w.ft)
		return false
	}
	return true
}

const (
	fullString = "string"
	fullLong   = "long"
	fullDouble = "double"
	fullBool   = "bool"
)

// afterAppend accounts the row and flushes the buffer when it is full.
func (w *ColumnWriter) afterAppend() {
	w.rows++
	if w.cur.Rows >= w.blockSize {
		w.flushBlock()
	}
}

// flushBlock encodes the buffered block into the data region.
func (w *ColumnWriter) flushBlock() {
	n := w.cur.Rows
	if n == 0 {
		return
	}
	st, minV, maxV := blockStatsOf(w.ft, w.cur)
	enc := chooseEncoding(w.ft, st)
	payload, err := encodeBlock(w.ft, enc, w.cur)
	if err != nil {
		w.err = err
		// Keep the buffer consistent even on failure.
		w.cur = newBlockData(w.ft, w.blockSize)
		return
	}
	zone := ZoneMap{HasNull: w.cur.nullCount > 0}
	if minV != nil {
		zone.Min = serializeZoneValue(w.ft, minV)
		zone.Max = serializeZoneValue(w.ft, maxV)
	}
	w.blocks = append(w.blocks, BlockIndex{
		Offset:    uint64(len(w.data)),
		Encoding:  enc,
		Zone:      zone,
		NullCount: uint32(w.cur.nullCount),
		NumRows:   uint32(n),
	})
	w.data = appendU32LE(w.data, uint32(len(payload)))
	w.data = append(w.data, payload...)
	w.acc.observe(w.ft, w.cur, minV, maxV)
	w.cur = newBlockData(w.ft, w.blockSize)
}

// Finish flushes the trailing block and serializes the complete
// col_{field_id}.bin content. The writer is unusable afterwards.
func (w *ColumnWriter) Finish() (*ColumnFile, error) {
	if w.err != nil {
		return nil, w.err
	}
	w.flushBlock()
	if w.err != nil {
		return nil, w.err
	}
	hdr := &ColumnHeader{
		FieldID:   w.fieldID,
		FieldName: w.name,
		Type:      w.ft,
		NumRows:   uint64(w.rows),
		Blocks:    w.blocks,
	}
	hw := MarshalColumnHeader(hdr)
	file := appendU64LE(nil, colMagic)
	file = appendU32LE(file, uint32(len(hw)))
	file = append(file, hw...)
	file = append(file, w.data...)

	var minB, maxB []byte
	if w.acc.min != nil {
		minB = serializeZoneValue(w.ft, w.acc.min)
		maxB = serializeZoneValue(w.ft, w.acc.max)
	}
	return &ColumnFile{
		Data:   file,
		Header: hdr,
		Stats: ColumnStats{
			FieldID:     w.fieldID,
			Min:         minB,
			Max:         maxB,
			NullCount:   uint64(w.acc.nullCount),
			Cardinality: uint64(w.acc.cardinality()),
		},
	}, nil
}

// blockStatsOf computes the selection-matrix statistics (cardinality,
// monotonicity) and the typed min/max of a block, excluding nulls.
// minV/maxV are nil when the block has no non-null value.
func blockStatsOf(ft meta.FieldType, d *BlockData) (st blockStats, minV, maxV any) {
	n := d.Rows
	distinct := make(map[any]struct{}, n/2+1)
	monotonicUp, monotonicDown := true, true
	switch ft {
	case meta.TypeKeyword, meta.TypeText:
		for i := 0; i < n; i++ {
			if d.IsNull(i) {
				continue
			}
			s := d.strs[i]
			distinct[s] = struct{}{}
			if minV == nil || s < minV.(string) {
				minV = s
			}
			if maxV == nil || s > maxV.(string) {
				maxV = s
			}
		}
	case meta.TypeLong, meta.TypeDate, meta.TypeIP:
		prevSet, prev := false, int64(0)
		for i := 0; i < n; i++ {
			if d.IsNull(i) {
				continue
			}
			v := d.longs[i]
			distinct[v] = struct{}{}
			if minV == nil || v < minV.(int64) {
				minV = v
			}
			if maxV == nil || v > maxV.(int64) {
				maxV = v
			}
			if prevSet {
				if v < prev {
					monotonicUp = false
				}
				if v > prev {
					monotonicDown = false
				}
			}
			prev, prevSet = v, true
		}
	case meta.TypeDouble:
		prevSet, prev := false, float64(0)
		for i := 0; i < n; i++ {
			if d.IsNull(i) {
				continue
			}
			v := d.doubles[i]
			distinct[v] = struct{}{}
			if minV == nil || v < minV.(float64) {
				minV = v
			}
			if maxV == nil || v > maxV.(float64) {
				maxV = v
			}
			if prevSet {
				if v < prev {
					monotonicUp = false
				}
				if v > prev {
					monotonicDown = false
				}
			}
			prev, prevSet = v, true
		}
	case meta.TypeBoolean:
		for i := 0; i < n; i++ {
			if d.IsNull(i) {
				continue
			}
			v := d.bools[i]
			distinct[v] = struct{}{}
			if minV == nil {
				minV, maxV = v, v
			} else {
				if !v {
					minV = false
				} else {
					maxV = true
				}
			}
		}
	}
	st.cardinality = len(distinct)
	st.monotonic = monotonicUp || monotonicDown
	return st, minV, maxV
}

// columnAccumulator aggregates whole-column stats across blocks.
type columnAccumulator struct {
	min, max  any
	nullCount int
	distinct  map[any]struct{}
}

func (a *columnAccumulator) init() {
	a.distinct = make(map[any]struct{})
}

func (a *columnAccumulator) cardinality() int { return len(a.distinct) }

// observe folds one block into the column aggregates. The per-block
// distinct values are re-derived here to keep the accumulator exact.
func (a *columnAccumulator) observe(ft meta.FieldType, d *BlockData, minV, maxV any) {
	a.nullCount += d.nullCount
	if minV == nil {
		return
	}
	if a.min == nil {
		a.min, a.max = minV, maxV
	} else {
		switch a.min.(type) {
		case string:
			if minV.(string) < a.min.(string) {
				a.min = minV
			}
			if maxV.(string) > a.max.(string) {
				a.max = maxV
			}
		case int64:
			if minV.(int64) < a.min.(int64) {
				a.min = minV
			}
			if maxV.(int64) > a.max.(int64) {
				a.max = maxV
			}
		case float64:
			if minV.(float64) < a.min.(float64) {
				a.min = minV
			}
			if maxV.(float64) > a.max.(float64) {
				a.max = maxV
			}
		case bool:
			if minV.(bool) != a.min.(bool) {
				a.min = false
			}
			if maxV.(bool) != a.max.(bool) {
				a.max = true
			}
		}
	}
	// Exact cardinality across the column.
	n := d.Rows
	switch ft {
	case meta.TypeKeyword, meta.TypeText:
		for i := 0; i < n; i++ {
			if !d.IsNull(i) {
				a.distinct[d.strs[i]] = struct{}{}
			}
		}
	case meta.TypeLong, meta.TypeDate, meta.TypeIP:
		for i := 0; i < n; i++ {
			if !d.IsNull(i) {
				a.distinct[d.longs[i]] = struct{}{}
			}
		}
	case meta.TypeDouble:
		for i := 0; i < n; i++ {
			if !d.IsNull(i) {
				a.distinct[d.doubles[i]] = struct{}{}
			}
		}
	case meta.TypeBoolean:
		for i := 0; i < n; i++ {
			if !d.IsNull(i) {
				a.distinct[d.bools[i]] = struct{}{}
			}
		}
	}
}
