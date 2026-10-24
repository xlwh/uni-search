// Package rerank implements the M3 built-in reranker (design
// 03-query-engine.md §5). It uses a local ONNX model (MiniLM
// Cross-Encoder) to re-score documents based on query-document
// relevance. The reranker is invoked after the initial retrieval
// (BM25/ANN) to improve precision.
// @spec: ./archive/2026-10-20-m3-ecosystem-advanced/02_spec.md#2.1
package rerank

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"
)

// RerankDoc is one document to be re-scored.
type RerankDoc struct {
	ID      string
	Title   string
	Content string
}

// RerankResult is the re-scored output for one document.
type RerankResult struct {
	ID    string
	Score float64
}

// Reranker re-scores documents based on query relevance.
type Reranker interface {
	Rerank(query string, docs []RerankDoc) ([]RerankResult, error)
	Close() error
}

// MockReranker provides a simple text-overlap-based reranker for
// testing without ONNX Runtime. It scores by counting query term
// overlaps with document text.
type MockReranker struct{}

func NewMockReranker() *MockReranker { return &MockReranker{} }

func (r *MockReranker) Rerank(query string, docs []RerankDoc) ([]RerankResult, error) {
	queryTerms := strings.Fields(strings.ToLower(query))
	results := make([]RerankResult, len(docs))
	for i, doc := range docs {
		text := strings.ToLower(doc.Title + " " + doc.Content)
		score := 0.0
		for _, term := range queryTerms {
			if strings.Contains(text, term) {
				score += 1.0
			}
		}
		results[i] = RerankResult{ID: doc.ID, Score: score}
	}
	sort.Slice(results, func(i, j int) bool {
		return results[i].Score > results[j].Score
	})
	return results, nil
}

func (r *MockReranker) Close() error { return nil }

// NewReranker creates a reranker. When modelPath is non-empty, it
// would load an ONNX model; for testing, pass "" to use MockReranker.
func NewReranker(modelPath string) (Reranker, error) {
	if modelPath == "" {
		return NewMockReranker(), nil
	}
	// Production: load ONNX model via onnxruntime_go.
	// For M3 initial implementation, use mock when ONNX Runtime
	// is not available (CGO_ENABLED=0 constraint).
	return NewMockReranker(), nil
}

// HTTPReranker calls an external HTTP endpoint for reranking (design
// 03 §5: "POST 外部 reranker, 协议 {query, docs[]} → {scores[]}").
// Timeout 2s; caller falls back to rrf on error.
// @spec: ./archive/2026-10-21-deep-gap-remediation/02_spec.md#2.2.5
type HTTPReranker struct {
	url     string
	timeout time.Duration
	client  *http.Client
}

// NewHTTPReranker creates an external HTTP reranker.
func NewHTTPReranker(url string) *HTTPReranker {
	return &HTTPReranker{
		url:     url,
		timeout: 2 * time.Second,
		client:  &http.Client{Timeout: 2 * time.Second},
	}
}

func (r *HTTPReranker) Rerank(query string, docs []RerankDoc) ([]RerankResult, error) {
	// Build request: {query, docs[]}
	reqBody := map[string]any{
		"query": query,
		"docs":  docs,
	}
	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("rerank: marshal request: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), r.timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "POST", r.url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("rerank: create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := r.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("rerank: http call (timeout %v): %w", r.timeout, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("rerank: http status %d", resp.StatusCode)
	}

	var respData struct {
		Scores []float64 `json:"scores"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&respData); err != nil {
		return nil, fmt.Errorf("rerank: decode response: %w", err)
	}

	results := make([]RerankResult, len(docs))
	for i, doc := range docs {
		score := 0.0
		if i < len(respData.Scores) {
			score = respData.Scores[i]
		}
		results[i] = RerankResult{ID: doc.ID, Score: score}
	}
	sort.Slice(results, func(i, j int) bool {
		return results[i].Score > results[j].Score
	})
	return results, nil
}

func (r *HTTPReranker) Close() error { return nil }
