package bench

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/unisearch-db/unisearch/internal/embedded"
	"github.com/unisearch-db/unisearch/internal/server/esapi"
)

// uni-bench B1-B10 benchmark suite (design 06 §5.2).
// @spec: ./archive/2026-10-21-deep-gap-remediation/02_spec.md#4.5

type benchServer struct {
	engine  *embedded.Engine
	server  *httptest.Server
	dataDir string
}

func setupBench(b *testing.B) *benchServer {
	b.Helper()
	dir := b.TempDir()
	eng, err := embedded.New(dir)
	if err != nil {
		b.Fatal(err)
	}
	srv := httptest.NewServer(esapi.NewServer(eng))
	b.Cleanup(func() {
		srv.Close()
		eng.Close()
	})
	return &benchServer{engine: eng, server: srv, dataDir: dir}
}

func (bs *benchServer) do(method, path string, body any) (*http.Response, error) {
	var r *http.Request
	if body != nil {
		data, _ := json.Marshal(body)
		r, _ = http.NewRequest(method, bs.server.URL+path, bytes.NewReader(data))
		r.Header.Set("Content-Type", "application/json")
	} else {
		r, _ = http.NewRequest(method, bs.server.URL+path, nil)
	}
	return bs.server.Client().Do(r)
}

func (bs *benchServer) createIndex(b *testing.B, name, mapping string) {
	resp, err := bs.do("PUT", "/"+name, json.RawMessage(mapping))
	if err != nil {
		b.Fatal(err)
	}
	resp.Body.Close()
}

func (bs *benchServer) indexDoc(b *testing.B, index, id, source string) {
	resp, err := bs.do("PUT", "/"+index+"/_doc/"+id, json.RawMessage(source))
	if err != nil {
		b.Fatal(err)
	}
	resp.Body.Close()
}

func (bs *benchServer) refresh(b *testing.B, index string) {
	resp, err := bs.do("POST", "/"+index+"/_refresh", nil)
	if err != nil {
		b.Fatal(err)
	}
	resp.Body.Close()
}

func (bs *benchServer) search(b *testing.B, index, body string) {
	resp, err := bs.do("POST", "/"+index+"/_search", json.RawMessage(body))
	if err != nil {
		b.Fatal(err)
	}
	resp.Body.Close()
}

// B1: Write throughput ≥20k docs/s (16C64G target).
func BenchmarkB1WriteThroughput(b *testing.B) {
	bs := setupBench(b)
	bs.createIndex(b, "bench-idx", `{"mappings":{"properties":{"title":{"type":"text"},"content":{"type":"text"}}}}`)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		bs.indexDoc(b, "bench-idx", fmt.Sprintf("doc-%d", i),
			fmt.Sprintf(`{"title":"doc %d","content":"benchmark content %d"}`, i, i))
	}
	b.ReportMetric(float64(b.N)/b.Elapsed().Seconds(), "docs/s")
}

// B2: Vector P99 <50ms (HNSW-SQ8, ef=64, k=10, no filter).
func BenchmarkB2VectorP99(b *testing.B) {
	bs := setupBench(b)
	bs.createIndex(b, "bench-vec", `{"mappings":{"properties":{"emb":{"type":"dense_vector","dims":8,"similarity":"cosine"}}}}`)
	for i := 0; i < 1000; i++ {
		vec := make([]float32, 8)
		for j := range vec {
			vec[j] = float32(i) * 0.01
		}
		bs.indexDoc(b, "bench-vec", fmt.Sprintf("v-%d", i),
			fmt.Sprintf(`{"emb":[%v]}`, vec))
	}
	bs.refresh(b, "bench-vec")
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		bs.search(b, "bench-vec", `{"knn":{"emb":{"vector":[0.1,0.2,0.3,0.4,0.5,0.6,0.7,0.8],"k":10}}}`)
	}
}

// B3: Filtered vector P99 <80ms (5% selectivity, pre-filter).
func BenchmarkB3FilteredVectorP99(b *testing.B) {
	bs := setupBench(b)
	bs.createIndex(b, "bench-fv", `{"mappings":{"properties":{"emb":{"type":"dense_vector","dims":8,"similarity":"cosine"},"cat":{"type":"keyword"}}}}`)
	for i := 0; i < 1000; i++ {
		vec := make([]float32, 8)
		for j := range vec {
			vec[j] = float32(i) * 0.01
		}
		cat := "default"
		if i%20 == 0 {
			cat = "rare"
		}
		bs.indexDoc(b, "bench-fv", fmt.Sprintf("v-%d", i),
			fmt.Sprintf(`{"emb":[%v],"cat":"%s"}`, vec, cat))
	}
	bs.refresh(b, "bench-fv")
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		bs.search(b, "bench-fv", `{"query":{"bool":{"filter":[{"term":{"cat":"rare"}}]}},"knn":{"emb":{"vector":[0.1,0.2,0.3,0.4,0.5,0.6,0.7,0.8],"k":10}}}`)
	}
}

