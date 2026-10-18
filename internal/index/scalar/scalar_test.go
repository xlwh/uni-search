package scalar

import (
	"context"
	"fmt"
	"io"
	"math"
	"math/rand/v2"
	"os"
	"path/filepath"
	"testing"

	"github.com/RoaringBitmap/roaring"
	"github.com/stretchr/testify/require"

	"github.com/unisearch-db/unisearch/internal/index"
	"github.com/unisearch-db/unisearch/pkg/types"
)

// ---------------------------------------------------------------------------
// Fixtures: scalar column providers (the narrow interfaces the storage
// engine will implement on col_*.bin readers) and temp-dir
// IndexOutput/IndexInput standing in for the segment index directory.
// ---------------------------------------------------------------------------

type baseCol struct {
	field string
	rows  int
}

func (c *baseCol) Field() string { return c.field }
func (c *baseCol) Rows() int64   { return int64(c.rows) }
func (c *baseCol) Dim() int      { return 0 }
func (c *baseCol) ForEachVector(func(int32, []float32) error) error {
	return fmt.Errorf("scalar column %q has no vectors", c.field)
}

type longCol struct {
	baseCol
	vals []int64
}

func (c *longCol) ForEachLong(fn func(rowID int32, v int64) error) error {
	for i, v := range c.vals {
		if err := fn(int32(i), v); err != nil {
			return err
		}
	}
	return nil
}

type doubleCol struct {
	baseCol
	vals []float64
}

func (c *doubleCol) ForEachDouble(fn func(rowID int32, v float64) error) error {
	for i, v := range c.vals {
		if err := fn(int32(i), v); err != nil {
			return err
		}
	}
	return nil
}

type geoCol struct {
	baseCol
	lats, lons []float64
}

func (c *geoCol) ForEachGeoPoint(fn func(rowID int32, lat, lon float64) error) error {
	for i := range c.lats {
		if err := fn(int32(i), c.lats[i], c.lons[i]); err != nil {
			return err
		}
	}
	return nil
}

type bytesCol struct {
	baseCol
	vals []string
}

func (c *bytesCol) ForEachValue(fn func(rowID int32, v []byte) error) error {
	for i, v := range c.vals {
		if err := fn(int32(i), []byte(v)); err != nil {
			return err
		}
	}
	return nil
}

// dirOutput / dirInput implement the registry IO traits over a real
// temporary directory: Build must survive an actual write + Load an
// actual read (R12 roundtrip requirement).
type dirOutput struct{ dir string }

func (d *dirOutput) Create(name string) (io.WriteCloser, error) {
	return os.Create(filepath.Join(d.dir, name))
}

type dirInput struct{ dir string }

func (d *dirInput) Open(name string) (io.ReadSeekCloser, error) {
	return os.Open(filepath.Join(d.dir, name))
}

// buildAndLoad drives the index.Index trait end to end into a fresh
// temp directory and returns the loaded (searchable) handle.
func buildAndLoad(t *testing.T, ix index.Index, col index.ColumnProvider) (index.LoadedIndex, string) {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, ix.Build(context.Background(), col, &dirOutput{dir}))
	li, err := ix.Load(&dirInput{dir})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, li.Close()) })
	return li, dir
}

func bitmapRows(t *testing.T, b *roaring.Bitmap) []uint32 {
	t.Helper()
	if b.IsEmpty() {
		return nil // normalize: empty results compare equal to nil wants
	}
	out := make([]uint32, 0, b.GetCardinality())
	it := b.Iterator()
	for it.HasNext() {
		out = append(out, it.Next())
	}
	return out
}

func hitRows(hits []index.SearchHit) []int32 {
	out := make([]int32, 0, len(hits))
	for _, h := range hits {
		out = append(out, h.RowID)
	}
	return out
}

// testHaversine is an independent per-row distance implementation for
// the linear-scan cross-checks (atan2 form, deliberately coded apart
// from the package's asin kernel; the Earth radius constant is shared
// domain knowledge, not implementation detail).
func testHaversine(lat1, lon1, lat2, lon2 float64) float64 {
	const R = 6371008.8
	rad := math.Pi / 180
	dp := (lat2 - lat1) * rad
	dl := (lon2 - lon1) * rad
	a := math.Sin(dp/2)*math.Sin(dp/2) + math.Cos(lat1*rad)*math.Cos(lat2*rad)*math.Sin(dl/2)*math.Sin(dl/2)
	return 2 * R * math.Atan2(math.Sqrt(a), math.Sqrt(1-a))
}

