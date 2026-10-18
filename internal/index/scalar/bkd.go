package scalar

import (
	"context"
	"encoding/binary"
	"fmt"
	"sort"

	"github.com/RoaringBitmap/roaring"
	"github.com/unisearch-db/unisearch/internal/index"
)

func init() {
	index.RegisterIndex(index.KindBKD, func() index.Index { return &BKD{} })
}

// DefaultLeafSize is the BKD leaf fanout (§6.1: leaf = 1024 points).
const DefaultLeafSize = 1024

// Segment index file set (all little-endian), laid out under the kind's
// index directory (seg_xxx/bkd.idx/ in the full storage mapping):
//
//	bkd.meta    header: magic, version, dims, valueKind, n, leafSize, nodeCount
//	bkd.values  1D: n sorted int64 values (double columns stored in
//	            order-preserving encoded form)
//	bkd.rows    1D: n int32 row ids parallel to bkd.values
//	bkd.lats    2D: n encoded latitudes (leaf-ordered)
//	bkd.lons    2D: n encoded longitudes (leaf-ordered)
//	bkd.nodes   2D: tree nodes (range + bbox + children)
const (
	fileMeta  = "bkd.meta"
	fileVals  = "bkd.values"
	fileRows  = "bkd.rows"
	fileLats  = "bkd.lats"
	fileLons  = "bkd.lons"
	fileNodes = "bkd.nodes"
)

const (
	bkdMagic   uint32 = 0x31444B42 // "BKD1"
	bkdVersion uint8  = 1
	kindLong   uint8  = 0
	kindDouble uint8  = 1
)

// BKDConfig holds the tunable BKD parameters. The zero value is valid.
type BKDConfig struct {
	// LeafSize is the max points per leaf; 0 -> DefaultLeafSize.
	LeafSize int
}

func (c BKDConfig) resolve() BKDConfig {
	if c.LeafSize <= 0 {
		c.LeafSize = DefaultLeafSize
	}
	return c
}

// bkdStore is the built or loaded in-memory index data (M0 keeps it
// fully resident, mirroring the HNSW M0 approach). Immutable once
// built/loaded; safe for concurrent search.
type bkdStore struct {
	dims      int // 1 or 2
	valueKind uint8
	n         int
	leafSize  int
	values    []int64  // 1D: sorted ascending
	rows      []int32  // 1D: parallel to values
	tree      *geoTree // 2D
	minVal    int64    // 1D zone stats (MemEstimate/debug)
	maxVal    int64
}

// BKD is the index.Index implementation registered under
// index.KindBKD (§6.1). It carries the build configuration and, after
// Build/Merge, the built store; the searchable artifact handed to
// callers is bkdLoaded.
type BKD struct {
	cfg   BKDConfig
	store *bkdStore
}

// NewBKD returns a BKD index with the given build configuration.
func NewBKD(cfg BKDConfig) *BKD { return &BKD{cfg: cfg} }

// Kind implements index.Index.
func (ix *BKD) Kind() index.IndexKind { return index.KindBKD }

// Build implements index.Index: it streams the scalar column through
// the provider (narrow interface assertion per package doc) and writes
// the file set above. Column types: GeoPointColumn -> 2D geo index,
// LongColumn -> 1D long, DoubleColumn -> 1D double (order-preserving
// encoded).
func (ix *BKD) Build(ctx context.Context, col index.ColumnProvider, w index.IndexOutput) error {
	cfg := ix.cfg.resolve()
	if err := ctx.Err(); err != nil {
		return err
	}
	if geo, ok := col.(GeoPointColumn); ok {
		return ix.buildGeo(ctx, geo, cfg, w)
	}
	if lc, ok := col.(LongColumn); ok {
		return ix.buildLong(ctx, lc, cfg, w, kindLong, nil)
	}
	if dc, ok := col.(DoubleColumn); ok {
		return ix.buildLong(ctx, dc, cfg, w, kindDouble, doubleToSortable)
	}
	return fmt.Errorf("scalar: column %q does not expose a scalar row iterator "+
		"(need GeoPointColumn, LongColumn or DoubleColumn)", col.Field())
}

