package inverted

import (
	"fmt"
	"io"
	"math"
)

// BM25Params controls the §3.3 scoring formula. Defaults follow ES 8.x
// and the design contract: k1 = 1.2 (term-frequency saturation rate),
// b = 0.75 (field-length normalization strength). Both are tunable per
// field in the full mapping (04 §4), but M0 keeps one set per segment.
type BM25Params struct {
	K1 float64
	B  float64
}

// DefaultBM25Params is the §3.3 default (k1=1.2, b=0.75).
func DefaultBM25Params() BM25Params { return BM25Params{K1: 1.2, B: 0.75} }

// SegmentStats are the segment-level statistics the §3.3 BM25 formula
// consumes: N = document count of the segment, AvgFieldLen = average
// field length across documents (in tokens). These are persisted in
// stats.bin and read back at Load; the §3 cross-segment IDF merge
// protocol (03 §7) reconciles them at query time.
type SegmentStats struct {
	DocCount    uint32
	TotalTokens uint64
	AvgFieldLen float64
}

// NewSegmentStats computes the segment-level statistics from per-doc
// field lengths. docCount is the number of documents; the sum of field
// lengths determines AvgFieldLen.
func NewSegmentStats(fieldLengths []int) SegmentStats {
	var sum uint64
	for _, l := range fieldLengths {
		if l < 0 {
			l = 0
		}
		sum += uint64(l)
	}
	n := uint32(len(fieldLengths))
	avg := 0.0
	if n > 0 {
		avg = float64(sum) / float64(n)
	}
	return SegmentStats{DocCount: n, TotalTokens: sum, AvgFieldLen: avg}
}

// IDF returns the §3.3 inverse document frequency:
//
//	IDF(t) = ln(1 + (N - df + 0.5)/(df + 0.5))
//
// The "+1" inside the logarithm keeps the score non-negative for terms
// that appear in every document (df → N), matching the ES 8.x behavior
// the design pins to.
func (s SegmentStats) IDF(df int) float64 {
	if df <= 0 {
		return 0
	}
	n := float64(s.DocCount)
	d := float64(df)
	return math.Log(1 + (n-d+0.5)/(d+0.5))
}

// ScoreTerm computes the §3.3 contribution of one query term to the
// BM25 score of a document:
//
//	IDF(t) · (tf · (k1+1)) / (tf + k1·(1 - b + b·fldLen/avgFL))
//
// fldLen is the document's field length (token count); avgFL is the
// segment average. When avgFL is 0 the length-normalization factor
// collapses to 1 - b (degenerate segment with no tokens; the formula
// is still well-defined).
func (s SegmentStats) ScoreTerm(p BM25Params, tf int, fldLen int, df int) float64 {
	if tf <= 0 || p.K1 <= 0 {
		return 0
	}
	idf := s.IDF(df)
	avgFL := s.AvgFieldLen
	norm := 1 - p.B
	if avgFL > 0 {
		norm = 1 - p.B + p.B*float64(fldLen)/avgFL
	}
	denom := float64(tf) + p.K1*norm
	if denom == 0 {
		return 0
	}
	return idf * (float64(tf)*(p.K1+1)) / denom
}

// ScoreQuery is the §3.3 sum of per-term contributions over a query
// expressed as a {term → tf_in_query} map. For the M0 single-segment
// path the per-term document frequency is looked up in the segment's
// posting lists. The returned score is the full BM25 score for one
// document.
//
// queryTermTF maps each query term to the number of times it occurs in
// the query (usually 1); dfOf maps each query term to its document
// frequency in the segment. The caller is responsible for resolving
// the document's per-term tf and field length (typically by walking
// each term's PostingList).
func (s SegmentStats) ScoreQuery(p BM25Params, contributions []float64) float64 {
	total := 0.0
	for _, c := range contributions {
		total += c
	}
	return total
}

// statsMagic / statsVersion tag stats.bin ("IST1" little-endian).
const (
	statsMagic   uint32 = 0x31545349 // "IST1" (LE)
	statsVersion uint8  = 1
)