// ---------------------------------------------------------------------------
// R12 scenario 1: 1D range filter vs linear scan, with pruning counters.
// ---------------------------------------------------------------------------

// TestR12_RangeFilter runs long range queries against a linear scan:
// row sets must be identical, and the counter must show the sorted-
// array binary search touching no more rows than the scan (strictly
// fewer on selective ranges — that is the pruning BKD contributes).
func TestR12_RangeFilter(t *testing.T) {
	const n = 5000
	r := rand.New(rand.NewPCG(1, 2))
	vals := make([]int64, n)
	for i := range vals {
		vals[i] = r.Int64N(1000)
	}
	col := &longCol{baseCol: baseCol{field: "count", rows: n}, vals: vals}
	ix := NewBKD(BKDConfig{})
	require.Equal(t, index.KindBKD, ix.Kind())
	li, _ := buildAndLoad(t, ix, col)
	rs, ok := li.(RangeSearcher)
	require.True(t, ok, "loaded BKD must expose RangeSearcher for T15")

	// The required R12 case first: [100, 200).
	q := RangeQuery{Min: 100, Max: 200}
	var want []uint32
	linearTouched := int64(0)
	for row, v := range vals {
		linearTouched++ // the linear scan touches every row once
		if v >= q.Min && v < q.Max {
			want = append(want, uint32(row))
		}
	}
	res, err := rs.SearchRange(context.Background(), q, index.SearchBudget{})
	require.NoError(t, err)
	require.Equal(t, want, bitmapRows(t, res.Rows),
		"range [100,200) must match the linear scan exactly")
	require.NotEmpty(t, want)
	// Counter verification: BKD pruned rows — binary search probes plus
	// emitted rows never exceed the linear scan's touched rows, and a
	// selective range touches strictly fewer.
	require.LessOrEqual(t, res.Stats.RowsTouched, linearTouched)
	require.Less(t, res.Stats.RowsTouched, linearTouched,
		"selective range must prune: touched %d, linear %d", res.Stats.RowsTouched, linearTouched)

	// Additional ranges: exactness for empty, open-ended and
	// full-coverage bounds.
	for _, q := range []RangeQuery{
		{Min: 700, Max: 300},           // inverted -> empty
		{Min: 999, Max: math.MaxInt64}, // open high end
		{Min: math.MinInt64, Max: 0},   // open low end (no negatives here)
		{Min: 0, Max: 1000},            // full coverage
		{Min: 500, Max: 501},           // single-value slice
	} {
		var want []uint32
		for row, v := range vals {
			if v >= q.Min && v < q.Max {
				want = append(want, uint32(row))
			}
		}
		res, err := rs.SearchRange(context.Background(), q, index.SearchBudget{})
		require.NoError(t, err)
		require.Equal(t, want, bitmapRows(t, res.Rows), "range %+v", q)
	}

	// The generic trait: encoded range query through
	// index.LoadedIndex.Search, with K capping and row filter.
	trait, err := li.Search(context.Background(),
		index.Query{Term: RangeQuery{Min: 100, Max: 200}.Encode()}, index.SearchBudget{})
	require.NoError(t, err)
	require.Equal(t, int32s(want), hitRows(trait.Hits))
	capped, err := li.Search(context.Background(),
		index.Query{Term: RangeQuery{Min: 100, Max: 200}.Encode(), K: 10}, index.SearchBudget{})
	require.NoError(t, err)
	require.Len(t, capped.Hits, 10)
	filtered, err := li.Search(context.Background(),
		index.Query{Term: RangeQuery{Min: 100, Max: 200}.Encode(),
			Filter: func(row int32) bool { return row%2 == 0 }}, index.SearchBudget{})
	require.NoError(t, err)
	var wantEven []int32
	for _, u := range want {
		if u%2 == 0 {
			wantEven = append(wantEven, int32(u))
		}
	}
	require.Equal(t, wantEven, hitRows(filtered.Hits))
}

func int32s(u []uint32) []int32 {
	out := make([]int32, len(u))
	for i, v := range u {
		out[i] = int32(v)
	}
	return out
}

