package hnsw

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"math"
	"math/rand/v2"
	"sort"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/unisearch-db/unisearch/internal/index"
	"github.com/unisearch-db/unisearch/internal/simd"
)

// randomUnitVectors returns n row-major dims-dimensional Gaussian
// directions scaled to unit norm (isotropic on the sphere).
func randomUnitVectors(r *rand.Rand, n, dims int) []float32 {
	out := make([]float32, n*dims)
	for i := range out {
		out[i] = float32(r.NormFloat64())
	}
	for i := 0; i < n; i++ {
		normalizeInto(out[i*dims:(i+1)*dims], out[i*dims:(i+1)*dims])
	}
	return out
}

// clusteredUnitVectors generates n random unit vectors with cluster
// structure — the distribution shape of real embedding spaces and the
// standard ANN benchmark workload. Each vector picks one of k random
// unit centers, adds isotropic noise of total magnitude ~0.35, and is
// normalized back to unit norm. (Pure i.i.d. directions are the worst
// case for graph ANN — neighbor transitivity collapses in high dims —
// and no HNSW implementation reaches the 0.95@ef64 target there.) It
// also returns the centers so queries can be drawn from the same
// distribution.
func clusteredUnitVectors(r *rand.Rand, n, dims int) (out, centers []float32) {
	k := max(n/20, 1)
	centers = randomUnitVectors(r, k, dims)
	out = clusteredAround(r, centers, dims, n)
	return out, centers
}

// clusteredAround draws n unit vectors around the given cluster
// centers (same shape as clusteredUnitVectors).
func clusteredAround(r *rand.Rand, centers []float32, dims, n int) []float32 {
	k := len(centers) / dims
	noise := 0.35 / float32(math.Sqrt(float64(dims)))
	out := make([]float32, n*dims)
	for i := 0; i < n; i++ {
		c := r.IntN(k) * dims
		v := out[i*dims : (i+1)*dims]
		for d := 0; d < dims; d++ {
			v[d] = centers[c+d] + noise*float32(r.NormFloat64())
		}
		normalizeInto(v, v)
	}
	return out
}

// bruteTopK returns the exact top-k ids by descending score.
func bruteTopK(n, k int, score func(id int32) float32) []int32 {
	type si struct {
		id int32
		s  float32
	}
	ss := make([]si, n)
	for i := 0; i < n; i++ {
		ss[i] = si{int32(i), score(int32(i))}
	}
	sort.Slice(ss, func(a, b int) bool {
		if ss[a].s == ss[b].s {
			return ss[a].id < ss[b].id
		}
		return ss[a].s > ss[b].s
	})
	out := make([]int32, k)
	for i := range out {
		out[i] = ss[i].id
	}
	return out
}

func hitIDs(hits []Hit) map[int32]struct{} {
	m := make(map[int32]struct{}, len(hits))
	for _, h := range hits {
		m[h.RowID] = struct{}{}
	}
	return m
}

// TestR8_SmallExactCorrectness checks a small 2000x64d build against
// brute force for every metric: recall@10 must be essentially exact.
func TestR8_SmallExactCorrectness(t *testing.T) {
	const n, dims, k, nq = 2000, 64, 10, 20
	r := rand.New(rand.NewPCG(1, 2))
	vecs, centers := clusteredUnitVectors(r, n, dims)
	qflat := clusteredAround(r, centers, dims, nq)
	for _, mc := range []struct {
		name string
		m    Metric
	}{
		{"cosine", MetricCosine},
		{"dot", MetricDot},
		{"l2", MetricL2},
	} {
		t.Run(mc.name, func(t *testing.T) {
			g, err := BuildGraph(Config{Seed: 3, Metric: mc.m}, n, dims, vecs)
			require.NoError(t, err)
			var recall float64
			for qi := 0; qi < nq; qi++ {
				q := qflat[qi*dims : (qi+1)*dims]
				qn := NormalizeQuery(Config{Metric: mc.m}, q)
				// Ground truth over the very copies the graph stores.
				gt := bruteTopK(n, k, func(id int32) float32 {
					if mc.m == MetricL2 {
						return -simd.L2F32(qn, g.Vector(id))
					}
					return simd.DotF32(qn, g.Vector(id))
				})
				hits, err := g.Search(context.Background(), qn, k, 64, nil, index.SearchBudget{})
				require.NoError(t, err)
				require.Len(t, hits, k)
				got := hitIDs(hits)
				hit := 0
				for _, id := range gt {
					if _, ok := got[id]; ok {
						hit++
					}
				}
				recall += float64(hit) / k
			}
			recall /= nq
			t.Logf("%s: recall@10 (ef=64) = %.4f", mc.name, recall)
			require.GreaterOrEqual(t, recall, 0.99)
		})
	}
}

