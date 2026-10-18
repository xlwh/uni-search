// Package embedded wires the meta.Store + an in-memory document store
// into a minimal esapi.Engine implementation suitable for the M0
// `--embedded` single-node mode. Documents persist to JSON files on
// Close (used for the kill-restart-zero-loss test in test/e2e).
//
// This is intentionally minimal: no HNSW search, no BM25 ranking —
// _search returns matches by exact term/string contains. The full
// engine wiring (storage segments + HNSW + inverted index + the
// Volcano executor) lands in M1 once the storage engine's public
// surface is finalized. M0's goal is end-to-end REST plumbing and
// the kill-restart durability contract.
package embedded

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/unisearch-db/unisearch/internal/engine/dsl"
	"github.com/unisearch-db/unisearch/internal/errors"
	"github.com/unisearch-db/unisearch/internal/meta"
	"github.com/unisearch-db/unisearch/internal/server/esapi"
)

// Engine implements esapi.Engine for the M0 embedded mode.
type Engine struct {
	mu      sync.RWMutex
	store   *meta.Store
	dataDir string
	metas   map[string]*meta.IndexMeta
	docs    map[string]map[string]doc // index -> id -> doc
	nextSeq int64
}

type doc struct {
	Source  []byte
	SeqNo   int64
	Primary int64
}