// TestR12_DoubleRange cross-checks the double column path
// (order-preserving encoded into the same sorted 1D array) against a
// linear scan with raw float comparisons.
func TestR12_DoubleRange(t *testing.T) {
	const n = 2000
	r := rand.New(rand.NewPCG(9, 10))
	vals := make([]float64, n)
	for i := range vals {
		vals[i] = r.Float64() * 1000
	}
	col := &doubleCol{baseCol: baseCol{field: "score", rows: n}, vals: vals}
	ix := NewBKD(BKDConfig{})
	li, _ := buildAndLoad(t, ix, col)
	rs := li.(RangeSearcher)

	q := NewDoubleRangeQuery(100.5, 200.25)
	var want []uint32
	for row, v := range vals {
		if v >= 100.5 && v < 200.25 {
			want = append(want, uint32(row))
		}
	}
	res, err := rs.SearchRange(context.Background(), q, index.SearchBudget{})
	require.NoError(t, err)
	require.Equal(t, want, bitmapRows(t, res.Rows))

	// Order-preserving encoding sanity: exact bit roundtrip and
	// monotonicity across the sign boundary.
	for _, f := range []float64{-1e18, -1, -1e-300, 0, 1e-300, 1, 100.5, 1e18} {
		require.Equal(t, f, sortableToDouble(doubleToSortable(f)))
	}
	require.Less(t, doubleToSortable(-1), doubleToSortable(0))
	require.Less(t, doubleToSortable(0), doubleToSortable(1))
	require.Less(t, doubleToSortable(-2), doubleToSortable(-1))

	// Open-ended double range over the whole column.
	res, err = rs.SearchRange(context.Background(),
		NewDoubleRangeQuery(-1, 1001), index.SearchBudget{})
	require.NoError(t, err)
	require.Equal(t, uint64(n), res.Rows.GetCardinality())
}

// ---------------------------------------------------------------------------
// R12 scenario 2: geo distance vs per-row haversine, with pruning.
// ---------------------------------------------------------------------------

// TestR12_GeoDistance builds a geo_point BKD over far-apart clusters and
// cross-checks distance queries against an independent per-row
// haversine linear scan; the counters must show subtree pruning.
func TestR12_GeoDistance(t *testing.T) {
	centers := [][2]float64{
		{39.90, 116.40},  // Beijing
		{35.68, 139.69},  // Tokyo
		{37.77, -122.42}, // San Francisco
		{51.51, -0.13},   // London
		{-33.87, 151.21}, // Sydney
		{52.52, 13.40},   // Berlin
		{55.75, 37.62},   // Moscow
		{-23.55, -46.63}, // Sao Paulo
	}
	const per = 600
	n := len(centers) * per
	r := rand.New(rand.NewPCG(3, 4))
	lats := make([]float64, n)
	lons := make([]float64, n)
	for i := 0; i < n; i++ {
		c := centers[i/per]
		lats[i] = c[0] + (r.Float64()-0.5)*6 // +/- 3 degrees
		lons[i] = c[1] + (r.Float64()-0.5)*6
	}
	col := &geoCol{baseCol: baseCol{field: "location", rows: n}, lats: lats, lons: lons}
	ix := NewBKD(BKDConfig{LeafSize: 128})
	li, _ := buildAndLoad(t, ix, col)
	gs, ok := li.(GeoSearcher)
	require.True(t, ok, "loaded geo BKD must expose GeoSearcher for T15")

	check := func(t *testing.T, lat, lon, radius float64) (Stats, []uint32) {
		t.Helper()
		var want []uint32
		for row := 0; row < n; row++ {
			if testHaversine(lat, lon, lats[row], lons[row]) <= radius {
				want = append(want, uint32(row))
			}
		}
		res, err := gs.SearchGeoDistance(context.Background(),
			GeoDistanceQuery{Lat: lat, Lon: lon, RadiusM: radius}, index.SearchBudget{})
		require.NoError(t, err)
		require.Equal(t, want, bitmapRows(t, res.Rows),
			"geo distance (%v,%v r=%v) must match per-row haversine exactly", lat, lon, radius)
		require.Greater(t, res.Stats.NodesVisited, int32(0))
		return res.Stats, want
	}

	// Selective query inside one cluster: pruning must keep the
	// distance computations well below the linear scan's n.
	stats, want := check(t, 39.90, 116.40, 150_000)
	require.NotEmpty(t, want)
	require.Less(t, stats.DistComps, int64(n)/4,
		"bbox pruning must skip far clusters: %d dist comps for n=%d", stats.DistComps, n)
	require.LessOrEqual(t, stats.RowsTouched, int64(n))

	// Mid-size and large radii still exact.
	check(t, 37.77, -122.42, 300_000)
	check(t, 51.51, -0.13, 2_000_000) // reaches Berlin/Moscow clusters
	check(t, 0, 0, 20_000_000)        // near-hemisphere: pole rule path

	// Radius 0 matches only a point decoding exactly onto the center.
	tiny := &geoCol{baseCol: baseCol{field: "pin", rows: 3},
		lats: []float64{10, 10, 10.1}, lons: []float64{20, 20.5, 20}}
	li2, _ := buildAndLoad(t, NewBKD(BKDConfig{}), tiny)
	res, err := li2.(GeoSearcher).SearchGeoDistance(context.Background(),
		GeoDistanceQuery{Lat: 10, Lon: 20, RadiusM: 0}, index.SearchBudget{})
	require.NoError(t, err)
	require.Equal(t, []uint32{0}, bitmapRows(t, res.Rows))

	// The generic trait: encoded geo query through Search.
	trait, err := li.Search(context.Background(),
		index.Query{Term: GeoDistanceQuery{Lat: 39.90, Lon: 116.40, RadiusM: 150_000}.Encode()},
		index.SearchBudget{})
	require.NoError(t, err)
	require.NotEmpty(t, trait.Hits)
	_, want150 := check(t, 39.90, 116.40, 150_000)
	require.Equal(t, int32s(want150), hitRows(trait.Hits))
}