// TestR8_CosineNormalization verifies the query-side transform (§2.2):
// unit norm after normalization, zero vectors passed through without
// NaN, and a graph containing a zero vector stays searchable.
func TestR8_CosineNormalization(t *testing.T) {
	r := rand.New(rand.NewPCG(5, 6))

	v := make([]float32, 128)
	for i := range v {
		v[i] = r.Float32()*2 - 1
	}
	q := NormalizeQuery(Config{}, v) // zero Config -> cosine
	var ss float64
	for _, x := range q {
		ss += float64(x) * float64(x)
	}
	require.InDelta(t, 1.0, math.Sqrt(ss), 1e-5, "normalized query must have unit norm")

	z := make([]float32, 128)
	zq := NormalizeQuery(Config{}, z)
	for i, x := range zq {
		require.False(t, math.IsNaN(float64(x)), "zero query must not produce NaN at dim %d", i)
		require.InDelta(t, 0.0, float64(x), 0.0)
	}

	// Row 0 of this column is the zero vector; the build and search
	// must handle it without NaNs.
	const n, dims = 200, 16
	vecs := make([]float32, n*dims)
	for i := dims; i < len(vecs); i++ {
		vecs[i] = r.Float32()*2 - 1
	}
	g, err := BuildGraph(Config{Seed: 9}, n, dims, vecs)
	require.NoError(t, err)
	hits, err := g.Search(context.Background(), zq[:dims], 5, 64, nil, index.SearchBudget{})
	require.NoError(t, err)
	require.NotEmpty(t, hits)
	for _, h := range hits {
		require.False(t, math.IsNaN(float64(h.Score)))
		require.False(t, math.IsInf(float64(h.Score), 0))
	}
	// A zero query is orthogonal to every (normalized) stored vector.
	for _, h := range hits {
		require.InDelta(t, 0.0, float64(h.Score), 1e-6)
	}
}

// bigFixture is shared by the recall tests: one build at the full
// scale (or the -short scale), queries, and exact ground truth.
type bigFixture struct {
	g       *Graph
	queries [][]float32
	gt      [][]int32
	n, dims int
	k       int
}

var (
	bigOnce sync.Once
	big     *bigFixture
	bigErr  error
)

func loadBig(t *testing.T) *bigFixture {
	bigOnce.Do(func() {
		n, dims, nq, k := 100000, 768, 100, 10
		if testing.Short() {
			n, dims, nq = 20000, 128, 20
		}
		r := rand.New(rand.NewPCG(7, 8))
		vecs, centers := clusteredUnitVectors(r, n, dims)
		g, err := BuildGraph(Config{Seed: 42}, n, dims, vecs)
		if err != nil {
			bigErr = err
			return
		}
		flat := clusteredAround(r, centers, dims, nq)
		queries := make([][]float32, nq)
		for i := range queries {
			queries[i] = flat[i*dims : (i+1)*dims]
		}
		gt := make([][]int32, nq)
		for i := range queries {
			q := queries[i]
			gt[i] = bruteTopK(n, k, func(id int32) float32 {
				return simd.DotF32(q, g.Vector(id))
			})
		}
		big = &bigFixture{g: g, queries: queries, gt: gt, n: n, dims: dims, k: k}
	})
	require.NoError(t, bigErr)
	require.NotNil(t, big)
	return big
}