// writeStats serializes segment-level statistics to w. Format:
//
//	magic u32 | version u8 | fieldLen u32 | field bytes |
//	docCount u32 | totalTokens u64 | avgFieldLen f64
func writeStats(w interface {
	Write(p []byte) (n int, err error)
}, field string, st SegmentStats) error {
	hdr := []any{statsMagic, statsVersion, uint32(len(field))}
	for _, v := range hdr {
		if err := writeAny(w, v); err != nil {
			return fmt.Errorf("inverted: write stats header: %w", err)
		}
	}
	if _, err := w.Write([]byte(field)); err != nil {
		return fmt.Errorf("inverted: write stats field: %w", err)
	}
	for _, v := range []any{st.DocCount, st.TotalTokens, st.AvgFieldLen} {
		if err := writeAny(w, v); err != nil {
			return fmt.Errorf("inverted: write stats body: %w", err)
		}
	}
	return nil
}

// readStats is the inverse of writeStats.
func readStats(r *byteReader) (field string, st SegmentStats, err error) {
	var magic uint32
	if magic, err = r.readU32(); err != nil {
		return "", SegmentStats{}, fmt.Errorf("inverted: read stats magic: %w", err)
	}
	if magic != statsMagic {
		return "", SegmentStats{}, fmt.Errorf("inverted: bad stats magic %#x", magic)
	}
	if r.off >= len(r.data) {
		return "", SegmentStats{}, fmt.Errorf("inverted: truncated stats version")
	}
	version := r.data[r.off]
	r.off++
	if version != statsVersion {
		return "", SegmentStats{}, fmt.Errorf("inverted: unsupported stats version %d", version)
	}
	fl, err := r.readU32()
	if err != nil {
		return "", SegmentStats{}, fmt.Errorf("inverted: read stats field len: %w", err)
	}
	fb, err := r.readN(int(fl))
	if err != nil {
		return "", SegmentStats{}, fmt.Errorf("inverted: read stats field: %w", err)
	}
	field = string(fb)
	if st.DocCount, err = r.readU32(); err != nil {
		return "", SegmentStats{}, fmt.Errorf("inverted: read stats docCount: %w", err)
	}
	if st.TotalTokens, err = r.readU64(); err != nil {
		return "", SegmentStats{}, fmt.Errorf("inverted: read stats totalTokens: %w", err)
	}
	if st.AvgFieldLen, err = r.readF64(); err != nil {
		return "", SegmentStats{}, fmt.Errorf("inverted: read stats avgFieldLen: %w", err)
	}
	return field, st, nil
}

// writeAny dispatches on the concrete Go type of v to write a fixed-size
// little-endian value. Supported types: uint8, uint32, uint64, float64.
func writeAny(w interface{ Write(p []byte) (n int, err error) }, v any) error {
	switch x := v.(type) {
	case uint8:
		_, err := w.Write([]byte{x})
		return err
	case uint32:
		_, err := w.Write(appendU32LE(nil, x))
		return err
	case uint64:
		_, err := w.Write(appendU64LE(nil, x))
		return err
	case float64:
		_, err := w.Write(appendU64LE(nil, math.Float64bits(x)))
		return err
	default:
		return fmt.Errorf("inverted: unsupported stats type %T", v)
	}
}

// readU64 and readF64 extend byteReader for the stats file fields.
func (r *byteReader) readU64() (uint64, error) {
	if r.off+8 > len(r.data) {
		return 0, io.ErrUnexpectedEOF
	}
	v := uint64(r.data[r.off]) | uint64(r.data[r.off+1])<<8 |
		uint64(r.data[r.off+2])<<16 | uint64(r.data[r.off+3])<<24 |
		uint64(r.data[r.off+4])<<32 | uint64(r.data[r.off+5])<<40 |
		uint64(r.data[r.off+6])<<48 | uint64(r.data[r.off+7])<<56
	r.off += 8
	return v, nil
}

func (r *byteReader) readF64() (float64, error) {
	u, err := r.readU64()
	if err != nil {
		return 0, err
	}
	return math.Float64frombits(u), nil
}