// TestR12_GeoCrossDateline verifies the antimeridian-wrap MBR: points
// on both sides of +-180 degrees must match exactly as the per-row
// haversine ground truth says.
func TestR12_GeoCrossDateline(t *testing.T) {
	const n = 400
	r := rand.New(rand.NewPCG(7, 8))
	lats := make([]float64, n)
	lons := make([]float64, n)
	for i := range lats {
		lats[i] = (r.Float64() - 0.5) * 4 // +/- 2 degrees
		lon := 179.0 + r.Float64()*2.5    // crosses 180
		if lon >= 180 {
			lon -= 360
		}
		lons[i] = lon
	}
	// One point exactly on the far side of the line.
	lats[0], lons[0] = 0.5, -179.9
	col := &geoCol{baseCol: baseCol{field: "dateline", rows: n}, lats: lats, lons: lons}
	ix := NewBKD(BKDConfig{LeafSize: 64})
	li, _ := buildAndLoad(t, ix, col)
	gs := li.(GeoSearcher)

	q := GeoDistanceQuery{Lat: 0.5, Lon: 179.8, RadiusM: 200_000}
	var want []uint32
	for row := 0; row < n; row++ {
		if testHaversine(q.Lat, q.Lon, lats[row], lons[row]) <= q.RadiusM {
			want = append(want, uint32(row))
		}
	}
	require.Contains(t, want, uint32(0), "wrapped point must be in the ground truth")
	res, err := gs.SearchGeoDistance(context.Background(), q, index.SearchBudget{})
	require.NoError(t, err)
	require.Equal(t, want, bitmapRows(t, res.Rows),
		"cross-dateline circle must match per-row haversine exactly")
}

// ---------------------------------------------------------------------------
// Bitmap index: auto-enable rule, term lookup vs linear scan, bloom.
// ---------------------------------------------------------------------------