// buildLong builds the 1D sorted-array index (§6.1: 1D degenerates to
// a sorted array with binary search — the Lucene leaf optimization
// applied to the whole column). enc converts the column value to the
// sorted long domain (nil for native long columns).
func (ix *BKD) buildLong(ctx context.Context, col index.ColumnProvider, cfg BKDConfig, w index.IndexOutput, valueKind uint8, enc func(float64) int64) error {
	n := int(col.Rows())
	pairs := make([]sortPair, n)
	i := 0
	collect := func(rowID int32, v int64) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if int(rowID) != i {
			return fmt.Errorf("scalar: column %q yielded row %d at position %d", col.Field(), rowID, i)
		}
		pairs[i] = sortPair{v: v, row: rowID}
		i++
		return nil
	}
	var err error
	if enc == nil {
		err = col.(LongColumn).ForEachLong(collect)
	} else {
		err = col.(DoubleColumn).ForEachDouble(func(rowID int32, f float64) error {
			if isNaN(f) {
				return fmt.Errorf("scalar: NaN double at row %d of column %q", rowID, col.Field())
			}
			return collect(rowID, enc(f))
		})
	}
	if err != nil {
		return err
	}
	if i != n {
		return fmt.Errorf("scalar: column %q yielded %d rows, header says %d", col.Field(), i, n)
	}
	sortSlice(pairs)
	st := &bkdStore{
		dims:      1,
		valueKind: valueKind,
		n:         n,
		leafSize:  cfg.LeafSize,
		values:    make([]int64, n),
		rows:      make([]int32, n),
	}
	for j, p := range pairs {
		st.values[j] = p.v
		st.rows[j] = p.row
	}
	if n > 0 {
		st.minVal, st.maxVal = st.values[0], st.values[n-1]
	}
	if err := writeBKD(w, st); err != nil {
		return err
	}
	ix.store = st
	return nil
}

// sortPair is one (value, row) of the 1D array, ordered by (v, row).
type sortPair struct {
	v   int64
	row int32
}

func sortSlice(p []sortPair) {
	sort.Slice(p, func(a, b int) bool {
		if p[a].v != p[b].v {
			return p[a].v < p[b].v
		}
		return p[a].row < p[b].row
	})
}

func (ix *BKD) buildGeo(ctx context.Context, col GeoPointColumn, cfg BKDConfig, w index.IndexOutput) error {
	n := int(col.Rows())
	pts := make([]geoPoint, n)
	i := 0
	err := col.ForEachGeoPoint(func(rowID int32, lat, lon float64) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if int(rowID) != i {
			return fmt.Errorf("scalar: column %q yielded row %d at position %d", col.Field(), rowID, i)
		}
		if isNaN(lat) || isNaN(lon) || lat < -90 || lat > 90 || lon < -180 || lon > 180 {
			return fmt.Errorf("scalar: geo_point (%v,%v) out of range at row %d", lat, lon, rowID)
		}
		pts[i] = geoPoint{lat: encodeLat(lat), lon: encodeLon(lon), row: rowID}
		i++
		return nil
	})
	if err != nil {
		return err
	}
	if i != n {
		return fmt.Errorf("scalar: column %q yielded %d points, header says %d", col.Field(), i, n)
	}
	tree, err := buildGeoTree(pts, cfg.LeafSize)
	if err != nil {
		return err
	}
	st := &bkdStore{dims: 2, n: n, leafSize: cfg.LeafSize, tree: tree}
	if err := writeBKD(w, st); err != nil {
		return err
	}
	ix.store = st
	return nil
}

// Load implements index.Index: reads the file set back into an
// immutable, concurrency-safe bkdLoaded.
func (ix *BKD) Load(r index.IndexInput) (index.LoadedIndex, error) {
	st, err := readBKD(r)
	if err != nil {
		return nil, err
	}
	return &bkdLoaded{st: st}, nil
}