// TestR8_AnnRecall measures recall@10 against brute force at the full
// benchmark scale (100k x 768d, 100 queries, ef=64, no filter) with a
// 0.95 threshold; -short shrinks the workload but keeps the threshold.
func TestR8_AnnRecall(t *testing.T) {
	f := loadBig(t)
	ctx := context.Background()
	var recall float64
	for i, q := range f.queries {
		qn := NormalizeQuery(f.g.Config(), q)
		hits, err := f.g.Search(ctx, qn, f.k, 64, nil, index.SearchBudget{})
		require.NoError(t, err)
		require.Len(t, hits, f.k)
		got := hitIDs(hits)
		hit := 0
		for _, id := range f.gt[i] {
			if _, ok := got[id]; ok {
				hit++
			}
		}
		recall += float64(hit) / float64(f.k)
	}
	recall /= float64(len(f.queries))
	t.Logf("n=%d dims=%d: recall@%d (ef=64, no filter) = %.4f", f.n, f.dims, f.k, recall)
	require.GreaterOrEqual(t, recall, 0.95)
}

// TestR8_SQ8Quantization verifies the warm tier (§2.5): after SQ8 the
// same queries reach >= 0.98 recall relative to the float32 index
// (int8 prefilter + float32 rerank), and the quantization state costs
// less than 1/3 of the raw float32 vectors.
func TestR8_SQ8Quantization(t *testing.T) {
	f := loadBig(t)
	ctx := context.Background()

	// Float32 baseline from the same graph.
	base := make([][]int32, len(f.queries))
	for i, q := range f.queries {
		hits, err := f.g.Search(ctx, NormalizeQuery(f.g.Config(), q), f.k, 64, nil, index.SearchBudget{})
		require.NoError(t, err)
		base[i] = make([]int32, len(hits))
		for j, h := range hits {
			base[i][j] = h.RowID
		}
	}

	require.NoError(t, f.g.EnableSQ8())
	defer f.g.DisableSQ8()
	require.True(t, f.g.Quantized())
	sq := f.g.sq.Load()
	require.NotNil(t, sq)
	rawBytes := int64(4 * f.n * f.dims)
	require.Less(t, sq.MemBytes(), rawBytes/3,
		"SQ8 state (%d B) must be < 1/3 of float32 raw vectors (%d B)", sq.MemBytes(), rawBytes/3)

	var rel float64
	for i, q := range f.queries {
		hits, err := f.g.Search(ctx, NormalizeQuery(f.g.Config(), q), f.k, 64, nil, index.SearchBudget{})
		require.NoError(t, err)
		require.Len(t, hits, f.k)
		got := hitIDs(hits)
		hit := 0
		for _, id := range base[i] {
			if _, ok := got[id]; ok {
				hit++
			}
		}
		rel += float64(hit) / float64(f.k)
	}
	rel /= float64(len(f.queries))
	t.Logf("n=%d dims=%d: SQ8 recall@%d relative to float32 index (ef=64 -> x1.5) = %.4f",
		f.n, f.dims, f.k, rel)
	require.GreaterOrEqual(t, rel, 0.98)
}

// sliceColumn is an in-memory index.ColumnProvider over a flat vector
// block.
type sliceColumn struct {
	field string
	dims  int
	vecs  []float32
}

func (c *sliceColumn) Field() string { return c.field }
func (c *sliceColumn) Rows() int64   { return int64(len(c.vecs) / c.dims) }
func (c *sliceColumn) Dim() int      { return c.dims }
func (c *sliceColumn) ForEachVector(fn func(int32, []float32) error) error {
	n := len(c.vecs) / c.dims
	for i := 0; i < n; i++ {
		if err := fn(int32(i), c.vecs[i*c.dims:(i+1)*c.dims]); err != nil {
			return err
		}
	}
	return nil
}

// memOutput / memInput implement index.IndexOutput / index.IndexInput
// over an in-memory file map, standing in for the segment directory.
type memOutput struct{ files map[string][]byte }

func (m *memOutput) Create(name string) (io.WriteCloser, error) {
	b := &bytes.Buffer{}
	return &memFile{b: b, on: func() { m.files[name] = b.Bytes() }}, nil
}