// TestR12_BitmapAutoEnable asserts the §6.3 rule and cross-checks term
// lookups of a low-cardinality column against a linear scan.
func TestR12_BitmapAutoEnable(t *testing.T) {
	// The rule itself: strictly below the limit.
	require.True(t, AutoEnable(0))
	require.True(t, AutoEnable(4095))
	require.False(t, AutoEnable(4096))
	require.False(t, AutoEnable(1<<20))
	require.Equal(t, 4096, BitmapCardinalityLimit)

	const n, card = 3000, 12
	r := rand.New(rand.NewPCG(5, 6))
	vals := make([]string, n)
	for i := range vals {
		vals[i] = fmt.Sprintf("status-%d", r.IntN(card))
	}
	col := &bytesCol{baseCol: baseCol{field: "status", rows: n}, vals: vals}
	ix := NewBitmap(BitmapConfig{})
	require.Equal(t, index.KindBitmap, ix.Kind())
	li, dir := buildAndLoad(t, ix, col)
	for _, f := range []string{fileMetaB, fileDict, filePosts} {
		require.FileExists(t, filepath.Join(dir, f), "Build must write %s", f)
	}

	bl, ok := li.(*bitmapLoaded)
	require.True(t, ok)
	require.True(t, bl.AutoEnabled(),
		"cardinality %d < %d must auto-enable the bitmap index", card, BitmapCardinalityLimit)
	require.Equal(t, card, bl.Cardinality())

	ts, ok := li.(TermSearcher)
	require.True(t, ok, "loaded bitmap index must expose TermSearcher for T15")

	// Linear-scan cross-check for every distinct value.
	byTerm := make(map[string][]uint32)
	for row, v := range vals {
		byTerm[v] = append(byTerm[v], uint32(row))
	}
	require.Len(t, byTerm, card)
	for term, want := range byTerm {
		got, err := ts.SearchTerm(context.Background(), []byte(term), index.SearchBudget{})
		require.NoError(t, err)
		require.Equal(t, want, bitmapRows(t, got), "term %q", term)
		require.True(t, bl.MightContain([]byte(term)))
	}
	for _, absent := range []string{"status-999", "", "nonsense"} {
		got, err := ts.SearchTerm(context.Background(), []byte(absent), index.SearchBudget{})
		require.NoError(t, err)
		require.Empty(t, bitmapRows(t, got), "absent term %q", absent)
		require.False(t, bl.MightContain([]byte(absent)))
	}

	// The generic trait: raw term in index.Query.Term with K capping.
	trait, err := li.Search(context.Background(),
		index.Query{Term: []byte("status-3"), K: 7}, index.SearchBudget{})
	require.NoError(t, err)
	require.LessOrEqual(t, len(trait.Hits), 7)
	for _, h := range trait.Hits {
		require.Equal(t, "status-3", vals[h.RowID])
	}
}

// TestR12_BitmapBloomFallback asserts the high-cardinality path: the
// auto-enable rule rejects the column, a bloom pre-filter is built
// instead (no false negatives, fpp-bounded false positives), and row
// enumeration reports ErrUnsupported (callers fall back to a scan).
func TestR12_BitmapBloomFallback(t *testing.T) {
	const n = 5000
	vals := make([]string, n)
	for i := range vals {
		vals[i] = fmt.Sprintf("uuid-%d", i) // all distinct
	}
	col := &bytesCol{baseCol: baseCol{field: "uid", rows: n}, vals: vals}
	ix := NewBitmap(BitmapConfig{})
	li, dir := buildAndLoad(t, ix, col)
	require.FileExists(t, filepath.Join(dir, fileBloomBn))

	bl := li.(*bitmapLoaded)
	require.False(t, bl.AutoEnabled(), "5000 distinct values must not auto-enable")
	require.False(t, bl.Cardinality() < BitmapCardinalityLimit)

	// No false negatives on present values.
	for i := 0; i < 300; i++ {
		require.True(t, bl.MightContain([]byte(vals[(i*17)%n])),
			"bloom must never miss an inserted value (%s)", vals[(i*17)%n])
	}
	// False positives stay within the fpp contract (1% target; assert
	// a 5x safety margin so the test is robust across rebuilds).
	fp := 0
	for i := 0; i < 300; i++ {
		if bl.MightContain([]byte(fmt.Sprintf("absent-%d", i))) {
			fp++
		}
	}
	require.Less(t, fp, 15, "bloom false positives (%d/300) exceed the fpp margin", fp)

	// Enumeration over a bloom-only index is impossible: ErrUnsupported.
	_, err := li.(TermSearcher).SearchTerm(context.Background(), []byte(vals[0]), index.SearchBudget{})
	require.ErrorIs(t, err, index.ErrUnsupported)
	_, err = li.Search(context.Background(), index.Query{Term: []byte(vals[0])}, index.SearchBudget{})
	require.ErrorIs(t, err, index.ErrUnsupported)

	// Boundary: exactly 4096 distinct values is already above the rule
	// (strictly-below) — bloom mode, not exact bitmaps.
	const bn = 4096
	bvals := make([]string, bn)
	for i := range bvals {
		bvals[i] = fmt.Sprintf("edge-%d", i)
	}
	bli, _ := buildAndLoad(t, NewBitmap(BitmapConfig{}),
		&bytesCol{baseCol: baseCol{field: "edge", rows: bn}, vals: bvals})
	require.False(t, bli.(*bitmapLoaded).AutoEnabled(),
		"exactly %d distinct must NOT auto-enable", bn)
	require.True(t, bli.(*bitmapLoaded).MightContain([]byte("edge-4095")))
}