// Merge implements index.Index: BKD semantics are "rebuild" (§1.1).
// The point sets of this index (when built) and every other loaded
// index are concatenated in order and rebuilt; new row ids are assigned
// by position, and the storage layer owns the old-to-new row mapping
// (same contract as the HNSW merge).
func (ix *BKD) Merge(ctx context.Context, others []index.LoadedIndex, w index.IndexOutput) error {
	var src []*bkdStore
	if ix.store != nil {
		src = append(src, ix.store)
	}
	for _, o := range others {
		li, ok := o.(*bkdLoaded)
		if !ok {
			return fmt.Errorf("scalar: cannot merge foreign loaded index %T", o)
		}
		src = append(src, li.st)
	}
	if len(src) == 0 {
		return fmt.Errorf("scalar: merge requires at least one source index")
	}
	dims := src[0].dims
	valueKind := src[0].valueKind
	leafSize := src[0].leafSize
	total := 0
	for _, s := range src {
		if s.dims != dims {
			return fmt.Errorf("scalar: dim mismatch in merge: %d vs %d", s.dims, dims)
		}
		if s.valueKind != valueKind {
			return fmt.Errorf("scalar: value kind mismatch in merge")
		}
		total += s.n
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	st := &bkdStore{dims: dims, valueKind: valueKind, n: total, leafSize: leafSize}
	if dims == 1 {
		// Row ids are reassigned into the concatenated row space (the
		// storage layer owns the old-to-new mapping, same contract as
		// the HNSW merge): source si's old row r becomes base+r.
		pairs := make([]sortPair, 0, total)
		base := 0
		for _, s := range src {
			for j := 0; j < s.n; j++ {
				pairs = append(pairs, sortPair{v: s.values[j], row: int32(base) + s.rows[j]})
			}
			base += s.n
		}
		sortSlice(pairs)
		st.values = make([]int64, total)
		st.rows = make([]int32, total)
		for j, p := range pairs {
			st.values[j] = p.v
			st.rows[j] = p.row
		}
		if total > 0 {
			st.minVal, st.maxVal = st.values[0], st.values[total-1]
		}
	} else {
		// Same positional row reassignment as the 1D branch.
		pts := make([]geoPoint, 0, total)
		base := 0
		for _, s := range src {
			for _, p := range s.tree.pts {
				p.row = int32(base) + p.row
				pts = append(pts, p)
			}
			base += s.n
		}
		tree, err := buildGeoTree(pts, leafSize)
		if err != nil {
			return err
		}
		st.tree = tree
	}
	if err := writeBKD(w, st); err != nil {
		return err
	}
	ix.store = st
	return nil
}

// Quantize implements index.Index: scalar indexes have no quantization
// tiers.
func (ix *BKD) Quantize(ctx context.Context, to index.QuantLevel) error {
	return fmt.Errorf("%w: bkd has no quantization (level %d)", index.ErrUnsupported, int(to))
}

// MemEstimate implements index.Index.
func (ix *BKD) MemEstimate() int64 {
	if ix.store == nil {
		return 0
	}
	return ix.store.memBytes()
}

func (s *bkdStore) memBytes() int64 {
	if s == nil {
		return 0
	}
	if s.dims == 1 {
		return int64(12*s.n) + 48
	}
	return int64(20*s.n) + int64(len(s.tree.nodes))*int64(2*4+2*8+2*4) + 64
}

// bkdLoaded is the searchable handle over one built BKD index. It is
// immutable and safe for concurrent use. It implements the typed
// Searcher surfaces (RangeSearcher / GeoSearcher) that the filter
// operator consumes, plus the generic index.LoadedIndex.Search trait
// via the encoded-query wire form.
type bkdLoaded struct {
	st *bkdStore
}

var (
	_ index.LoadedIndex = (*bkdLoaded)(nil)
	_ RangeSearcher     = (*bkdLoaded)(nil)
	_ GeoSearcher       = (*bkdLoaded)(nil)
)

// SearchRange implements RangeSearcher: half-open [Min, Max) over the
// sorted 1D array via two binary searches; output is a roaring bitmap
// for filter intersection. Rows are emitted in ascending order.
func (l *bkdLoaded) SearchRange(ctx context.Context, q RangeQuery, budget index.SearchBudget) (*RangeResult, error) {
	stats := Stats{}
	out := roaring.New()
	if l.st.dims != 1 {
		return nil, fmt.Errorf("scalar: range query over %dD index", l.st.dims)
	}
	n := len(l.st.values)
	if n == 0 || q.Min > q.Max {
		return &RangeResult{Rows: out, Stats: stats}, nil
	}
	lo := lowerBound(l.st.values, q.Min, &stats)
	hi := lowerBound(l.st.values, q.Max, &stats)
	for i := lo; i < hi; i++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		stats.RowsTouched++
		if err := touchBudget(budget, &stats); err != nil {
			return nil, err
		}
		out.Add(uint32(l.st.rows[i]))
	}
	return &RangeResult{Rows: out, Stats: stats}, nil
}

