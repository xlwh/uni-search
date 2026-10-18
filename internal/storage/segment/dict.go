package segment

import (
	"fmt"
	"sort"

	"github.com/unisearch-db/unisearch/internal/meta"
)

// Dictionary encoding (EncodingDict): the sorted distinct values of the
// block go into a zstd-compressed dictionary, and each row becomes a
// bitpacked dictionary index. Null rows use the sentinel index == dict
// size, so no separate null bitmap is needed.
//
// Payload (see api/proto/segment.proto):
//
//	u32 dict_len | zstd(dict blob) | u8 bit_width | u32 num_rows | packed
//
// dict blob:
//
//	keyword/text : u32 count | per entry (u32 len | bytes)
//	long/date/ip : u32 count | count * i64

func encodeDictBlock(ft meta.FieldType, data *BlockData) ([]byte, error) {
	switch ft {
	case meta.TypeKeyword, meta.TypeText:
		return encodeDictStringBlock(data)
	case meta.TypeLong, meta.TypeDate, meta.TypeIP:
		return encodeDictLongBlock(data)
	default:
		return nil, fmt.Errorf("segment: dict encoding not applicable to %s", ft)
	}
}

func encodeDictStringBlock(data *BlockData) ([]byte, error) {
	n := data.Rows
	dictSet := make(map[string]uint32, n/2+1)
	var order []string // first-seen order, sorted before emit
	for i := 0; i < n; i++ {
		if data.IsNull(i) {
			continue
		}
		s := data.strs[i]
		if _, ok := dictSet[s]; !ok {
			dictSet[s] = 0 // placeholder; real ids assigned after sort
			order = append(order, s)
		}
	}
	sort.Strings(order)
	for id, s := range order {
		dictSet[s] = uint32(id)
	}

	blob := appendU32LE(nil, uint32(len(order)))
	for _, s := range order {
		blob = appendU32LE(blob, uint32(len(s)))
		blob = append(blob, s...)
	}
	compressed, err := zstdCompress(blob)
	if err != nil {
		return nil, err
	}

	sentinel := uint64(len(order)) // null index when nulls exist
	indices := make([]uint64, n)
	for i := 0; i < n; i++ {
		if data.IsNull(i) {
			indices[i] = sentinel
			continue
		}
		indices[i] = uint64(dictSet[data.strs[i]])
	}
	maxIdx := uint64(0)
	if data.nullCount > 0 {
		maxIdx = sentinel
	} else if len(order) > 1 {
		maxIdx = uint64(len(order) - 1)
	}
	w := bitWidth(maxIdx)

	out := appendU32LE(nil, uint32(len(compressed)))
	out = append(out, compressed...)
	out = append(out, byte(w))
	out = appendU32LE(out, uint32(n))
	out = append(out, bitpackEncode(indices, w)...)
	return out, nil
}

func encodeDictLongBlock(data *BlockData) ([]byte, error) {
	n := data.Rows
	dictSet := make(map[int64]uint32, n/2+1)
	var order []int64
	for i := 0; i < n; i++ {
		if data.IsNull(i) {
			continue
		}
		v := data.longs[i]
		if _, ok := dictSet[v]; !ok {
			dictSet[v] = 0
			order = append(order, v)
		}
	}
	sort.Slice(order, func(i, j int) bool { return order[i] < order[j] })
	for id, v := range order {
		dictSet[v] = uint32(id)
	}

	blob := appendU32LE(nil, uint32(len(order)))
	for _, v := range order {
		blob = appendU64LE(blob, uint64(v))
	}
	compressed, err := zstdCompress(blob)
	if err != nil {
		return nil, err
	}

	sentinel := uint64(len(order))
	indices := make([]uint64, n)
	for i := 0; i < n; i++ {
		if data.IsNull(i) {
			indices[i] = sentinel
			continue
		}
		indices[i] = uint64(dictSet[data.longs[i]])
	}
	maxIdx := uint64(0)
	if data.nullCount > 0 {
		maxIdx = sentinel
	} else if len(order) > 1 {
		maxIdx = uint64(len(order) - 1)
	}
	w := bitWidth(maxIdx)

	out := appendU32LE(nil, uint32(len(compressed)))
	out = append(out, compressed...)
	out = append(out, byte(w))
	out = appendU32LE(out, uint32(n))
	out = append(out, bitpackEncode(indices, w)...)
	return out, nil
}

func decodeDictBlock(ft meta.FieldType, payload []byte, nrows int) (*BlockData, error) {
	if len(payload) < 9 { // dict_len(4) + bit_width(1) + num_rows(4) minimum
		return nil, fmt.Errorf("segment: truncated dict block")
	}
	dictLen := int(readU32LE(payload, 0))
	if 4+dictLen+5 > len(payload) {
		return nil, fmt.Errorf("segment: truncated dict dictionary")
	}
	blob, err := zstdDecompress(payload[4 : 4+dictLen])
	if err != nil {
		return nil, err
	}
	if len(blob) < 4 {
		return nil, fmt.Errorf("segment: corrupt dict blob")
	}
	count := int(readU32LE(blob, 0))
	off := 4

	// Pre-parse the dictionary entries into typed slices.
	var longDict []int64
	var strDict []string
	switch ft {
	case meta.TypeLong, meta.TypeDate, meta.TypeIP:
		if off+count*8 > len(blob) {
			return nil, fmt.Errorf("segment: truncated long dict entries")
		}
		longDict = make([]int64, count)
		for i := range longDict {
			longDict[i] = int64(readU64LE(blob, off+i*8))
		}
	case meta.TypeKeyword, meta.TypeText:
		strDict = make([]string, count)
		for i := range strDict {
			if off+4 > len(blob) {
				return nil, fmt.Errorf("segment: truncated string dict entry")
			}
			l := int(readU32LE(blob, off))
			off += 4
			if off+l > len(blob) {
				return nil, fmt.Errorf("segment: truncated string dict entry")
			}
			strDict[i] = string(blob[off : off+l])
			off += l
		}
	default:
		return nil, fmt.Errorf("segment: dict decoding not applicable to %s", ft)
	}

	wo := 4 + dictLen
	w := int(payload[wo])
	n := int(readU32LE(payload, wo+1))
	if n != nrows {
		return nil, fmt.Errorf("segment: dict block row count %d != expected %d", n, nrows)
	}
	indices := bitpackDecode(payload[wo+5:], n, w)

	data := newBlockData(ft, nrows)
	for _, idx := range indices {
		if idx == uint64(count) { // null sentinel
			data.appendNull()
			continue
		}
		if idx > uint64(count) {
			return nil, fmt.Errorf("segment: dict index %d out of range (count %d)", idx, count)
		}
		switch {
		case longDict != nil:
			data.longs = append(data.longs, longDict[idx])
		case strDict != nil:
			data.strs = append(data.strs, strDict[idx])
		}
		data.Rows++
	}
	return data, nil
}