// ---------------------------------------------------------------------------
// Trait plumbing: roundtrip, merge, budget, quantize, _id wiring.
// ---------------------------------------------------------------------------

// TestR12_BuildLoadRoundtrip covers Build-to-temp-dir + Load for every
// scalar index flavor, the registry factories, Quantize rejection and
// MemEstimate.
func TestR12_BuildLoadRoundtrip(t *testing.T) {
	ctx := context.Background()

	t.Run("bkd_long", func(t *testing.T) {
		vals := []int64{5, 3, 9, 1, 7, 3, 0, 9}
		ix, err := index.NewIndex(index.KindBKD) // registry factory
		require.NoError(t, err)
		require.Equal(t, index.KindBKD, ix.Kind())
		li, dir := buildAndLoad(t, ix,
			&longCol{baseCol: baseCol{field: "v", rows: len(vals)}, vals: vals})
		for _, f := range []string{fileMeta, fileVals, fileRows} {
			require.FileExists(t, filepath.Join(dir, f))
		}
		res, err := li.(RangeSearcher).SearchRange(ctx, RangeQuery{Min: 3, Max: 7}, index.SearchBudget{})
		require.NoError(t, err)
		// half-open [3,7): values 5,3,3 at rows 0,1,5 (the 7 at row 3
		// and the 9s are excluded).
		require.Equal(t, []uint32{0, 1, 5}, bitmapRows(t, res.Rows))
		require.ErrorIs(t, ix.Quantize(ctx, index.QuantSQ8), index.ErrUnsupported)
		require.Greater(t, ix.MemEstimate(), int64(0))
	})

	t.Run("bkd_geo", func(t *testing.T) {
		lats := []float64{10, 10.01, -50, 80}
		lons := []float64{20, 20.01, 30, -100}
		ix := NewBKD(BKDConfig{})
		li, dir := buildAndLoad(t, ix,
			&geoCol{baseCol: baseCol{field: "loc", rows: len(lats)}, lats: lats, lons: lons})
		for _, f := range []string{fileMeta, fileLats, fileLons, fileRows, fileNodes} {
			require.FileExists(t, filepath.Join(dir, f))
		}
		res, err := li.(GeoSearcher).SearchGeoDistance(ctx,
			GeoDistanceQuery{Lat: 10, Lon: 20, RadiusM: 5_000}, index.SearchBudget{})
		require.NoError(t, err)
		require.Equal(t, []uint32{0, 1}, bitmapRows(t, res.Rows))
	})

	t.Run("bitmap", func(t *testing.T) {
		vals := []string{"a", "b", "a", "c", "b", "a"}
		ix, err := index.NewIndex(index.KindBitmap) // registry factory
		require.NoError(t, err)
		require.Equal(t, index.KindBitmap, ix.Kind())
		li, dir := buildAndLoad(t, ix,
			&bytesCol{baseCol: baseCol{field: "tag", rows: len(vals)}, vals: vals})
		for _, f := range []string{fileMetaB, fileDict, filePosts} {
			require.FileExists(t, filepath.Join(dir, f))
		}
		got, err := li.(TermSearcher).SearchTerm(ctx, []byte("a"), index.SearchBudget{})
		require.NoError(t, err)
		require.Equal(t, []uint32{0, 2, 5}, bitmapRows(t, got))
		require.ErrorIs(t, ix.Quantize(ctx, index.QuantSQ8), index.ErrUnsupported)
		require.Greater(t, ix.MemEstimate(), int64(0))
	})

	t.Run("wrong_column_type", func(t *testing.T) {
		vec := vecOnlyCol{field: "vec", rows: 4, dims: 2}
		err := NewBKD(BKDConfig{}).Build(ctx, vec, &dirOutput{dir: t.TempDir()})
		require.Error(t, err)
		err = NewBitmap(BitmapConfig{}).Build(ctx, vec, &dirOutput{dir: t.TempDir()})
		require.Error(t, err)
	})
}

