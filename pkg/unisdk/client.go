// Package unisdk implements the M3 Go SDK (design 00 §4, 概要设计 §10).
// HTTP REST client compatible with elasticsearch-go interface patterns.
// @spec: ./archive/2026-10-20-m3-ecosystem-advanced/02_spec.md#2.7
package unisdk

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Client is the UniSearch Go SDK client (HTTP REST).
type Client struct {
	baseURL string
	http    *http.Client
}

// NewClient creates a client targeting the given base URL.
func NewClient(addr string) *Client {
	return &Client{
		baseURL: addr,
		http:    &http.Client{Timeout: 30 * time.Second},
	}
}

// IndexResult is the response from indexing a document.
type IndexResult struct {
	ID      string `json:"_id"`
	Index   string `json:"_index"`
	Version int64  `json:"_version"`
	Result  string `json:"result"`
	SeqNo   int64  `json:"_seq_no"`
}

// SearchResult is the response from a search request.
type SearchResult struct {
	Took int        `json:"took"`
	Hits SearchHits `json:"hits"`
}

type SearchHits struct {
	Total struct {
		Value    int    `json:"value"`
		Relation string `json:"relation"`
	} `json:"total"`
	Hits []json.RawMessage `json:"hits"`
}

// BulkResult is the response from a bulk request.
type BulkResult struct {
	Errors bool              `json:"errors"`
	Items  []json.RawMessage `json:"items"`
}

// GetResult is the response from a get request.
type GetResult struct {
	Found  bool            `json:"found"`
	Index  string          `json:"_index"`
	ID     string          `json:"_id"`
	Source json.RawMessage `json:"_source"`
}

// Index indexes a single document.
func (c *Client) Index(ctx context.Context, index, id string, doc any) (*IndexResult, error) {
	body, err := json.Marshal(doc)
	if err != nil {
		return nil, err
	}
	req, _ := http.NewRequestWithContext(ctx, "PUT", c.baseURL+"/"+index+"/_doc/"+id, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var result IndexResult
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	return &result, nil
}

// Search executes a search request.
func (c *Client) Search(ctx context.Context, index string, body []byte) (*SearchResult, error) {
	req, _ := http.NewRequestWithContext(ctx, "POST", c.baseURL+"/"+index+"/_search", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var result SearchResult
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	return &result, nil
}

// Bulk sends a bulk request (NDJSON body).
func (c *Client) Bulk(ctx context.Context, body []byte) (*BulkResult, error) {
	req, _ := http.NewRequestWithContext(ctx, "POST", c.baseURL+"/_bulk", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/x-ndjson")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var result BulkResult
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	return &result, nil
}

// Get retrieves a single document by ID.
func (c *Client) Get(ctx context.Context, index, id string) (*GetResult, error) {
	req, _ := http.NewRequestWithContext(ctx, "GET", c.baseURL+"/"+index+"/_doc/"+id, nil)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var result GetResult
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	return &result, nil
}

// Delete removes a document by ID.
func (c *Client) Delete(ctx context.Context, index, id string) error {
	req, _ := http.NewRequestWithContext(ctx, "DELETE", c.baseURL+"/"+index+"/_doc/"+id, nil)
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// CreateIndex creates a new index with optional mapping.
func (c *Client) CreateIndex(ctx context.Context, index string, body []byte) error {
	var reader io.Reader
	if len(body) > 0 {
		reader = bytes.NewReader(body)
	}
	req, _ := http.NewRequestWithContext(ctx, "PUT", c.baseURL+"/"+index, reader)
	if len(body) > 0 {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("unisdk: create index failed: %s", resp.Status)
	}
	return nil
}

// Refresh flushes an index.
func (c *Client) Refresh(ctx context.Context, index string) error {
	req, _ := http.NewRequestWithContext(ctx, "POST", c.baseURL+"/"+index+"/_refresh", nil)
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// Health returns cluster health.
func (c *Client) Health(ctx context.Context) (map[string]any, error) {
	req, _ := http.NewRequestWithContext(ctx, "GET", c.baseURL+"/_cluster/health", nil)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var result map[string]any
	json.NewDecoder(resp.Body).Decode(&result)
	return result, nil
}
