package segment

import (
	"fmt"
	"math/bits"
	"sort"

	"github.com/unisearch-db/unisearch/pkg/types"
)

// _id storage (design 01 §4.4): id.dict is the sorted, zstd-compressed id
// dictionary; id.hash is the optional-load open-addressing hash table used
// for _id -> RowID point lookups. Formats are documented in
// api/proto/segment.proto.

const emptyIDSlot uint32 = 0xFFFFFFFF

// idEntry is one (id, row) pair of the dictionary.
type idEntry struct {
	id  string
	row types.RowID
}

// fnv1a computes the 64-bit FNV-1a hash used by id.hash.
func fnv1a(s string) uint64 {
	const (
		offset64 = 14695981039346656037
		prime64  = 1099511628211
	)
	h := uint64(offset64)
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= prime64
	}
	return h
}

// buildIDDict serializes the id.dict container: sorted entries, zstd.
func buildIDDict(ids []string) ([]byte, error) {
	entries := make([]idEntry, len(ids))
	for i, id := range ids {
		entries[i] = idEntry{id: id, row: types.RowID(i)}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].id < entries[j].id })

	blob := appendU32LE(nil, uint32(len(entries)))
	for _, e := range entries {
		blob = appendU32LE(blob, uint32(len(e.id)))
		blob = append(blob, e.id...)
		blob = appendU32LE(blob, uint32(e.row))
	}
	frame, err := zstdCompress(blob)
	if err != nil {
		return nil, err
	}
	out := appendU32LE(nil, uint32(len(frame)))
	return append(out, frame...), nil
}

// readIDDict parses an id.dict container into its sorted entries.
func readIDDict(data []byte) ([]idEntry, error) {
	if len(data) < 4 {
		return nil, fmt.Errorf("segment: truncated id.dict")
	}
	frameLen := int(readU32LE(data, 0))
	if 4+frameLen > len(data) {
		return nil, fmt.Errorf("segment: truncated id.dict frame")
	}
	blob, err := zstdDecompress(data[4 : 4+frameLen])
	if err != nil {
		return nil, err
	}
	if len(blob) < 4 {
		return nil, fmt.Errorf("segment: corrupt id.dict blob")
	}
	count := int(readU32LE(blob, 0))
	entries := make([]idEntry, 0, count)
	off := 4
	for range count {
		if off+4 > len(blob) {
			return nil, fmt.Errorf("segment: truncated id.dict entry")
		}
		l := int(readU32LE(blob, off))
		off += 4
		if l < 0 || off+l+4 > len(blob) {
			return nil, fmt.Errorf("segment: truncated id.dict entry")
		}
		e := idEntry{id: string(blob[off : off+l])}
		off += l
		e.row = types.RowID(readU32LE(blob, off))
		off += 4
		entries = append(entries, e)
	}
	return entries, nil
}

// buildIDHash serializes the id.hash container: power-of-two open
// addressing table with linear probing, load factor <= 0.7.
func buildIDHash(ids []string) ([]byte, error) {
	n := len(ids)
	capacity := 1
	need := (n * 10) / 7
	if need > 0 {
		capacity = 1 << uint(bits.Len(uint(need-1)))
	}
	slots := make([]byte, 0, capacity*12)
	occupied := make([]bool, capacity)
	rows := make([]uint32, capacity)
	idAt := make([]string, capacity)
	for i, id := range ids {
		h := fnv1a(id)
		idx := int(h & uint64(capacity-1))
		for occupied[idx] {
			if idAt[idx] == id {
				return nil, fmt.Errorf("segment: duplicate _id %q", id)
			}
			idx = (idx + 1) & (capacity - 1)
		}
		occupied[idx] = true
		idAt[idx] = id
		rows[idx] = uint32(i)
	}
	for i := range capacity {
		if !occupied[i] {
			slots = appendU32LE(slots, emptyIDSlot)
			slots = appendU32LE(slots, 0)
			continue
		}
		slots = appendU32LE(slots, rows[i])
		slots = appendU32LE(slots, uint32(len(idAt[i])))
		slots = append(slots, idAt[i]...)
	}
	out := appendU32LE(nil, uint32(capacity))
	out = appendU32LE(out, uint32(n))
	return append(out, slots...), nil
}