// SearchGeoDistance implements GeoSearcher: bounding-box pruned tree
// descent plus exact haversine in the leaves (§6.1).
func (l *bkdLoaded) SearchGeoDistance(ctx context.Context, q GeoDistanceQuery, budget index.SearchBudget) (*GeoResult, error) {
	if l.st.dims != 2 {
		return nil, fmt.Errorf("scalar: geo query over %dD index", l.st.dims)
	}
	stats := Stats{}
	rows, err := l.st.tree.searchDistance(ctx, q, budget, &stats)
	if err != nil {
		return nil, err
	}
	return &GeoResult{Rows: rows, Stats: stats}, nil
}

// Search implements index.LoadedIndex: the encoded query (see
// query.go) selects the typed path; hits carry score 1.0 and rows are
// capped by q.K when positive. q.Filter drops rows post-search.
func (l *bkdLoaded) Search(ctx context.Context, q index.Query, budget index.SearchBudget) (*index.SearchResult, error) {
	enc, err := decodeQuery(q.Term)
	if err != nil {
		return nil, err
	}
	var rows *roaring.Bitmap
	switch r := enc.(type) {
	case RangeQuery:
		res, err := l.SearchRange(ctx, r, budget)
		if err != nil {
			return nil, err
		}
		rows = res.Rows
	case GeoDistanceQuery:
		res, err := l.SearchGeoDistance(ctx, r, budget)
		if err != nil {
			return nil, err
		}
		rows = res.Rows
	default:
		return nil, fmt.Errorf("scalar: unexpected query type %T", enc)
	}
	return hitsFromBitmap(rows, q.K, q.Filter), nil
}

// Close implements index.LoadedIndex. M0 stores are in-memory.
func (l *bkdLoaded) Close() error { return nil }