// vecOnlyCol is the M0 minimal vector-only provider: it implements
// none of the scalar extensions, so Build must reject it.
type vecOnlyCol struct {
	field string
	rows  int
	dims  int
}

func (c vecOnlyCol) Field() string { return c.field }
func (c vecOnlyCol) Rows() int64   { return int64(c.rows) }
func (c vecOnlyCol) Dim() int      { return c.dims }
func (c vecOnlyCol) ForEachVector(fn func(int32, []float32) error) error {
	for i := 0; i < c.rows; i++ {
		if err := fn(int32(i), make([]float32, c.dims)); err != nil {
			return err
		}
	}
	return nil
}

// TestR12_Merge exercises the trait Merge (rebuild semantics) for BKD
// and bitmap: row ids are reassigned by position.
func TestR12_Merge(t *testing.T) {
	ctx := context.Background()

	t.Run("bkd_long", func(t *testing.T) {
		r := rand.New(rand.NewPCG(11, 12))
		const half = 1000
		valsA := make([]int64, half)
		valsB := make([]int64, half)
		for i := range valsA {
			valsA[i] = r.Int64N(500)
			valsB[i] = r.Int64N(500)
		}
		ixA := NewBKD(BKDConfig{})
		dirA := t.TempDir()
		require.NoError(t, ixA.Build(ctx,
			&longCol{baseCol: baseCol{field: "v", rows: half}, vals: valsA}, &dirOutput{dirA}))
		ixB := NewBKD(BKDConfig{})
		dirB := t.TempDir()
		require.NoError(t, ixB.Build(ctx,
			&longCol{baseCol: baseCol{field: "v", rows: half}, vals: valsB}, &dirOutput{dirB}))
		liB, err := ixB.Load(&dirInput{dirB})
		require.NoError(t, err)
		defer liB.Close()

		mergedDir := t.TempDir()
		require.NoError(t, ixA.Merge(ctx, []index.LoadedIndex{liB}, &dirOutput{mergedDir}))
		liM, err := ixA.Load(&dirInput{mergedDir})
		require.NoError(t, err)
		defer liM.Close()

		// Ground truth: concatenated column, positional rows.
		concat := append(append([]int64{}, valsA...), valsB...)
		var want []uint32
		for row, v := range concat {
			if v >= 100 && v < 200 {
				want = append(want, uint32(row))
			}
		}
		res, err := liM.(RangeSearcher).SearchRange(ctx, RangeQuery{Min: 100, Max: 200}, index.SearchBudget{})
		require.NoError(t, err)
		require.Equal(t, want, bitmapRows(t, res.Rows))
	})

	t.Run("bitmap", func(t *testing.T) {
		terms := []string{"alpha", "beta", "gamma", "delta"}
		termsB := []string{"gamma", "delta", "epsilon", "zeta"}
		r := rand.New(rand.NewPCG(13, 14))
		const half = 500
		valsA := make([]string, half)
		valsB := make([]string, half)
		for i := range valsA {
			valsA[i] = terms[r.IntN(len(terms))]
			valsB[i] = termsB[r.IntN(len(termsB))]
		}
		ixA := NewBitmap(BitmapConfig{})
		dirA := t.TempDir()
		require.NoError(t, ixA.Build(ctx,
			&bytesCol{baseCol: baseCol{field: "tag", rows: half}, vals: valsA}, &dirOutput{dirA}))
		ixB := NewBitmap(BitmapConfig{})
		dirB := t.TempDir()
		require.NoError(t, ixB.Build(ctx,
			&bytesCol{baseCol: baseCol{field: "tag", rows: half}, vals: valsB}, &dirOutput{dirB}))
		liB, err := ixB.Load(&dirInput{dirB})
		require.NoError(t, err)
		defer liB.Close()

		mergedDir := t.TempDir()
		require.NoError(t, ixA.Merge(ctx, []index.LoadedIndex{liB}, &dirOutput{mergedDir}))
		liM, err := ixA.Load(&dirInput{mergedDir})
		require.NoError(t, err)
		defer liM.Close()
		require.True(t, liM.(*bitmapLoaded).AutoEnabled())

		// "gamma" exists in both halves: positional rows keep A's
		// 0..499 and place B's at 500..999.
		var want []uint32
		for row, v := range append(append([]string{}, valsA...), valsB...) {
			if v == "gamma" {
				want = append(want, uint32(row))
			}
		}
		got, err := liM.(TermSearcher).SearchTerm(ctx, []byte("gamma"), index.SearchBudget{})
		require.NoError(t, err)
		require.Equal(t, want, bitmapRows(t, got))
	})
}