type memFile struct {
	b  *bytes.Buffer
	on func()
}

func (f *memFile) Write(p []byte) (int, error) { return f.b.Write(p) }
func (f *memFile) Close() error                { f.on(); return nil }

type memInput struct{ files map[string][]byte }

func (m *memInput) Open(name string) (io.ReadSeekCloser, error) {
	b, ok := m.files[name]
	if !ok {
		return nil, fmt.Errorf("no such file %q", name)
	}
	return &memReader{bytes.NewReader(b)}, nil
}

type memReader struct{ *bytes.Reader }

func (m *memReader) Close() error { return nil }

// TestR8_BuildLoadRoundtrip exercises the index.Index trait end to
// end: Build writes the §2.1 file set, Load restores a searchable
// index, Quantize flips the SQ8 tier (and a rewrite + reload restores
// it from quant.state), and Merge rebuilds a whole from parts.
func TestR8_BuildLoadRoundtrip(t *testing.T) {
	const n, dims = 500, 32
	r := rand.New(rand.NewPCG(11, 12))
	vecs := randomUnitVectors(r, n, dims)

	out := &memOutput{files: map[string][]byte{}}
	ix := New(Config{Seed: 13})
	require.Equal(t, index.KindHNSW, ix.Kind())
	require.NoError(t, ix.Build(context.Background(), &sliceColumn{field: "vec", dims: dims, vecs: vecs}, out))
	for _, f := range []string{fileGraph, fileLevels, fileEntry, fileQuant, fileVecs} {
		require.Contains(t, out.files, f, "Build must write %s", f)
	}
	require.Greater(t, ix.MemEstimate(), int64(0))

	l1, err := ix.Load(&memInput{files: out.files})
	require.NoError(t, err)
	defer l1.Close()

	// A data-point query must return the point itself first.
	q := NormalizeQuery(Config{}, vecs[3*dims:4*dims])
	res, err := l1.Search(context.Background(), index.Query{Vector: q, K: 1}, index.SearchBudget{})
	require.NoError(t, err)
	require.NotEmpty(t, res.Hits)
	require.Equal(t, int32(3), res.Hits[0].RowID)

	// Quantize the built index and re-serialize: the reloaded index
	// must come back in the SQ8 tier and find the same nearest row.
	require.NoError(t, ix.Quantize(context.Background(), index.QuantSQ8))
	require.NoError(t, writeGraphFiles(out, ix.g))
	l2, err := ix.Load(&memInput{files: out.files})
	require.NoError(t, err)
	defer l2.Close()
	res2, err := l2.Search(context.Background(), index.Query{Vector: q, K: 1}, index.SearchBudget{})
	require.NoError(t, err)
	require.NotEmpty(t, res2.Hits)
	require.Equal(t, int32(3), res2.Hits[0].RowID)

	// Merge: combine an index built over the first half with a loaded
	// graph over the second half; the merged whole must search rows
	// from the far side (ids are reassigned by position, the storage
	// layer owns the old-to-new row mapping).
	half := n / 2
	outA := &memOutput{files: map[string][]byte{}}
	ixA := New(Config{Seed: 15})
	require.NoError(t, ixA.Build(context.Background(),
		&sliceColumn{field: "vec", dims: dims, vecs: vecs[:half*dims]}, outA))
	g2, err := BuildGraph(Config{Seed: 14}, n-half, dims, vecs[half*dims:])
	require.NoError(t, err)
	merged := &memOutput{files: map[string][]byte{}}
	require.NoError(t, ixA.Merge(context.Background(), []index.LoadedIndex{&loadedIndex{g: g2}}, merged))
	lm, err := ixA.Load(&memInput{files: merged.files})
	require.NoError(t, err)
	defer lm.Close()
	lastQ := NormalizeQuery(Config{}, vecs[(n-1)*dims:n*dims])
	rm, err := lm.Search(context.Background(), index.Query{Vector: lastQ, K: 1}, index.SearchBudget{})
	require.NoError(t, err)
	require.NotEmpty(t, rm.Hits)
	require.Equal(t, int32(n-1), rm.Hits[0].RowID)
}
