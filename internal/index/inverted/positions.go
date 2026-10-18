package inverted

import (
	"encoding/binary"
	"fmt"
	"io"
)

// Position blocks (§3.2: pos.{field}.bin). For each (term, doc) the
// inverted index stores the list of token positions where the term
// occurs in that document. Positions are 0-based token positions emitted
// by the analyzer (analyzer.Token.Pos). Phrase queries (match_phrase)
// consume them to verify token order; the BM25 field-length bucket
// (§3.2 fldLen bucket) lives in the posting block, not here.
//
// M0 encoding per doc: delta-encode the sorted positions (pos[0] stays
// as-is, pos[i] - pos[i-1] for i>0), then FOR-bitpack the deltas. The
// doc-level frame is:
//
//	u32 numPositions | FOR(delta-encoded positions)
//
// Empty position lists (numPositions = 0) are valid: terms on non-phrase
// fields store no positions and phrase queries on them return no
// matches.

// encodePositions writes one doc's position frame to w. positions must
// be sorted ascending and unique (the analyzer guarantees this for a
// single term in a single doc).
func encodePositions(w io.Writer, positions []uint16) error {
	if err := binary.Write(w, binary.LittleEndian, uint32(len(positions))); err != nil {
		return fmt.Errorf("inverted: write position count: %w", err)
	}
	if len(positions) == 0 {
		return nil
	}
	deltas := make([]uint16, len(positions))
	deltas[0] = positions[0]
	prev := positions[0]
	for i := 1; i < len(positions); i++ {
		deltas[i] = positions[i] - prev
		prev = positions[i]
	}
	if _, err := w.Write(forEncodeU16(deltas)); err != nil {
		return fmt.Errorf("inverted: write positions: %w", err)
	}
	return nil
}

// decodePositions reads one doc's position frame from a byte slice at
// offset off and returns (positions, nextOff). It is the inverse of
// encodePositions.
func decodePositions(data []byte, off int) (positions []uint16, nextOff int, err error) {
	if off+4 > len(data) {
		return nil, off, fmt.Errorf("inverted: truncated position frame header at %d", off)
	}
	count := binary.LittleEndian.Uint32(data[off:])
	off += 4
	if count == 0 {
		return []uint16{}, off, nil
	}
	if off+1 > len(data) {
		return nil, off, fmt.Errorf("inverted: truncated position FOR block at %d", off)
	}
	bitW := int(data[off])
	packedLen := (int(count)*bitW + 7) / 8
	if off+1+packedLen > len(data) {
		return nil, off, fmt.Errorf("inverted: truncated position FOR payload at %d", off)
	}
	deltas, err := forDecodeU16(data[off:off+1+packedLen], int(count))
	if err != nil {
		return nil, off, err
	}
	off += 1 + packedLen
	positions = make([]uint16, len(deltas))
	prev := uint16(0)
	for i, d := range deltas {
		if i == 0 {
			positions[i] = d
		} else {
			positions[i] = prev + d
		}
		prev = positions[i]
	}
	return positions, off, nil
}