// lowerBound returns the first index with values[i] >= target, probing
// the sorted array (each probe counts as a touched row in stats).
func lowerBound(values []int64, target int64, stats *Stats) int {
	lo, hi := 0, len(values)
	for lo < hi {
		mid := int(uint(lo+hi) >> 1)
		stats.RowsTouched++
		if values[mid] < target {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	return lo
}

// hitsFromBitmap materializes a search result from a row bitmap,
// honoring K (cap, when positive) and a post-filter predicate.
func hitsFromBitmap(rows *roaring.Bitmap, k int, filter func(int32) bool) *index.SearchResult {
	out := &index.SearchResult{}
	it := rows.Iterator()
	for it.HasNext() {
		row := int32(it.Next())
		if filter != nil && !filter(row) {
			continue
		}
		out.Hits = append(out.Hits, index.SearchHit{RowID: row, Score: 1})
		if k > 0 && len(out.Hits) >= k {
			break
		}
	}
	return out
}

// writeBKD serializes a store into the file set (all little-endian).
func writeBKD(w index.IndexOutput, st *bkdStore) error {
	f, err := w.Create(fileMeta)
	if err != nil {
		return fmt.Errorf("scalar: create %s: %w", fileMeta, err)
	}
	nodeCount := 0
	if st.dims == 2 {
		nodeCount = len(st.tree.nodes)
	}
	hdr := []any{bkdMagic, bkdVersion, uint8(st.dims), st.valueKind,
		int32(st.n), int32(st.leafSize), int32(nodeCount)}
	for _, v := range hdr {
		if err := binary.Write(f, binary.LittleEndian, v); err != nil {
			f.Close()
			return fmt.Errorf("scalar: write %s header: %w", fileMeta, err)
		}
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("scalar: close %s: %w", fileMeta, err)
	}

	if st.dims == 1 {
		if err := writeI64s(w, fileVals, st.values); err != nil {
			return err
		}
		if err := writeI32s(w, fileRows, st.rows); err != nil {
			return err
		}
		return nil
	}
	lats := make([]int64, len(st.tree.pts))
	lons := make([]int64, len(st.tree.pts))
	for i, p := range st.tree.pts {
		lats[i], lons[i] = p.lat, p.lon
	}
	if err := writeI64s(w, fileLats, lats); err != nil {
		return err
	}
	if err := writeI64s(w, fileLons, lons); err != nil {
		return err
	}
	if err := writeI32s(w, fileRows, st.rowIDs()); err != nil {
		return err
	}
	// Nodes: lo, hi, left, right (i32 x4) + bbox (i64 x4).
	f, err = w.Create(fileNodes)
	if err != nil {
		return fmt.Errorf("scalar: create %s: %w", fileNodes, err)
	}
	for _, n := range st.tree.nodes {
		for _, v := range []any{n.lo, n.hi, n.left, n.right, n.minLat, n.maxLat, n.minLon, n.maxLon} {
			if err := binary.Write(f, binary.LittleEndian, v); err != nil {
				f.Close()
				return fmt.Errorf("scalar: write %s: %w", fileNodes, err)
			}
		}
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("scalar: close %s: %w", fileNodes, err)
	}
	return nil
}

// rowIDs reconstructs the row-parallel slice of the 2D point array.
func (s *bkdStore) rowIDs() []int32 {
	out := make([]int32, len(s.tree.pts))
	for i, p := range s.tree.pts {
		out[i] = p.row
	}
	return out
}

// readBKD is the inverse of writeBKD.
func readBKD(r index.IndexInput) (*bkdStore, error) {
	f, err := r.Open(fileMeta)
	if err != nil {
		return nil, fmt.Errorf("scalar: open %s: %w", fileMeta, err)
	}
	var magic uint32
	var version, dims, valueKind uint8
	var n, leafSize, nodeCount int32
	// Header packs as u32 magic + u8 version/dims/valueKind + i32 x3;
	// read it piecewise (matches the writeBKD order).
	if err := binary.Read(f, binary.LittleEndian, &magic); err != nil {
		return nil, fmt.Errorf("scalar: read %s magic: %w", fileMeta, err)
	}
	if err := binary.Read(f, binary.LittleEndian, &version); err != nil {
		return nil, fmt.Errorf("scalar: read %s version: %w", fileMeta, err)
	}
	if err := binary.Read(f, binary.LittleEndian, &dims); err != nil {
		return nil, fmt.Errorf("scalar: read %s dims: %w", fileMeta, err)
	}
	if err := binary.Read(f, binary.LittleEndian, &valueKind); err != nil {
		return nil, fmt.Errorf("scalar: read %s valueKind: %w", fileMeta, err)
	}
	for _, v := range []any{&n, &leafSize, &nodeCount} {
		if err := binary.Read(f, binary.LittleEndian, v); err != nil {
			return nil, fmt.Errorf("scalar: read %s header: %w", fileMeta, err)
		}
	}
	if err := f.Close(); err != nil {
		return nil, fmt.Errorf("scalar: close %s: %w", fileMeta, err)
	}
	if magic != bkdMagic || version != bkdVersion {
		return nil, fmt.Errorf("scalar: bad bkd header (magic %#x version %d)", magic, version)
	}
	if dims != 1 && dims != 2 {
		return nil, fmt.Errorf("scalar: bad bkd dims %d", dims)
	}
	if n < 0 || leafSize <= 0 || nodeCount < 0 {
		return nil, fmt.Errorf("scalar: corrupt bkd header (n=%d leaf=%d nodes=%d)", n, leafSize, nodeCount)
	}
	st := &bkdStore{dims: int(dims), valueKind: valueKind, n: int(n), leafSize: int(leafSize)}
	if dims == 1 {
		if st.values, err = readI64s(r, fileVals, int(n)); err != nil {
			return nil, err
		}
		if st.rows, err = readI32s(r, fileRows, int(n)); err != nil {
			return nil, err
		}
		if n > 0 {
			st.minVal, st.maxVal = st.values[0], st.values[n-1]
			if !sort.SliceIsSorted(st.values, func(a, b int) bool { return st.values[a] < st.values[b] }) {
				return nil, fmt.Errorf("scalar: bkd.values not sorted")
			}
		}
		return st, nil
	}
	lats, err := readI64s(r, fileLats, int(n))
	if err != nil {
		return nil, err
	}
	lons, err := readI64s(r, fileLons, int(n))
	if err != nil {
		return nil, err
	}
	rows, err := readI32s(r, fileRows, int(n))
	if err != nil {
		return nil, err
	}
	if nodeCount == 0 && n > 0 {
		return nil, fmt.Errorf("scalar: bkd 2D with %d points has no nodes", n)
	}
	pts := make([]geoPoint, n)
	for i := range pts {
		pts[i] = geoPoint{lat: lats[i], lon: lons[i], row: rows[i]}
	}
	fn, err := r.Open(fileNodes)
	if err != nil {
		return nil, fmt.Errorf("scalar: open %s: %w", fileNodes, err)
	}
	defer fn.Close()
	nodes := make([]geoNode, nodeCount)
	for i := range nodes {
		for _, v := range []any{&nodes[i].lo, &nodes[i].hi, &nodes[i].left, &nodes[i].right,
			&nodes[i].minLat, &nodes[i].maxLat, &nodes[i].minLon, &nodes[i].maxLon} {
			if err := binary.Read(fn, binary.LittleEndian, v); err != nil {
				return nil, fmt.Errorf("scalar: read %s entry %d: %w", fileNodes, i, err)
			}
		}
	}
	st.tree = &geoTree{pts: pts, nodes: nodes}
	return st, nil
}

func writeI64s(w index.IndexOutput, name string, v []int64) error {
	f, err := w.Create(name)
	if err != nil {
		return fmt.Errorf("scalar: create %s: %w", name, err)
	}
	if err := binary.Write(f, binary.LittleEndian, v); err != nil {
		f.Close()
		return fmt.Errorf("scalar: write %s: %w", name, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("scalar: close %s: %w", name, err)
	}
	return nil
}

func writeI32s(w index.IndexOutput, name string, v []int32) error {
	f, err := w.Create(name)
	if err != nil {
		return fmt.Errorf("scalar: create %s: %w", name, err)
	}
	if err := binary.Write(f, binary.LittleEndian, v); err != nil {
		f.Close()
		return fmt.Errorf("scalar: write %s: %w", name, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("scalar: close %s: %w", name, err)
	}
	return nil
}

func readI64s(r index.IndexInput, name string, n int) ([]int64, error) {
	f, err := r.Open(name)
	if err != nil {
		return nil, fmt.Errorf("scalar: open %s: %w", name, err)
	}
	defer f.Close()
	v := make([]int64, n)
	if err := binary.Read(f, binary.LittleEndian, v); err != nil {
		return nil, fmt.Errorf("scalar: read %s: %w", name, err)
	}
	return v, nil
}

func readI32s(r index.IndexInput, name string, n int) ([]int32, error) {
	f, err := r.Open(name)
	if err != nil {
		return nil, fmt.Errorf("scalar: open %s: %w", name, err)
	}
	defer f.Close()
	v := make([]int32, n)
	if err := binary.Read(f, binary.LittleEndian, v); err != nil {
		return nil, fmt.Errorf("scalar: read %s: %w", name, err)
	}
	return v, nil
}