// TestR12_Budget asserts the budget contract: scalar indexes are exact
// filters, so a cap that truncates work surfaces ErrBudgetExhausted
// instead of silently returning a partial row set.
func TestR12_Budget(t *testing.T) {
	ctx := context.Background()
	const n = 2000
	r := rand.New(rand.NewPCG(15, 16))
	vals := make([]int64, n)
	for i := range vals {
		vals[i] = r.Int64N(1000)
	}
	li, _ := buildAndLoad(t, NewBKD(BKDConfig{}),
		&longCol{baseCol: baseCol{field: "v", rows: n}, vals: vals})
	rs := li.(RangeSearcher)

	q := RangeQuery{Min: 100, Max: 200} // ~10% of rows match
	res, err := rs.SearchRange(ctx, q, index.SearchBudget{})
	require.NoError(t, err)
	require.NotEmpty(t, bitmapRows(t, res.Rows))

	_, err = rs.SearchRange(ctx, q, index.SearchBudget{MaxVisited: 5})
	require.ErrorIs(t, err, ErrBudgetExhausted)
	// Zero budget fields mean unlimited.
	res, err = rs.SearchRange(ctx, q, index.SearchBudget{MaxVisited: int64(n) * 2})
	require.NoError(t, err)
	require.NotEmpty(t, bitmapRows(t, res.Rows))

	lats := make([]float64, n)
	lons := make([]float64, n)
	for i := range lats {
		lats[i] = 39.9 + (r.Float64()-0.5)*2
		lons[i] = 116.4 + (r.Float64()-0.5)*2
	}
	gli, _ := buildAndLoad(t, NewBKD(BKDConfig{LeafSize: 64}),
		&geoCol{baseCol: baseCol{field: "loc", rows: n}, lats: lats, lons: lons})
	gq := GeoDistanceQuery{Lat: 39.9, Lon: 116.4, RadiusM: 200_000}
	_, err = gli.(GeoSearcher).SearchGeoDistance(ctx, gq, index.SearchBudget{MaxDistComps: 3})
	require.ErrorIs(t, err, ErrBudgetExhausted)
}

// TestR12_IDLookup verifies the _id wiring (T12.3): LookupIDs adapts
// the storage engine's hash-based point lookups (T4 idstore) into the
// roaring row-bitmap filter surface; the hash table itself is reused,
// never re-implemented here.
type fakeIDStore struct {
	m map[string]types.RowID
}

// LookupID mirrors segment.Segment.LookupID's exact signature, so the
// storage segment satisfies IDLookup with zero adapter code.
func (f *fakeIDStore) LookupID(ctx context.Context, id string) (types.RowID, bool, error) {
	row, ok := f.m[id]
	return row, ok, nil
}

func TestR12_IDLookup(t *testing.T) {
	ctx := context.Background()
	var _ IDLookup = (*fakeIDStore)(nil)

	lk := &fakeIDStore{m: map[string]types.RowID{
		"doc-a": 0, "doc-b": 7, "doc-c": 12345,
	}}
	got, err := LookupIDs(ctx, lk, []string{"doc-c", "missing", "doc-a", "doc-b", "doc-a"})
	require.NoError(t, err)
	require.Equal(t, []uint32{0, 7, 12345}, bitmapRows(t, got))

	// Empty batch and lookup errors propagate.
	got, err = LookupIDs(ctx, lk, nil)
	require.NoError(t, err)
	require.True(t, got.IsEmpty())

	_, err = LookupIDs(ctx, nil, []string{"x"})
	require.Error(t, err)

	// Canceled context aborts the batch walk.
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	_, err = LookupIDs(canceled, lk, []string{"doc-a"})
	require.Error(t, err)
}