// readIDHash parses an id.hash container, returning its raw slot table for
// LookupID / row-to-id reconstruction.
type idHashTable struct {
	capacity int
	count    int
	slots    []byte // raw serialized slots, variable stride
	offs     []int  // lazily built per-slot offsets into slots (len = capacity+1)
}

func readIDHash(data []byte) (*idHashTable, error) {
	if len(data) < 8 {
		return nil, fmt.Errorf("segment: truncated id.hash")
	}
	capacity := int(readU32LE(data, 0))
	count := int(readU32LE(data, 4))
	if capacity == 0 || capacity&(capacity-1) != 0 {
		return nil, fmt.Errorf("segment: id.hash capacity %d is not a power of two", capacity)
	}
	t := &idHashTable{capacity: capacity, count: count, slots: data[8:]}
	// Validate that the slot table parses cleanly end-to-end.
	off := 0
	for range capacity {
		if off+8 > len(t.slots) {
			return nil, fmt.Errorf("segment: truncated id.hash slot")
		}
		row := readU32LE(t.slots, off)
		l := int(readU32LE(t.slots, off+4))
		if row == emptyIDSlot {
			off += 8
			continue
		}
		if l < 0 || off+8+l > len(t.slots) {
			return nil, fmt.Errorf("segment: truncated id.hash slot")
		}
		off += 8 + l
	}
	if off != len(t.slots) {
		return nil, fmt.Errorf("segment: trailing bytes in id.hash")
	}
	return t, nil
}

// slotAt returns the (row, id) at slot index i; ok=false for empty slots.
func (t *idHashTable) slotAt(i int) (row types.RowID, id string, ok bool) {
	// Slots are variable stride; walk from the region start recorded per
	// lookup is avoided by precomputing offsets once.
	if t.offs == nil {
		t.offs = make([]int, t.capacity+1)
		off := 0
		for j := range t.capacity {
			t.offs[j] = off
			l := 0
			if readU32LE(t.slots, off) != emptyIDSlot {
				l = int(readU32LE(t.slots, off+4))
			}
			off += 8 + l
		}
		t.offs[t.capacity] = off
	}
	if i < 0 || i >= t.capacity {
		return 0, "", false
	}
	base := t.offs[i]
	if readU32LE(t.slots, base) == emptyIDSlot {
		return 0, "", false
	}
	l := int(readU32LE(t.slots, base+4))
	return types.RowID(readU32LE(t.slots, base)), string(t.slots[base+8 : base+8+l]), true
}

// lookup returns the RowID for id, using the same linear probing as
// buildIDHash.
func (t *idHashTable) lookup(id string) (types.RowID, bool) {
	h := fnv1a(id)
	idx := int(h & uint64(t.capacity-1))
	for j := 0; j < t.capacity; j++ {
		row, sid, ok := t.slotAt(idx)
		if !ok {
			return 0, false // empty slot: absent
		}
		if sid == id {
			return row, true
		}
		idx = (idx + 1) & (t.capacity - 1)
	}
	return 0, false
}

// rowsToIDs reconstructs the row-ordered id vector by iterating slots.
func (t *idHashTable) rowsToIDs(n int) ([]string, error) {
	ids := make([]string, n)
	seen := 0
	for i := range t.capacity {
		row, id, ok := t.slotAt(i)
		if !ok {
			continue
		}
		if int(row) >= n {
			return nil, fmt.Errorf("segment: id.hash row %d out of range %d", row, n)
		}
		ids[row] = id
		seen++
	}
	if seen != n {
		return nil, fmt.Errorf("segment: id.hash holds %d ids, expected %d", seen, n)
	}
	return ids, nil
}