// New opens (or creates) an Engine rooted at dataDir. Meta files are
// read from {dataDir}/meta/*.json; document files from {dataDir}/docs/
// {index}.jsonl. Missing directories are created on first write.
func New(dataDir string) (*Engine, error) {
	if dataDir == "" {
		return nil, fmt.Errorf("embedded: data dir required")
	}
	if err := os.MkdirAll(filepath.Join(dataDir, "meta"), 0o755); err != nil {
		return nil, fmt.Errorf("embedded: create meta dir: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(dataDir, "docs"), 0o755); err != nil {
		return nil, fmt.Errorf("embedded: create docs dir: %w", err)
	}
	store, err := meta.OpenStore(filepath.Join(dataDir, "meta"))
	if err != nil {
		return nil, err
	}
	e := &Engine{
		store:   store,
		dataDir: dataDir,
		metas:   map[string]*meta.IndexMeta{},
		docs:    map[string]map[string]doc{},
		nextSeq: 1,
	}
	if err := e.recover(); err != nil {
		return nil, err
	}
	return e, nil
}

// recover loads all meta files and per-index doc files.
func (e *Engine) recover() error {
	metas, err := e.store.List()
	if err != nil {
		return fmt.Errorf("embedded: list metas: %w", err)
	}
	for _, m := range metas {
		e.metas[m.Name] = m
		docs, err := e.loadDocs(m.Name)
		if err != nil {
			return fmt.Errorf("embedded: load docs for %s: %w", m.Name, err)
		}
		e.docs[m.Name] = docs
		for _, d := range docs {
			if d.SeqNo >= e.nextSeq {
				e.nextSeq = d.SeqNo + 1
			}
		}
	}
	return nil
}

// loadDocs reads {dataDir}/docs/{index}.jsonl; tolerates a missing
// file (the index has not been written to yet).
func (e *Engine) loadDocs(index string) (map[string]doc, error) {
	path := filepath.Join(e.dataDir, "docs", index+".jsonl")
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return map[string]doc{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := map[string]doc{}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var d doc
		if err := json.Unmarshal([]byte(line), &d); err != nil {
			return nil, fmt.Errorf("decode doc line: %w", err)
		}
		// We need the _id to register the doc; loadDocs reads files
		// we wrote via persistDocs which always carries _id as a
		// JSON string field of the wrapper.
		// Re-decode as wrapper:
		var w struct {
			ID string
			doc
		}
		_ = json.Unmarshal([]byte(line), &w)
		out[w.ID] = w.doc
	}
	return out, nil
}

// persistDocs writes the per-index doc table atomically (tmp + fsync
// + rename). The on-disk shape is one JSON object per line carrying
// _id plus the inner doc fields.
func (e *Engine) persistDocs(index string, docs map[string]doc) error {
	path := filepath.Join(e.dataDir, "docs", index+".jsonl")
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+index+".jsonl.tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	// Deterministic order for stable output.
	ids := make([]string, 0, len(docs))
	for id := range docs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	enc := json.NewEncoder(tmp)
	for _, id := range ids {
		d := docs[id]
		w := struct {
			ID string
			doc
		}{ID: id, doc: d}
		if err := enc.Encode(w); err != nil {
			tmp.Close()
			return err
		}
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// Close persists every index's docs and returns. Idempotent.
func (e *Engine) Close() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	var firstErr error
	for index, docs := range e.docs {
		if err := e.persistDocs(index, docs); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// ---------------------------------------------------------------------------
// esapi.Engine implementation
// ---------------------------------------------------------------------------

// ensureIndex loads or creates the meta for an index on first write.
func (e *Engine) ensureIndex(name string) (*meta.IndexMeta, error) {
	if m, ok := e.metas[name]; ok {
		return m, nil
	}
	m := &meta.IndexMeta{Name: name, Mapping: meta.NewMapping()}
	if err := e.store.Save(m); err != nil {
		return nil, err
	}
	e.metas[name] = m
	e.docs[name] = map[string]doc{}
	return m, nil
}

func (e *Engine) Write(_ context.Context, docReq esapi.IndexDoc, _ string) (*esapi.WriteResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, err := e.ensureIndex(docReq.Index); err != nil {
		return nil, err
	}
	// Auto-generate an _id when the client did not provide one
	// (POST /{index}/_doc with no {id} placeholder). ES uses a
	// 20-byte base64-encoded UUID; M0 uses 16 random bytes hex-
	// encoded, which is good enough for the kill-restart test.
	if docReq.ID == "" {
		var buf [16]byte
		if _, err := rand.Read(buf[:]); err != nil {
			return nil, fmt.Errorf("embedded: generate id: %w", err)
		}
		docReq.ID = hex.EncodeToString(buf[:])
	}
	table := e.docs[docReq.Index]
	existing, present := table[docReq.ID]
	// OpCreate conflicts on existing.
	if docReq.Op == esapi.OpCreate && present {
		return &esapi.WriteResult{
			Index: docReq.Index, ID: docReq.ID, Op: docReq.Op,
			Result: "conflict", SeqNo: existing.SeqNo,
		}, nil
	}
	// if_seq_no optimistic locking.
	if docReq.IfSeqNo != nil && present && existing.SeqNo != *docReq.IfSeqNo {
		return &esapi.WriteResult{
			Index: docReq.Index, ID: docReq.ID, Op: docReq.Op,
			Result: "conflict", SeqNo: existing.SeqNo,
		}, nil
	}
	result := "created"
	if present {
		result = "updated"
	}
	seq := e.nextSeq
	e.nextSeq++
	table[docReq.ID] = doc{Source: docReq.Source, SeqNo: seq, Primary: 1}
	return &esapi.WriteResult{
		Index: docReq.Index, ID: docReq.ID, Op: docReq.Op,
		Result: result, SeqNo: seq, Version: 1,
	}, nil
}

func (e *Engine) Bulk(_ context.Context, docs []esapi.IndexDoc, _ string) ([]esapi.WriteResult, error) {
	out := make([]esapi.WriteResult, 0, len(docs))
	for _, d := range docs {
		r, err := e.Write(context.Background(), d, "")
		if err != nil {
			out = append(out, esapi.WriteResult{Index: d.Index, ID: d.ID, Op: d.Op, Err: err})
			continue
		}
		out = append(out, *r)
	}
	return out, nil
}

func (e *Engine) Get(_ context.Context, index, id string) ([]byte, int64, int64, bool, error) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	table, ok := e.docs[index]
	if !ok {
		return nil, 0, 0, true, nil
	}
	d, ok := table[id]
	if !ok {
		return nil, 0, 0, true, nil
	}
	return d.Source, d.SeqNo, d.Primary, false, nil
}

func (e *Engine) MGet(_ context.Context, reqs []esapi.GetRequest) ([]esapi.GetResult, error) {
	out := make([]esapi.GetResult, 0, len(reqs))
	for _, r := range reqs {
		src, seq, prim, notFound, err := e.Get(context.Background(), r.Index, r.ID)
		if err != nil {
			return nil, err
		}
		out = append(out, esapi.GetResult{
			Index: r.Index, ID: r.ID,
			Found: !notFound, Source: src,
			SeqNo: seq, Primary: prim,
		})
	}
	return out, nil
}

func (e *Engine) Delete(_ context.Context, index, id string, ifSeq *int64, _ string) (*esapi.WriteResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	table, ok := e.docs[index]
	if !ok {
		return &esapi.WriteResult{Index: index, ID: id, Op: esapi.OpDelete, Result: "not_found"}, nil
	}
	existing, present := table[id]
	if ifSeq != nil && present && existing.SeqNo != *ifSeq {
		return &esapi.WriteResult{Index: index, ID: id, Op: esapi.OpDelete, Result: "conflict", SeqNo: existing.SeqNo}, nil
	}
	if !present {
		return &esapi.WriteResult{Index: index, ID: id, Op: esapi.OpDelete, Result: "not_found"}, nil
	}
	delete(table, id)
	return &esapi.WriteResult{Index: index, ID: id, Op: esapi.OpDelete, Result: "deleted", SeqNo: existing.SeqNo}, nil
}

// Search runs a minimal scan: parse the body via the DSL parser, then
// walk every doc in the indices (or all indices when none given),
// returning those that match a top-level match clause by case-
// insensitive substring. M1 wires the real plan+executor here.
func (e *Engine) Search(_ context.Context, indices []string, body []byte) (*esapi.SearchResult, error) {
	q, from, size, err := dsl.ParseSearchBody(body)
	if err != nil {
		return nil, err
	}
	if size <= 0 {
		size = 10
	}
	e.mu.RLock()
	defer e.mu.RUnlock()
	if len(indices) == 0 {
		for name := range e.docs {
			indices = append(indices, name)
		}
		sort.Strings(indices)
	}
	var hits []esapi.SearchHit
	for _, idx := range indices {
		table, ok := e.docs[idx]
		if !ok {
			continue
		}
		ids := make([]string, 0, len(table))
		for id := range table {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			d := table[id]
			if matchesQuery(q, d.Source) {
				hits = append(hits, esapi.SearchHit{
					Index: idx, ID: id, Score: 1.0,
					Source: d.Source, SeqNo: d.SeqNo, Primary: d.Primary,
				})
			}
		}
	}
	total := len(hits)
	maxScore := float32(0)
	if len(hits) > 0 {
		maxScore = 1.0
	}
	// Apply from/size slicing.
	start := from
	if start > len(hits) {
		start = len(hits)
	}
	end := start + size
	if end > len(hits) {
		end = len(hits)
	}
	hits = hits[start:end]
	return &esapi.SearchResult{
		Took:     0,
		Shards:   esapi.ShardStats{Total: 1, Successful: 1},
		Total:    total,
		Relation: "eq",
		MaxScore: &maxScore,
		Hits:     hits,
	}, nil
}

// matchesQuery is the M0 minimal predicate: a top-level MatchQuery
// matches if the field's value (string coercion) contains the text
// case-insensitively. Anything else matches everything (match_all
// semantics) so _search at least returns data.
func matchesQuery(q dsl.Query, source []byte) bool {
	if q == nil {
		return true
	}
	mq, ok := q.(*dsl.MatchQuery)
	if !ok {
		// For non-match queries in M0, behave like match_all so the
		// REST pipeline is testable end-to-end. M1 will route to the
		// real plan+executor.
		return true
	}
	var doc map[string]any
	if err := json.Unmarshal(source, &doc); err != nil {
		return false
	}
	val, ok := doc[mq.Field]
	if !ok {
		return false
	}
	str := fmt.Sprintf("%v", val)
	return strings.Contains(strings.ToLower(str), strings.ToLower(mq.Text))
}

func (e *Engine) Count(_ context.Context, indices []string, body []byte) (int64, error) {
	if len(body) > 0 {
		if _, _, _, err := dsl.ParseSearchBody(body); err != nil {
			return 0, err
		}
	}
	e.mu.RLock()
	defer e.mu.RUnlock()
	if len(indices) == 0 {
		var total int64
		for _, table := range e.docs {
			total += int64(len(table))
		}
		return total, nil
	}
	var total int64
	for _, idx := range indices {
		total += int64(len(e.docs[idx]))
	}
	return total, nil
}

func (e *Engine) PutMapping(_ context.Context, index string, body []byte) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	m, ok := e.metas[index]
	if !ok {
		m = &meta.IndexMeta{Name: index, Mapping: meta.NewMapping()}
		e.metas[index] = m
		e.docs[index] = map[string]doc{}
	}
	// Merge top-level "mappings.properties" if present, else merge
	// the body as a mapping fragment.
	var spec struct {
		Mappings *meta.Mapping  `json:"mappings"`
		Settings map[string]any `json:"settings"`
	}
	if err := json.Unmarshal(body, &spec); err != nil {
		return errors.NewDocumentParsing("invalid mapping body: " + err.Error())
	}
	if spec.Mappings != nil {
		m.Mapping = spec.Mappings
	}
	return e.store.Save(m)
}

func (e *Engine) GetMapping(_ context.Context, index string) ([]byte, error) {
	e.mu.RLock()
	m, ok := e.metas[index]
	e.mu.RUnlock()
	if !ok {
		return nil, errors.NewIndexNotFound(index)
	}
	body := map[string]any{
		index: map[string]any{
			"mappings": m.Mapping,
		},
	}
	return json.Marshal(body)
}

func (e *Engine) PutSettings(_ context.Context, index string, body []byte) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	m, ok := e.metas[index]
	if !ok {
		m = &meta.IndexMeta{Name: index, Mapping: meta.NewMapping()}
		e.metas[index] = m
		e.docs[index] = map[string]doc{}
	}
	// Decode ES-style settings wire format; ignore unknown keys.
	var spec struct {
		NumberOfShards   int    `json:"number_of_shards"`
		NumberOfReplicas int    `json:"number_of_replicas"`
		RefreshInterval  string `json:"refresh_interval"`
	}
	if err := json.Unmarshal(body, &spec); err == nil {
		m.Settings.NumberOfShards = spec.NumberOfShards
		m.Settings.NumberOfReplicas = spec.NumberOfReplicas
		if spec.RefreshInterval != "" {
			m.Settings.RefreshInterval = spec.RefreshInterval
		}
	}
	return e.store.Save(m)
}

func (e *Engine) GetSettings(_ context.Context, index string) ([]byte, error) {
	e.mu.RLock()
	m, ok := e.metas[index]
	e.mu.RUnlock()
	if !ok {
		return nil, errors.NewIndexNotFound(index)
	}
	return json.Marshal(map[string]any{
		index: map[string]any{"settings": m.Settings},
	})
}

func (e *Engine) PutAlias(_ context.Context, index, alias string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	m, ok := e.metas[index]
	if !ok {
		return errors.NewIndexNotFound(index)
	}
	for _, a := range m.Aliases {
		if a == alias {
			return nil
		}
	}
	m.Aliases = append(m.Aliases, alias)
	return e.store.Save(m)
}

func (e *Engine) GetAliases(_ context.Context, index string) (map[string][]string, error) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	out := map[string][]string{}
	for name, m := range e.metas {
		if index != "" && name != index {
			continue
		}
		if len(m.Aliases) > 0 {
			out[name] = append([]string(nil), m.Aliases...)
		}
	}
	return out, nil
}