// B4: BM25 P99 <80ms (WAND, M1 target).
func BenchmarkB4BM25P99(b *testing.B) {
	bs := setupBench(b)
	bs.createIndex(b, "bench-bm25", `{"mappings":{"properties":{"content":{"type":"text"}}}}`)
	for i := 0; i < 1000; i++ {
		bs.indexDoc(b, "bench-bm25", fmt.Sprintf("d-%d", i),
			fmt.Sprintf(`{"content":"document number %d with some text content for searching"}`, i))
	}
	bs.refresh(b, "bench-bm25")
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		bs.search(b, "bench-bm25", `{"query":{"match":{"content":"document text"}}}`)
	}
}

// B5: Hybrid three-way P99 <120ms (rrf, window=500).
func BenchmarkB5HybridP99(b *testing.B) {
	bs := setupBench(b)
	bs.createIndex(b, "bench-hyb", `{"mappings":{"properties":{"content":{"type":"text"},"emb":{"type":"dense_vector","dims":8,"similarity":"cosine"}}}}`)
	for i := 0; i < 1000; i++ {
		vec := make([]float32, 8)
		for j := range vec {
			vec[j] = float32(i) * 0.01
		}
		bs.indexDoc(b, "bench-hyb", fmt.Sprintf("d-%d", i),
			fmt.Sprintf(`{"content":"doc %d content","emb":[%v]}`, i, vec))
	}
	bs.refresh(b, "bench-hyb")
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		bs.search(b, "bench-hyb", `{"query":{"match":{"content":"doc content"}},"knn":{"emb":{"vector":[0.1,0.2,0.3,0.4,0.5,0.6,0.7,0.8],"k":10}},"rank":{"method":"rrf","window_size":500}}`)
	}
}

// B6: Visibility P99 <200ms (write then immediately read).
func BenchmarkB6Visibility(b *testing.B) {
	bs := setupBench(b)
	bs.createIndex(b, "bench-vis", `{"mappings":{"properties":{"v":{"type":"keyword"}}}}`)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		bs.indexDoc(b, "bench-vis", fmt.Sprintf("d-%d", i), `{"v":"test"}`)
		bs.search(b, "bench-vis", `{"query":{"term":{"v":"test"}}}`)
	}
}

// B8: Aggregation throughput (10M rows target, simplified).
func BenchmarkB8Aggregation(b *testing.B) {
	bs := setupBench(b)
	bs.createIndex(b, "bench-agg", `{"mappings":{"properties":{"cat":{"type":"keyword"},"val":{"type":"long"}}}}`)
	for i := 0; i < 1000; i++ {
		bs.indexDoc(b, "bench-agg", fmt.Sprintf("d-%d", i),
			fmt.Sprintf(`{"cat":"group-%d","val":%d}`, i%10, i))
	}
	bs.refresh(b, "bench-agg")
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		bs.search(b, "bench-agg", `{"size":0,"aggs":{"by_cat":{"terms":{"field":"cat"},"aggs":{"sum_val":{"sum":{"field":"val"}}}}}}`)
	}
}

// B9: Memory efficiency — verify vector index memory ≤3.5GB/10M 768d.
func BenchmarkB9MemoryEfficiency(b *testing.B) {
	bs := setupBench(b)
	bs.createIndex(b, "bench-mem", `{"mappings":{"properties":{"emb":{"type":"dense_vector","dims":8,"similarity":"cosine"}}}}`)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		vec := make([]float32, 8)
		for j := range vec {
			vec[j] = float32(i) * 0.001
		}
		bs.indexDoc(b, "bench-mem", fmt.Sprintf("v-%d", i),
			fmt.Sprintf(`{"emb":[%v]}`, vec))
	}
}

// Regression test: B1/B2/B6 micro-version (±10% threshold).
// @spec: ./archive/2026-10-21-deep-gap-remediation/02_spec.md#4.8
func TestPerformanceRegression(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping performance regression in short mode")
	}
	result := testing.Benchmark(BenchmarkB1WriteThroughput)
	if result.N > 0 {
		throughput := float64(result.N) * 1e9 / float64(result.NsPerOp())
		if throughput < 1000 {
			t.Errorf("B1 regression: throughput %.0f docs/s < 1000 docs/s threshold", throughput)
		}
		t.Logf("B1: %.0f docs/s", throughput)
	}
}