func (e *Engine) Analyze(_ context.Context, _, _, text string) ([]esapi.AnalyzeToken, error) {
	// Minimal M0 analyze: one token per whitespace-separated word.
	parts := strings.Fields(text)
	tokens := make([]esapi.AnalyzeToken, 0, len(parts))
	offset := 0
	for i, p := range parts {
		tokens = append(tokens, esapi.AnalyzeToken{
			Token:       strings.ToLower(p),
			StartOffset: offset,
			EndOffset:   offset + len(p),
			Type:        "word",
			Position:    i,
		})
		offset += len(p) + 1 // +1 for the delimiter.
	}
	return tokens, nil
}

func (e *Engine) Refresh(_ context.Context, _ string) error {
	// M0 in-memory store is always visible; refresh is a no-op.
	return nil
}

func (e *Engine) Indices(_ context.Context, _ string) ([]esapi.IndexInfo, error) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	infos := make([]esapi.IndexInfo, 0, len(e.metas))
	for name := range e.metas {
		docs := int64(len(e.docs[name]))
		infos = append(infos, esapi.IndexInfo{
			Health:      "green",
			Status:      "open",
			Index:       name,
			UUID:        name, // M0 uses the index name as the UUID placeholder.
			Primaries:   1,
			Replicas:    0,
			DocsCount:   docs,
			DocsDeleted: 0,
			StoreSize:   "0kb",
		})
	}
	sort.Slice(infos, func(i, j int) bool { return infos[i].Index < infos[j].Index })
	return infos, nil
}

func (e *Engine) Health(_ context.Context) (*esapi.ClusterHealth, error) {
	return &esapi.ClusterHealth{
		ClusterName:         "unisearch",
		Status:              "green",
		NumberOfNodes:       1,
		NumberOfDataNodes:   1,
		ActivePrimaryShards: 1,
		ActiveShards:        1,
	}, nil
}

// Compile-time assertion that Engine satisfies esapi.Engine.
var _ esapi.Engine = (*Engine)(nil)
