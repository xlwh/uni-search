// Package embedded wires the real engine stack (storage.Shard + HNSW +
// inverted index + Volcano executor + plan optimizer + fusion) into an
// esapi.Engine implementation for the --embedded single-node mode.
//
// Phase 0 (M0 engine wiring): replaces the previous minimal in-memory
// store with the full engine stack. _search now goes through
// DSL→Plan→Optimize→Exec pipeline instead of substring matching.
// @spec: ./archive/2026-10-20-m1-es-compat-ga/02_spec.md#2.1.7
package embedded

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"github.com/unisearch-db/unisearch/internal/jsonx"
	"sort"
	"strings"
	"sync"

	"github.com/unisearch-db/unisearch/internal/engine/agg"
	"github.com/unisearch-db/unisearch/internal/engine/dsl"
	"github.com/unisearch-db/unisearch/internal/engine/plan"
	"github.com/unisearch-db/unisearch/internal/engine/script"
	"github.com/unisearch-db/unisearch/internal/engine/suggest"
	"github.com/unisearch-db/unisearch/internal/errors"
	"github.com/unisearch-db/unisearch/internal/index/analyzer"
	"github.com/unisearch-db/unisearch/internal/meta"
	"github.com/unisearch-db/unisearch/internal/observability"
	"github.com/unisearch-db/unisearch/internal/server/esapi"
	"github.com/unisearch-db/unisearch/internal/storage"
	"github.com/unisearch-db/unisearch/internal/tenant"
	"github.com/unisearch-db/unisearch/pkg/types"

	// Register all index kinds so flush can build/load inverted, scalar,
	// and HNSW indexes (D3: Index Registry declarative routing).
	_ "github.com/unisearch-db/unisearch/internal/index/hnsw"
	_ "github.com/unisearch-db/unisearch/internal/index/inverted"
	_ "github.com/unisearch-db/unisearch/internal/index/scalar"
)

// Engine implements esapi.Engine for the embedded mode using the real
// engine stack: storage.Shard for durability, HNSW for vector search,
// inverted index for BM25, and the Volcano executor for query pipelines.
type Engine struct {
	mu        sync.RWMutex
	store     *meta.Store
	manager   *indexManager
	dataDir   string
	tenantMgr *tenant.Manager // M1: multi-tenant resource management (nil = no quotas)
}

// New opens (or creates) an Engine rooted at dataDir. Meta files are
// read from {dataDir}/meta/*.json; shard data lives under
// {dataDir}/shards/. The engine recovers all existing indices on startup.
func New(dataDir string) (*Engine, error) {
	if dataDir == "" {
		return nil, fmt.Errorf("embedded: data dir required")
	}
	store, err := meta.OpenStore(dataDir + "/meta")
	if err != nil {
		return nil, fmt.Errorf("embedded: open meta store: %w", err)
	}
	mgr := newIndexManager(store, dataDir)
	if err := mgr.recoverAll(context.Background()); err != nil {
		return nil, fmt.Errorf("embedded: recover indices: %w", err)
	}
	// Ensure index sub-packages are registered.
	return &Engine{
		store:   store,
		manager: mgr,
		dataDir: dataDir,
	}, nil
}

// Close flushes all shards and closes the store. Idempotent.
func (e *Engine) Close() error {
	e.manager.closeAll()
	return nil
}

// ---------------------------------------------------------------------------
// esapi.Engine: Write path
// ---------------------------------------------------------------------------

func (e *Engine) Write(ctx context.Context, docReq esapi.IndexDoc, refresh string) (*esapi.WriteResult, error) {
	idx, err := e.manager.ensureIndex(ctx, docReq.Index)
	if err != nil {
		return nil, err
	}

	// Auto-generate _id when the client did not provide one.
	if docReq.ID == "" {
		var buf [16]byte
		if _, err := rand.Read(buf[:]); err != nil {
			return nil, fmt.Errorf("embedded: generate id: %w", err)
		}
		docReq.ID = hex.EncodeToString(buf[:])
	}

	// Inject _id into the document JSON so applyInsertFn can extract it.
	// The client specifies _id via the URL path (PUT /{index}/_doc/{id}),
	// but the JSON body may not contain an _id field.
	if len(docReq.Source) > 0 && docReq.Source[0] == '{' {
		var doc map[string]any
		if jsonx.Unmarshal(docReq.Source, &doc) == nil {
			if _, hasID := doc["_id"]; !hasID {
				doc["_id"] = docReq.ID
				if updated, err := jsonx.Marshal(doc); err == nil {
					docReq.Source = updated
				}
			}
		}
	}

	// Check for conflicts (if_seq_no + if_primary_term and OpCreate).
	// @spec: ./archive/2026-10-21-deep-gap-remediation/02_spec.md#if_primary_term
	snap, _ := idx.shard.Snapshot()
	if docReq.IfSeqNo != nil {
		if _, existingSeq, found := idx.shard.GetDocSource(ctx, docReq.ID, snap); found {
			if int64(existingSeq) != *docReq.IfSeqNo {
				return &esapi.WriteResult{
					Index: docReq.Index, ID: docReq.ID, Op: docReq.Op,
					Result: "conflict", SeqNo: int64(existingSeq),
				}, nil
			}
			// if_primary_term: in M0 embedded mode, primary_term is always 1.
			// Reject if the client expects a different term (design 04 §5).
			if docReq.IfPrimaryTerm != 0 && docReq.IfPrimaryTerm != 1 {
				return &esapi.WriteResult{
					Index: docReq.Index, ID: docReq.ID, Op: docReq.Op,
					Result: "conflict", SeqNo: int64(existingSeq),
				}, nil
			}
		}
	}
	if docReq.Op == esapi.OpCreate {
		if _, _, found := idx.shard.GetDocSource(ctx, docReq.ID, snap); found {
			return &esapi.WriteResult{
				Index: docReq.Index, ID: docReq.ID, Op: docReq.Op,
				Result: "conflict",
			}, nil
		}
	}

	// Write through the shard (WAL + memtable).
	var ops []storage.WriteOp
	if docReq.Op == esapi.OpDelete {
		ops = []storage.WriteOp{{Kind: storage.WriteDelete, ID: docReq.ID}}
	} else {
		// index / create / update all go through Insert (UPDATE is
		// handled as delete+insert at the WAL level by the shard).
		ops = []storage.WriteOp{{Kind: storage.WriteInsert, ID: docReq.ID, Doc: docReq.Source}}
	}
	seq, err := idx.shard.Write(ctx, ops)
	if err != nil {
		return nil, fmt.Errorf("embedded: write: %w", err)
	}

	// Update the mini inverted index so unflushed data is immediately
	// searchable (design 01 §2.1 "写入即可见").
	// @spec: ./archive/2026-10-21-deep-gap-remediation/02_spec.md#memtable-search
	if docReq.Op != esapi.OpDelete && idx.miniInv != nil && len(docReq.Source) > 0 {
		var doc map[string]any
		if jsonx.Unmarshal(docReq.Source, &doc) == nil {
			// Percolator registration: a document carrying a "query"
			// object is a percolator query (ES percolator field
			// convention); register it so percolate searches can match
			// documents against it.
			if idx.percolator != nil {
				if qraw, ok := doc["query"].(map[string]any); ok && len(qraw) > 0 {
					if qb, merr := jsonx.Marshal(qraw); merr == nil {
						_ = idx.percolator.Register(docReq.ID, qb)
					}
				}
			}
			// Use the seq number as a proxy for rowID in the mini inverted
			// index (the memtable assigns sequential row IDs).
			rowID := int32(seq)
			idx.mu.RLock()
			for fieldName, fieldID := range idx.fieldIDs {
				fd := idx.fieldDefs[fieldID]
				if fd == nil || fd.Type != meta.TypeText {
					continue
				}
				if val, ok := doc[fieldName]; ok {
					if text, ok := val.(string); ok && text != "" {
						az, err := analyzer.Get(fd.Analyzer, nil)
						if err != nil || az == nil {
							az, _ = analyzer.Get("standard", nil)
						}
						idx.miniInv.add(fieldID, text, rowID, az)
					}
				}
			}
			idx.mu.RUnlock()
		}
	}

	result := "created"
	if docReq.Op == esapi.OpUpdate {
		result = "updated"
	}

	// Handle refresh.
	if refresh == "true" || refresh == "wait_for" {
		_ = idx.shard.FlushAll(ctx)
	}

	return &esapi.WriteResult{
		Index:   docReq.Index,
		ID:      docReq.ID,
		Op:      docReq.Op,
		SeqNo:   int64(seq),
		Version: 1,
		Result:  result,
	}, nil
}

func (e *Engine) Bulk(ctx context.Context, docs []esapi.IndexDoc, refresh string) ([]esapi.WriteResult, error) {
	out := make([]esapi.WriteResult, 0, len(docs))
	for _, d := range docs {
		r, err := e.Write(ctx, d, "")
		if err != nil {
			out = append(out, esapi.WriteResult{Index: d.Index, ID: d.ID, Op: d.Op, Err: err})
			continue
		}
		out = append(out, *r)
	}
	// Handle refresh for the whole batch.
	if refresh == "true" || refresh == "wait_for" {
		for _, d := range docs {
			if idx, ok := e.manager.getIndex(d.Index); ok {
				_ = idx.shard.FlushAll(ctx)
			}
		}
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// esapi.Engine: Read path (point lookup)
// ---------------------------------------------------------------------------

func (e *Engine) Get(ctx context.Context, index, id string) ([]byte, int64, int64, bool, error) {
	idx, ok := e.manager.getIndex(index)
	if !ok {
		return nil, 0, 0, true, nil
	}
	snap, _ := idx.shard.Snapshot()
	source, seqNo, found := idx.shard.GetDocSource(ctx, id, snap)
	if !found {
		return nil, 0, 0, true, nil
	}
	return source, int64(seqNo), 1, false, nil
}

func (e *Engine) MGet(ctx context.Context, reqs []esapi.GetRequest) ([]esapi.GetResult, error) {
	out := make([]esapi.GetResult, 0, len(reqs))
	for _, r := range reqs {
		src, seq, prim, notFound, err := e.Get(ctx, r.Index, r.ID)
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

// ---------------------------------------------------------------------------
// esapi.Engine: Delete path
// ---------------------------------------------------------------------------

func (e *Engine) Delete(ctx context.Context, index, id string, ifSeqNo *int64, refresh string) (*esapi.WriteResult, error) {
	idx, ok := e.manager.getIndex(index)
	if !ok {
		return &esapi.WriteResult{Index: index, ID: id, Result: "not_found"}, nil
	}

	// Check if_seq_no.
	if ifSeqNo != nil {
		snap, _ := idx.shard.Snapshot()
		if _, existingSeq, found := idx.shard.GetDocSource(ctx, id, snap); found {
			if int64(existingSeq) != *ifSeqNo {
				return &esapi.WriteResult{
					Index: index, ID: id,
					Result: "conflict", SeqNo: int64(existingSeq),
				}, nil
			}
		}
	}

	_, err := idx.shard.Write(ctx, []storage.WriteOp{{Kind: storage.WriteDelete, ID: id}})
	if err != nil {
		return nil, fmt.Errorf("embedded: delete: %w", err)
	}

	// Unregister any percolator query stored under this document ID
	// (not-found is fine: ordinary documents were never registered).
	if idx.percolator != nil {
		_ = idx.percolator.Unregister(id)
	}

	if refresh == "true" || refresh == "wait_for" {
		_ = idx.shard.FlushAll(ctx)
	}

	return &esapi.WriteResult{
		Index:  index,
		ID:     id,
		Op:     esapi.OpDelete,
		Result: "deleted",
	}, nil
}

// ---------------------------------------------------------------------------
// esapi.Engine: Search path
// ---------------------------------------------------------------------------

func (e *Engine) Search(ctx context.Context, indices []string, body []byte) (*esapi.SearchResult, error) {
	// OTel: inject plan-level span (design 06 §2, level 2 of 4).
	// @spec: ./active_specs/2026-10-20-gap-fill/02_spec.md#2.2
	ctx, planSpan := observability.StartPlanSpan(ctx)
	defer planSpan.End()

	q, from, size, extras, err := dsl.ParseSearchBodyFull(body)
	if err != nil {
		return nil, err
	}

	// M1: When aggregations are requested, fetch ALL matching documents
	// (not just the size-limited page) so size:0 still produces correct
	// aggregation results (ES semantics: aggs run on all matches).
	// @spec: ./archive/2026-10-20-m1-es-compat-ga/02_spec.md#2.2.3
	searchSize := size
	if len(extras.Aggs) > 0 && size == 0 {
		searchSize = 10000 // fetch all for aggregation
	}
	// track_total_hits with a numeric value N: fetch at least N docs
	// so the total count can be capped accurately. When track_total_hits
	// is true (boolean), fetch all docs to get exact count.
	// @spec: ./archive/2026-10-21-deep-gap-remediation/02_spec.md#track_total_hits
	if len(extras.TrackTotalHits) > 0 {
		var tth any
		if jsonx.Unmarshal(extras.TrackTotalHits, &tth) == nil {
			switch v := tth.(type) {
			case bool:
				if v && 10000 > searchSize {
					searchSize = 10000
				}
			case float64:
				if int(v) > searchSize {
					searchSize = int(v)
				}
			}
		}
	}

	// Resolve target indices.
	targets := indices
	if len(targets) == 0 {
		targets = make([]string, 0)
		for _, idx := range e.manager.allIndices() {
			targets = append(targets, idx.name)
		}
	}

	// Resolve aliases: if an index name is actually an alias, replace
	// it with the real index name. This also serves as existence check.
	resolvedTargets := make([]string, 0, len(targets))
	for _, t := range targets {
		if _, ok := e.manager.getIndex(t); ok {
			resolvedTargets = append(resolvedTargets, t)
			continue
		}
		// Check if it's an alias
		if aliases, err := e.GetAliases(ctx, ""); err == nil {
			found := false
			for indexName, aliasList := range aliases {
				for _, a := range aliasList {
					if a == t {
						resolvedTargets = append(resolvedTargets, indexName)
						found = true
						break
					}
				}
				if found {
					break
				}
			}
		}
	}
	// If no targets resolved and original targets were specific (non-wildcard),
	// return index_not_found so the handler returns 404.
	if len(resolvedTargets) == 0 && len(targets) > 0 {
		hasWildcard := false
		for _, t := range targets {
			if strings.ContainsAny(t, "*?") {
				hasWildcard = true
				break
			}
		}
		if !hasWildcard {
			return nil, fmt.Errorf("index_not_found: %s", targets[0])
		}
	}
	targets = resolvedTargets

	var allHits []esapi.SearchHit
	total := 0
	maxScore := float32(0)

	for _, indexName := range targets {
		idx, ok := e.manager.getIndex(indexName)
		if !ok {
			continue
		}
		snap, segSet := idx.shard.Snapshot()

		// Translate DSL → per-segment plans.
		// Pass from=0 and size=from+searchSize so the segment plans return
		// enough results; the global from/size pagination is applied once
		// after cross-index merging below.
		plans, err := translateQuery(q, idx, snap, segSet, 0, from+searchSize)
		if err != nil {
			return nil, err
		}

		// Optimize each per-segment plan.
		for i, p := range plans {
			plans[i], _ = plan.Optimize(p, plan.EstimatedSelectivity{}, from+searchSize)
		}

		// Execute and merge.
		hits, cnt, err := buildAndExecute(ctx, plans, idx, snap, segSet, 0, from+searchSize, nil, q)
		if err != nil {
			return nil, err
		}

		// Assemble SearchHits.
		results := assembleHits(hits, segSet, idx)
		for _, r := range results {
			allHits = append(allHits, esapi.SearchHit{
				Index:  indexName,
				ID:     r.id,
				Score:  r.score,
				Source: r.source,
			})
			if r.score > maxScore {
				maxScore = r.score
			}
		}
		total += cnt
	}

	// Post-filter hits for query types that the segment execution path
	// can't handle (bool with term/range sub-queries, exists, terms, range).
	// This checks each hit's _source against the original DSL query.
	if q != nil && len(allHits) > 0 {
		allHits = postFilterHits(allHits, q)
		total = len(allHits)
	}

	// script_score: re-score the merged hits by evaluating the script
	// against each hit's _source (the inner query's score is exposed to
	// the script as _score), then apply min_score filtering. Doing this
	// once on the merged hits keeps the memtable and segment paths on a
	// single scoring implementation.
	// @spec: ./active_specs/2026-10-22-es-feature-gap-fill/02_spec.md#2.7
	if ss, ok := q.(*dsl.ScriptScoreQuery); ok && ss != nil && len(allHits) > 0 {
		allHits = applyScriptScore(ss, allHits)
		total = len(allHits)
		maxScore = 0
		for _, h := range allHits {
			if h.Score > maxScore {
				maxScore = h.Score
			}
		}
	}

	// terminate_after: limit the total number of documents collected
	// (design 03 §8: "行数阈值提前终止"). When set, truncate hits
	// to this count and update total accordingly.
	// @spec: ./archive/2026-10-21-deep-gap-remediation/02_spec.md#terminate_after
	if extras.TerminateAfter > 0 && len(allHits) > extras.TerminateAfter {
		allHits = allHits[:extras.TerminateAfter]
		total = extras.TerminateAfter
	}

	// Sort all hits by score descending.
	sort.Slice(allHits, func(i, j int) bool {
		return allHits[i].Score > allHits[j].Score
	})

	// M1: Run aggregations on ALL matching documents (before pagination),
	// so size:0 still produces correct aggregation results (ES semantics).
	// @spec: ./archive/2026-10-20-m1-es-compat-ga/02_spec.md#2.2.3
	var aggResults map[string]any
	if len(extras.Aggs) > 0 {
		aggResults = e.runAggregations(extras.Aggs, allHits)
	}

	// P0: post_filter — filter hits after aggregations (ES semantics:
	// aggs run on all matching docs; post_filter reduces hits but
	// does not affect aggs or total count).
	// @spec: ./active_specs/2026-10-22-es-feature-gap-fill/02_spec.md#2.3.3
	if len(extras.PostFilter) > 0 && len(allHits) > 0 {
		pfQuery, pfErr := dsl.ParseQuery(extras.PostFilter)
		if pfErr == nil && pfQuery != nil {
			allHits = filterByPostFilter(allHits, pfQuery)
		}
	}

	// P0: rescore — re-score top-N hits within the window using a
	// secondary query, blending primary and rescore scores.
	// @spec: ./active_specs/2026-10-22-es-feature-gap-fill/02_spec.md#2.3.1
	if len(extras.Rescore) > 0 && len(allHits) > 0 {
		allHits = applyRescore(allHits, extras.Rescore)
	}

	// P0: collapse — group hits by a field value, keeping only the
	// top hit per group (ES semantics: collapse is applied before
	// from/size pagination).
	// @spec: ./active_specs/2026-10-22-es-feature-gap-fill/02_spec.md#2.3.2
	if len(extras.Collapse) > 0 && len(allHits) > 0 {
		allHits = applyCollapse(allHits, extras.Collapse)
		total = len(allHits) // collapse reduces total
	}

	// Apply pagination (from/size already applied in buildAndExecute,
	// but we re-apply here for cross-index merging).
	start := from
	if start > len(allHits) {
		start = len(allHits)
	}
	end := start + size
	if end > len(allHits) {
		end = len(allHits)
	}
	pagedHits := allHits[start:end]

	result := &esapi.SearchResult{
		Took:     0,
		TimedOut: false,
		Shards:   esapi.ShardStats{Total: 1, Successful: 1},
		Total:    total,
		Relation: "eq",
		MaxScore: &maxScore,
		Hits:     pagedHits,
	}

	// track_total_hits: false → skip exact count; N → cap at N.
	// @spec: ./archive/2026-10-21-deep-gap-remediation/02_spec.md#2.2.6
	if len(extras.TrackTotalHits) > 0 {
		var tth any
		if jsonx.Unmarshal(extras.TrackTotalHits, &tth) == nil {
			switch v := tth.(type) {
			case bool:
				if !v {
					result.Total = 0 // don't count
				}
			case float64:
				cap := int(v)
				if result.Total > cap {
					result.Total = cap
					result.Relation = "gte"
				}
			}
		}
	}

	if aggResults != nil {
		result.Aggregations = aggResults
	}

	// M1: Run highlight on the paginated hits only (ES highlights only
	// the returned hits, not all matches).
	// @spec: ./archive/2026-10-20-m1-es-compat-ga/02_spec.md#2.2.5
	if len(extras.Highlight) > 0 {
		result.Highlight = e.runHighlight(extras.Highlight, pagedHits, q)
	}

	// M1: Run suggesters (term/phrase/completion) against the target
	// indices' term dictionaries.
	// @spec: ./archive/2026-10-20-m1-es-compat-ga/02_spec.md#2.2.5
	if len(extras.Suggest) > 0 {
		result.Suggest = e.runSuggest(extras.Suggest, targets)
	}

	// P2: Evaluate script_fields and runtime_mappings for each hit.
	// @spec: ./active_specs/2026-10-22-es-feature-gap-fill/02_spec.md#2.3.6
	if len(extras.ScriptFields) > 0 || len(extras.RuntimeMappings) > 0 {
		for i := range pagedHits {
			src := decodeSource(pagedHits[i].Source)
			if src == nil {
				continue
			}
			if pagedHits[i].Fields == nil {
				pagedHits[i].Fields = make(map[string]any)
			}
			// script_fields: evaluate each script field.
			if len(extras.ScriptFields) > 0 {
				var sfMap map[string]jsonx.RawMessage
				if jsonx.Unmarshal(extras.ScriptFields, &sfMap) == nil {
					for fieldName, raw := range sfMap {
						var sf struct {
							Script struct {
								Source string `json:"source"`
							} `json:"script"`
						}
						if jsonx.Unmarshal(raw, &sf) == nil && sf.Script.Source != "" {
							pagedHits[i].Fields[fieldName] = evalScriptField(sf.Script.Source, src, nil, float64(pagedHits[i].Score))
						}
					}
				}
			}
			// runtime_mappings: evaluate each runtime field.
			if len(extras.RuntimeMappings) > 0 {
				var rmMap map[string]jsonx.RawMessage
				if jsonx.Unmarshal(extras.RuntimeMappings, &rmMap) == nil {
					for fieldName, raw := range rmMap {
						var rm struct {
							Type   string `json:"type"`
							Script struct {
								Source string `json:"source"`
							} `json:"script"`
						}
						if jsonx.Unmarshal(raw, &rm) == nil && rm.Script.Source != "" {
							pagedHits[i].Fields[fieldName] = evalScriptField(rm.Script.Source, src, nil, float64(pagedHits[i].Score))
						}
					}
				}
			}
		}
	}

	return result, nil
}

// evalScriptField evaluates a Painless-subset script expression against
// a document using the script engine (doc['field'].value, _score,
// params, Math.*, arithmetic/logic/ternary, vector functions), with the
// ES-aligned 100k step budget. Returns nil when the script cannot be
// parsed or evaluated.
// @spec: ./active_specs/2026-10-22-es-feature-gap-fill/02_spec.md#2.3.6
func evalScriptField(source string, doc map[string]any, params map[string]any, score float64) any {
	scr, err := script.Parse(source)
	if err != nil {
		return nil
	}
	v, err := scr.Eval(&script.ScriptCtx{Doc: doc, Score: score, Params: params})
	if err != nil {
		return nil
	}
	return v
}

// applyScriptScore re-scores each hit by evaluating the script_score
// script against the hit's _source (the inner query score is passed to
// the script as _score) and drops hits below min_score.
// @spec: ./active_specs/2026-10-22-es-feature-gap-fill/02_spec.md#2.7
func applyScriptScore(q *dsl.ScriptScoreQuery, hits []esapi.SearchHit) []esapi.SearchHit {
	scr, err := script.Parse(q.Script)
	if err != nil {
		// ES rejects unparseable scripts at request validation; keep the
		// inner scores here so search degrades to the base query rather
		// than failing the whole request.
		return hits
	}
	var out []esapi.SearchHit
	for _, h := range hits {
		src := decodeSource(h.Source)
		if src == nil {
			continue
		}
		v, err := scr.EvalFloat(&script.ScriptCtx{Doc: src, Score: float64(h.Score), Params: q.Params})
		if err != nil {
			continue
		}
		if q.MinScore > 0 && v < q.MinScore {
			continue
		}
		h.Score = float32(v)
		out = append(out, h)
	}
	return out
}

// ---------------------------------------------------------------------------
// Suggesters (term / phrase / completion, design 03 §10)
// ---------------------------------------------------------------------------

// suggestSpec is one named suggester in the "suggest" search body
// section, e.g. {"text": "tring", "term": {"field": "title"}}.
type suggestSpec struct {
	Prefix     string       `json:"prefix"`
	Text       string       `json:"text"`
	Term       *suggestBody `json:"term"`
	Phrase     *suggestBody `json:"phrase"`
	Completion *suggestBody `json:"completion"`
}

// suggestBody carries the per-suggester options (field, size, max_edits).
type suggestBody struct {
	Field    string `json:"field"`
	Size     int    `json:"size"`
	MaxEdits int    `json:"max_edits"`
}

// suggestOption is one option in an ES-shaped suggestion entry.
type suggestOption struct {
	Text  string  `json:"text"`
	Score float64 `json:"score"`
	Freq  int     `json:"freq,omitempty"`
}

// suggestEntry is one ES-shaped suggestion entry (term suggesters emit
// one entry per input token; phrase/completion emit one for the text).
type suggestEntry struct {
	Text    string          `json:"text"`
	Offset  int             `json:"offset"`
	Length  int             `json:"length"`
	Options []suggestOption `json:"options"`
}

// runSuggest evaluates the "suggest" section of a search body against
// the target indices' term dictionaries and returns the ES-shaped
// "suggest" response block.
// @spec: ./archive/2026-10-20-m1-es-compat-ga/02_spec.md#2.2.5
func (e *Engine) runSuggest(raw jsonx.RawMessage, targets []string) map[string]any {
	var specs map[string]suggestSpec
	if jsonx.Unmarshal(raw, &specs) != nil || len(specs) == 0 {
		return nil
	}
	out := make(map[string]any, len(specs))
	for name, spec := range specs {
		var body *suggestBody
		switch {
		case spec.Completion != nil:
			body = spec.Completion
		case spec.Phrase != nil:
			body = spec.Phrase
		case spec.Term != nil:
			body = spec.Term
		default:
			continue
		}
		terms := e.collectSuggestTerms(targets, body.Field)
		var entries []suggestEntry
		switch {
		case spec.Completion != nil:
			entries = runCompletionSuggester(spec, body, terms)
		case spec.Phrase != nil:
			entries = runPhraseSuggester(spec, body, terms)
		case spec.Term != nil:
			entries = runTermSuggester(spec, body, terms)
		}
		if len(entries) > 0 {
			out[name] = entries
		}
	}
	return out
}

// collectSuggestTerms merges the term→docFreq dictionary of one field
// across the target indices (real-time memtable terms via miniInv).
func (e *Engine) collectSuggestTerms(targets []string, field string) map[string]int {
	terms := make(map[string]int)
	for _, name := range targets {
		idx, ok := e.manager.getIndex(name)
		if !ok || idx.miniInv == nil || field == "" {
			continue
		}
		fid, _, found := idx.resolveField(field)
		if !found {
			continue
		}
		for t, df := range idx.miniInv.fieldTerms(fid) {
			terms[t] += df
		}
	}
	return terms
}

// runTermSuggester runs the term suggester: one entry per input token,
// each with edit-distance suggestions ranked by frequency.
func runTermSuggester(spec suggestSpec, body *suggestBody, terms map[string]int) []suggestEntry {
	ts := suggest.NewTermSuggester(terms)
	size := body.Size
	if size <= 0 {
		size = 5
	}
	maxEdits := body.MaxEdits
	if maxEdits <= 0 {
		maxEdits = 2
	}
	var entries []suggestEntry
	offset := 0
	for _, word := range strings.Fields(spec.Text) {
		entry := suggestEntry{Text: word, Offset: offset, Length: len(word)}
		for _, s := range ts.Suggest(word, maxEdits) {
			if len(entry.Options) >= size {
				break
			}
			entry.Options = append(entry.Options, suggestOption{Text: s.Text, Score: s.Score, Freq: s.Frequency})
		}
		entries = append(entries, entry)
		offset += len(word) + 1
	}
	return entries
}

// runPhraseSuggester runs the phrase suggester: corrects each unknown
// word to its best term suggestion and returns one corrected-phrase
// entry.
func runPhraseSuggester(spec suggestSpec, body *suggestBody, terms map[string]int) []suggestEntry {
	ps := suggest.NewPhraseSuggester(suggest.NewTermSuggester(terms))
	maxEdits := body.MaxEdits
	if maxEdits <= 0 {
		maxEdits = 2
	}
	entry := suggestEntry{Text: spec.Text, Offset: 0, Length: len(spec.Text)}
	for _, s := range ps.Suggest(spec.Text, maxEdits) {
		entry.Options = append(entry.Options, suggestOption{Text: s.Text, Score: s.Score})
	}
	return []suggestEntry{entry}
}

// runCompletionSuggester runs the completion suggester: prefix-match
// autocomplete over the field's terms.
func runCompletionSuggester(spec suggestSpec, body *suggestBody, terms map[string]int) []suggestEntry {
	entries := make([]suggest.CompletionEntry, 0, len(terms))
	for t, df := range terms {
		entries = append(entries, suggest.CompletionEntry{Text: t, Score: float64(df)})
	}
	cs := suggest.NewCompletionSuggester(entries)
	prefix := spec.Prefix
	if prefix == "" {
		prefix = spec.Text
	}
	size := body.Size
	if size <= 0 {
		size = 5
	}
	entry := suggestEntry{Text: prefix, Offset: 0, Length: len(prefix)}
	for _, s := range cs.Suggest(prefix, size) {
		entry.Options = append(entry.Options, suggestOption{Text: s.Text, Score: s.Score, Freq: int(s.Score)})
	}
	return []suggestEntry{entry}
}

// runHighlight produces per-field highlighted fragments for the
// search hits using the unified highlighter (design 02 §3.5).
// @spec: ./archive/2026-10-20-m1-es-compat-ga/02_spec.md#2.2.5
func (e *Engine) runHighlight(highlightJSON jsonx.RawMessage, hits []esapi.SearchHit, q dsl.Query) map[string]map[string][]string {
	var hlDef map[string]jsonx.RawMessage
	if err := jsonx.Unmarshal(highlightJSON, &hlDef); err != nil || len(hlDef) == 0 {
		return nil
	}
	// Parse highlight fields and their config.
	fields := make(map[string]map[string]any)
	for field, cfg := range hlDef {
		if field == "pre_tags" || field == "post_tags" || field == "fragment_size" || field == "number_of_fragments" {
			continue // global highlight options
		}
		var m map[string]any
		if len(cfg) > 0 {
			jsonx.Unmarshal(cfg, &m)
		}
		fields[field] = m
	}
	if len(fields) == 0 {
		return nil
	}

	// Extract query terms from the DSL query AST for highlighting.
	queryTerms := extractQueryTerms(q)
	if len(queryTerms) == 0 {
		// Fallback: extract terms from hit _source text fields.
		for _, hit := range hits {
			queryTerms = append(queryTerms, extractTermsFromHit(hit)...)
		}
	}

	// Simple inline highlight: wrap query terms in <em> tags.
	result := make(map[string]map[string][]string, len(hits))
	for _, hit := range hits {
		var src map[string]any
		if jsonx.Unmarshal(hit.Source, &src) != nil {
			continue
		}
		fieldResults := make(map[string][]string)
		for field := range fields {
			val, ok := src[field]
			if !ok {
				continue
			}
			text, ok := val.(string)
			if !ok {
				text = fmt.Sprintf("%v", val)
			}
			// Simple highlight: find query terms and wrap in <em>.
			highlighted := text
			for _, term := range queryTerms {
				if term == "" {
					continue
				}
				lower := strings.ToLower(highlighted)
				lterm := strings.ToLower(term)
				idx := strings.Index(lower, lterm)
				if idx >= 0 {
					end := idx + len(lterm)
					highlighted = highlighted[:idx] + "<em>" + highlighted[idx:end] + "</em>" + highlighted[end:]
				}
			}
			if highlighted != text {
				fieldResults[field] = []string{highlighted}
			}
		}
		if len(fieldResults) > 0 {
			result[hit.ID] = fieldResults
		}
	}
	return result
}

// extractQueryTerms extracts highlight terms from the DSL query AST.
// For match queries, the text is split into words. For term queries,
// the value is used directly. For bool queries, all sub-clauses are
// recursed.
func extractQueryTerms(q dsl.Query) []string {
	if q == nil {
		return nil
	}
	switch typed := q.(type) {
	case *dsl.MatchQuery:
		return splitTerms(typed.Text)
	case *dsl.MatchPhraseQuery:
		return splitTerms(typed.Text)
	case *dsl.TermQuery:
		return []string{fmt.Sprintf("%v", typed.Value)}
	case *dsl.MultiMatchQuery:
		return splitTerms(typed.Text)
	case *dsl.QueryStringQuery:
		return splitQueryStringTerms(typed.Query)
	case *dsl.BoolQuery:
		var terms []string
		for _, clause := range typed.Must {
			terms = append(terms, extractQueryTerms(clause)...)
		}
		for _, clause := range typed.Should {
			terms = append(terms, extractQueryTerms(clause)...)
		}
		for _, clause := range typed.Filter {
			terms = append(terms, extractQueryTerms(clause)...)
		}
		return terms
	case *dsl.MatchAllQuery:
		return nil
	default:
		return nil
	}
}

// splitTerms tokenizes text into individual lowercase words for highlighting.
func splitTerms(text string) []string {
	if text == "" {
		return nil
	}
	return strings.Fields(strings.ToLower(text))
}

// splitQueryStringTerms extracts terms from a query_string query,
// splitting on AND/OR/NOT and whitespace.
func splitQueryStringTerms(qs string) []string {
	qs = strings.ReplaceAll(qs, " AND ", " ")
	qs = strings.ReplaceAll(qs, " and ", " ")
	qs = strings.ReplaceAll(qs, " OR ", " ")
	qs = strings.ReplaceAll(qs, " or ", " ")
	qs = strings.ReplaceAll(qs, " NOT ", " ")
	qs = strings.ReplaceAll(qs, " not ", " ")
	return splitTerms(qs)
}

// extractTermsFromHit extracts query terms from a search hit for
// highlighting. Uses the hit's _source fields as a simplified
// approach; M2 can extract from the actual query AST.
func extractTermsFromHit(hit esapi.SearchHit) []string {
	var src map[string]any
	if jsonx.Unmarshal(hit.Source, &src) != nil {
		return nil
	}
	var terms []string
	for _, v := range src {
		if s, ok := v.(string); ok {
			terms = append(terms, s)
		}
	}
	return terms
}

// runAggregations executes the aggregation definitions against the
// search hits and returns ES-style aggregation results.
// @spec: ./archive/2026-10-20-m1-es-compat-ga/02_spec.md#2.2.3
func (e *Engine) runAggregations(aggsJSON jsonx.RawMessage, hits []esapi.SearchHit) map[string]any {
	var aggsDef map[string]jsonx.RawMessage
	if err := jsonx.Unmarshal(aggsJSON, &aggsDef); err != nil || len(aggsDef) == 0 {
		return nil
	}
	results := make(map[string]any, len(aggsDef))
	for name, def := range aggsDef {
		// Parse the aggregation definition to find the type.
		var raw map[string]jsonx.RawMessage
		if jsonx.Unmarshal(def, &raw) != nil {
			continue
		}
		for aggType, aggParams := range raw {
			if aggType == "aggs" {
				continue // sub-aggregations, handled recursively later
			}
			params := make(map[string]any)
			if len(aggParams) > 0 {
				jsonx.Unmarshal(aggParams, &params)
			}
			aggregator, err := agg.NewAggregator(aggType, params)
			if err != nil {
				continue // unknown agg type, skip
			}
			// Feed each hit's field value to the aggregator.
			fieldName := ""
			if f, ok := params["field"].(string); ok {
				fieldName = f
			}
			for _, hit := range hits {
				var src map[string]any
				if jsonx.Unmarshal(hit.Source, &src) == nil {
					aggregator.Collect(src[fieldName])
				}
			}
			// For scalar metric aggs (sum, avg, etc.), wrap in "value".
			// For complex metric aggs (stats, extended_stats), use directly.
			// For bucket aggs (terms, range, etc.), use "buckets" directly.
			// @spec: ./archive/2026-10-21-deep-gap-remediation/02_spec.md#aggregation
			aggResult := aggregator.Result()
			switch v := aggResult.(type) {
			case float64:
				// Scalar result (sum, avg, min, max, cardinality, value_count)
				results[name] = map[string]any{"value": v}
			case map[string]any:
				if _, hasBuckets := v["buckets"]; hasBuckets {
					// Bucket aggregation — use directly, and execute sub-aggs per bucket.
					results[name] = v
					// Execute sub-aggregations on each bucket.
					if subAggs, hasSubAggs := raw["aggs"]; hasSubAggs {
						runSubAggregations(v, subAggs, hits, params)
					}
				} else {
					// Complex metric agg (stats, extended_stats) — use directly.
					results[name] = v
				}
			default:
				results[name] = map[string]any{"value": aggResult}
			}
			break // only one agg type per definition
		}
	}
	return results
}

// runSubAggregations executes sub-aggregations for each bucket in a
// bucket aggregation result. For each bucket, it filters the hits
// that belong to that bucket and runs the sub-aggregations on them.
func runSubAggregations(bucketResult map[string]any, subAggsJSON jsonx.RawMessage, allHits []esapi.SearchHit, bucketParams map[string]any) {
	buckets, ok := bucketResult["buckets"].([]map[string]any)
	if !ok {
		return
	}
	// Get the field name for bucket grouping.
	fieldName, _ := bucketParams["field"].(string)
	// Parse sub-aggregation definitions.
	var subAggsDef map[string]jsonx.RawMessage
	if jsonx.Unmarshal(subAggsJSON, &subAggsDef) != nil || len(subAggsDef) == 0 {
		return
	}
	for i := range buckets {
		bucket := buckets[i]
		key, _ := bucket["key"].(string)
		// Filter hits for this bucket.
		var bucketHits []esapi.SearchHit
		for _, hit := range allHits {
			var src map[string]any
			if jsonx.Unmarshal(hit.Source, &src) == nil {
				if v, ok := src[fieldName]; ok {
					if s, ok := v.(string); ok && s == key {
						bucketHits = append(bucketHits, hit)
					} else if fmt.Sprintf("%v", v) == key {
						bucketHits = append(bucketHits, hit)
					}
				}
			}
		}
		// Run sub-aggregations on bucket hits.
		subResults := make(map[string]any)
		for subName, subDef := range subAggsDef {
			var subRaw map[string]jsonx.RawMessage
			if jsonx.Unmarshal(subDef, &subRaw) != nil {
				continue
			}
			for aggType, aggParams := range subRaw {
				if aggType == "aggs" {
					continue
				}
				params := make(map[string]any)
				if len(aggParams) > 0 {
					jsonx.Unmarshal(aggParams, &params)
				}
				aggregator, err := agg.NewAggregator(aggType, params)
				if err != nil {
					continue
				}
				subField, _ := params["field"].(string)
				for _, hit := range bucketHits {
					var src map[string]any
					if jsonx.Unmarshal(hit.Source, &src) == nil {
						aggregator.Collect(src[subField])
					}
				}
				aggResult := aggregator.Result()
				switch v := aggResult.(type) {
				case float64:
					subResults[subName] = map[string]any{"value": v}
				case map[string]any:
					if _, hasBuckets := v["buckets"]; hasBuckets {
						subResults[subName] = v
					} else {
						subResults[subName] = v
					}
				default:
					subResults[subName] = map[string]any{"value": aggResult}
				}
				break
			}
		}
		// Merge sub-aggregation results into the bucket.
		for k, v := range subResults {
			bucket[k] = v
		}
	}
}

func (e *Engine) Count(ctx context.Context, indices []string, body []byte) (int64, error) {
	// Default to match_all when no body is provided (GET /_count, GET /{index}/_count).
	if len(body) == 0 {
		body = []byte(`{"query":{"match_all":{}}}`)
	}
	result, err := e.Search(ctx, indices, body)
	if err != nil {
		return 0, err
	}
	return int64(result.Total), nil
}

// ---------------------------------------------------------------------------
// esapi.Engine: Mapping & Settings
// ---------------------------------------------------------------------------

func (e *Engine) PutMapping(ctx context.Context, index string, body []byte) error {
	idx, err := e.manager.ensureIndex(ctx, index)
	if err != nil {
		return err
	}
	// Parse the mapping body (ES format: {"mappings": {...}} or bare mapping).
	var spec struct {
		Mappings *meta.Mapping  `json:"mappings"`
		Settings map[string]any `json:"settings"`
	}
	if err := jsonx.Unmarshal(body, &spec); err != nil {
		return errors.NewDocumentParsing("invalid mapping body: " + err.Error())
	}
	// Load existing meta, update mapping, save.
	m, err := e.store.Load(index)
	if err != nil {
		return err
	}
	if spec.Mappings != nil {
		m.Mapping = spec.Mappings
	}
	// Update fieldIDs for new fields in the indexState.
	idx.mu.Lock()
	for name, fd := range m.Mapping.Properties {
		if _, exists := idx.fieldIDs[name]; !exists {
			fd.Name = name
			nextID := types.FieldID(2)
			for fid := range idx.fieldDefs {
				if fid >= nextID {
					nextID = fid + 1
				}
			}
			idx.fieldIDs[name] = nextID
			idx.fieldDefs[nextID] = fd
		}
	}
	// Always sync m.FieldIDs with the current idx.fieldIDs so that
	// dynamically added and mapping-introduced fields are persisted.
	m.FieldIDs = idx.fieldIDs
	idx.mu.Unlock()
	return e.store.Save(m)
}

// DeleteIndex removes an index, its shard data, and meta from the store.
func (e *Engine) DeleteIndex(ctx context.Context, index string) error {
	return e.manager.deleteIndex(ctx, index)
}

func (e *Engine) GetMapping(ctx context.Context, index string) ([]byte, error) {
	idx, ok := e.manager.getIndex(index)
	if !ok {
		return nil, errors.NewIndexNotFound(index)
	}
	_ = idx
	m, err := e.store.Load(index)
	if err != nil {
		return nil, err
	}
	body := map[string]any{
		index: map[string]any{
			"mappings": m.Mapping,
		},
	}
	return jsonx.Marshal(body)
}

func (e *Engine) PutSettings(ctx context.Context, index string, body []byte) error {
	idx, err := e.manager.ensureIndex(ctx, index)
	if err != nil {
		return err
	}
	_ = idx
	m, err := e.store.Load(index)
	if err != nil {
		return err
	}
	var spec struct {
		NumberOfShards   int    `json:"number_of_shards"`
		NumberOfReplicas int    `json:"number_of_replicas"`
		RefreshInterval  string `json:"refresh_interval"`
	}
	if err := jsonx.Unmarshal(body, &spec); err == nil {
		m.Settings.NumberOfShards = spec.NumberOfShards
		m.Settings.NumberOfReplicas = spec.NumberOfReplicas
		if spec.RefreshInterval != "" {
			m.Settings.RefreshInterval = spec.RefreshInterval
		}
	}
	return e.store.Save(m)
}

func (e *Engine) GetSettings(ctx context.Context, index string) ([]byte, error) {
	idx, ok := e.manager.getIndex(index)
	if !ok {
		return nil, errors.NewIndexNotFound(index)
	}
	_ = idx
	m, err := e.store.Load(index)
	if err != nil {
		return nil, err
	}
	return jsonx.Marshal(map[string]any{
		index: map[string]any{"settings": m.Settings},
	})
}

// ---------------------------------------------------------------------------
// esapi.Engine: Aliases
// ---------------------------------------------------------------------------

func (e *Engine) PutAlias(ctx context.Context, index, alias string) error {
	m, err := e.store.Load(index)
	if err != nil {
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

// @spec: ./archive/2026-10-21-deep-gap-remediation/02_spec.md#B4
func (e *Engine) RemoveAlias(ctx context.Context, index, alias string) error {
	m, err := e.store.Load(index)
	if err != nil {
		return errors.NewIndexNotFound(index)
	}
	for i, a := range m.Aliases {
		if a == alias {
			m.Aliases = append(m.Aliases[:i], m.Aliases[i+1:]...)
			return e.store.Save(m)
		}
	}
	return nil // idempotent: alias not found is a no-op
}

func (e *Engine) GetAliases(ctx context.Context, index string) (map[string][]string, error) {
	out := map[string][]string{}
	for _, idx := range e.manager.allIndices() {
		if index != "" && idx.name != index {
			continue
		}
		m, err := e.store.Load(idx.name)
		if err != nil {
			continue
		}
		if len(m.Aliases) > 0 {
			out[idx.name] = append([]string(nil), m.Aliases...)
		}
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// esapi.Engine: Analyze
// ---------------------------------------------------------------------------

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
		offset += len(p) + 1
	}
	return tokens, nil
}

// ---------------------------------------------------------------------------
// esapi.Engine: Admin
// ---------------------------------------------------------------------------

func (e *Engine) Refresh(ctx context.Context, index string) error {
	idx, ok := e.manager.getIndex(index)
	if !ok {
		return errors.NewIndexNotFound(index)
	}
	return idx.shard.FlushAll(ctx)
}

func (e *Engine) Indices(ctx context.Context, _ string) ([]esapi.IndexInfo, error) {
	infos := make([]esapi.IndexInfo, 0)
	for _, idx := range e.manager.allIndices() {
		snap, segSet := idx.shard.Snapshot()
		docsCount := int64(0)
		if segSet != nil {
			for _, entry := range segSet.Items() {
				if entry.Reader != nil {
					docsCount += int64(entry.Reader.NumRows())
				}
			}
		}
		_ = snap
		infos = append(infos, esapi.IndexInfo{
			Health:    "green",
			Status:    "open",
			Index:     idx.name,
			UUID:      idx.name,
			Primaries: 1,
			Replicas:  0,
			DocsCount: docsCount,
			StoreSize: "0kb",
		})
	}
	sort.Slice(infos, func(i, j int) bool { return infos[i].Index < infos[j].Index })
	return infos, nil
}

func (e *Engine) Health(_ context.Context) (*esapi.ClusterHealth, error) {
	// Calculate health based on shard availability (design 06 §4).
	// green: all indices have at least one readable shard.
	// yellow: some indices missing but at least one alive.
	// red: no indices or all shards down.
	// @spec: ./archive/2026-10-21-deep-gap-remediation/02_spec.md#4.4
	allIndices := e.manager.allIndices()
	activeShards := 0
	totalShards := 0
	for _, idx := range allIndices {
		if idx.shard != nil {
			totalShards++
			// In embedded mode, shard is always active (single node).
			activeShards++
		}
	}
	status := "green"
	if totalShards > 0 && activeShards < totalShards {
		status = "yellow"
	}
	if totalShards == 0 && len(allIndices) > 0 {
		status = "red"
	}
	return &esapi.ClusterHealth{
		ClusterName:         "unisearch",
		Status:              status,
		NumberOfNodes:       1,
		NumberOfDataNodes:   1,
		ActivePrimaryShards: activeShards,
		ActiveShards:        activeShards,
	}, nil
}

// CurrentSnapshot returns the current visibility watermark across
// the given indices' shards, used by PIT to pin a consistent read view.
// @spec: ./archive/2026-10-21-deep-gap-remediation/02_spec.md#2.2.1
func (e *Engine) CurrentSnapshot(_ context.Context, indices []string) (types.Snapshot, error) {
	var maxSeq types.SeqNo
	if len(indices) == 0 {
		// All indices: iterate all managed indices.
		for _, idx := range e.manager.allIndices() {
			if idx.shard != nil {
				if s := idx.shard.VisibleSeqNo(); s > maxSeq {
					maxSeq = s
				}
			}
		}
	} else {
		for _, name := range indices {
			if idx, ok := e.manager.getIndex(name); ok && idx.shard != nil {
				if s := idx.shard.VisibleSeqNo(); s > maxSeq {
					maxSeq = s
				}
			}
		}
	}
	return types.NewSnapshot(maxSeq), nil
}

// postFilterHits filters hits by checking each hit's _source against
// the original DSL query. This handles query types (bool must/must_not,
// range, exists, terms) that the segment execution path can't evaluate
// because column data is not available for keyword/text/float fields.
func postFilterHits(hits []esapi.SearchHit, q dsl.Query) []esapi.SearchHit {
	switch typed := q.(type) {
	case *dsl.BoolQuery:
		return filterBoolHits(hits, typed)
	case *dsl.RangeQuery:
		return filterRangeHits(hits, typed)
	case *dsl.ExistsQuery:
		return filterExistsHits(hits, typed)
	case *dsl.TermsQuery:
		return filterTermsHits(hits, typed)
	case *dsl.PercolateQuery:
		// Percolate hits are produced by the registered-query table in
		// searchMemtableByPercolate; their _source is the stored query
		// DSL, not the percolated document, so _source matching does not
		// apply. Pass them through untouched.
		_ = typed
		return hits
	case *dsl.MatchQuery, *dsl.MatchAllQuery, *dsl.MatchPhraseQuery,
		*dsl.TermQuery, *dsl.MatchNoneQuery, *dsl.KnnQuery,
		*dsl.HybridQuery, *dsl.SparseVectorQuery, *dsl.MultiMatchQuery,
		*dsl.QueryStringQuery, *dsl.FuzzyQuery:
		// These are handled by the segment execution path.
		return hits
	default:
		// For all other query types (prefix, regexp, geo, function_score,
		// script_score, etc.), use docMatchesQuery to filter by _source.
		var out []esapi.SearchHit
		for _, h := range hits {
			src := decodeSource(h.Source)
			if src == nil {
				out = append(out, h)
				continue
			}
			if docMatchesQuery(q, src) {
				out = append(out, h)
			}
		}
		return out
	}
}

func filterBoolHits(hits []esapi.SearchHit, q *dsl.BoolQuery) []esapi.SearchHit {
	var out []esapi.SearchHit
	for _, h := range hits {
		src := decodeSource(h.Source)
		if src == nil {
			out = append(out, h)
			continue
		}
		match := true
		// All must clauses must match
		for _, clause := range q.Must {
			if !matchSourceQuery(src, clause) {
				match = false
				break
			}
		}
		if !match {
			continue
		}
		// All filter clauses must match
		for _, clause := range q.Filter {
			if !matchSourceQuery(src, clause) {
				match = false
				break
			}
		}
		if !match {
			continue
		}
		// No must_not clause may match
		for _, clause := range q.MustNot {
			if matchSourceQuery(src, clause) {
				match = false
				break
			}
		}
		if !match {
			continue
		}
		// Should clauses: if present, at least one must match (MinShouldMatch=1)
		if len(q.Should) > 0 {
			shouldMatch := false
			for _, clause := range q.Should {
				if matchSourceQuery(src, clause) {
					shouldMatch = true
					break
				}
			}
			if !shouldMatch {
				continue
			}
		}
		out = append(out, h)
	}
	return out
}

func filterRangeHits(hits []esapi.SearchHit, q *dsl.RangeQuery) []esapi.SearchHit {
	var out []esapi.SearchHit
	for _, h := range hits {
		src := decodeSource(h.Source)
		if src == nil {
			out = append(out, h)
			continue
		}
		val, ok := src[q.Field]
		if !ok {
			continue
		}
		fval := toFloat64Any(val)
		if fval == nil {
			continue
		}
		v := *fval
		if q.Gte != nil {
			if min := toFloat64Any(q.Gte); min != nil && v < *min {
				continue
			}
		}
		if q.Gt != nil {
			if min := toFloat64Any(q.Gt); min != nil && v <= *min {
				continue
			}
		}
		if q.Lte != nil {
			if max := toFloat64Any(q.Lte); max != nil && v > *max {
				continue
			}
		}
		if q.Lt != nil {
			if max := toFloat64Any(q.Lt); max != nil && v >= *max {
				continue
			}
		}
		out = append(out, h)
	}
	return out
}

func filterExistsHits(hits []esapi.SearchHit, q *dsl.ExistsQuery) []esapi.SearchHit {
	var out []esapi.SearchHit
	for _, h := range hits {
		src := decodeSource(h.Source)
		if src == nil {
			continue
		}
		if _, ok := src[q.Field]; ok {
			out = append(out, h)
		}
	}
	return out
}

func filterTermsHits(hits []esapi.SearchHit, q *dsl.TermsQuery) []esapi.SearchHit {
	termSet := make(map[string]bool, len(q.Values))
	for _, v := range q.Values {
		termSet[fmt.Sprintf("%v", v)] = true
	}
	var out []esapi.SearchHit
	for _, h := range hits {
		src := decodeSource(h.Source)
		if src == nil {
			out = append(out, h)
			continue
		}
		val, ok := src[q.Field]
		if !ok {
			continue
		}
		if termSet[fmt.Sprintf("%v", val)] {
			out = append(out, h)
		}
	}
	return out
}

func matchSourceQuery(src map[string]any, q dsl.Query) bool {
	switch typed := q.(type) {
	case *dsl.TermQuery:
		val, ok := src[typed.Field]
		if !ok {
			return false
		}
		return fmt.Sprintf("%v", val) == fmt.Sprintf("%v", typed.Value)
	case *dsl.MatchQuery:
		val, ok := src[typed.Field]
		if !ok {
			return false
		}
		s := fmt.Sprintf("%v", val)
		return strings.Contains(strings.ToLower(s), strings.ToLower(typed.Text))
	case *dsl.MatchPhraseQuery:
		val, ok := src[typed.Field]
		if !ok {
			return false
		}
		s := fmt.Sprintf("%v", val)
		return strings.Contains(strings.ToLower(s), strings.ToLower(typed.Text))
	case *dsl.RangeQuery:
		val, ok := src[typed.Field]
		if !ok {
			return false
		}
		fval := toFloat64Any(val)
		if fval == nil {
			return false
		}
		v := *fval
		if typed.Gte != nil {
			if min := toFloat64Any(typed.Gte); min != nil && v < *min {
				return false
			}
		}
		if typed.Gt != nil {
			if min := toFloat64Any(typed.Gt); min != nil && v <= *min {
				return false
			}
		}
		if typed.Lte != nil {
			if max := toFloat64Any(typed.Lte); max != nil && v > *max {
				return false
			}
		}
		if typed.Lt != nil {
			if max := toFloat64Any(typed.Lt); max != nil && v >= *max {
				return false
			}
		}
		return true
	case *dsl.ExistsQuery:
		_, ok := src[typed.Field]
		return ok
	case *dsl.TermsQuery:
		val, ok := src[typed.Field]
		if !ok {
			return false
		}
		s := fmt.Sprintf("%v", val)
		for _, v := range typed.Values {
			if s == fmt.Sprintf("%v", v) {
				return true
			}
		}
		return false
	case *dsl.MatchAllQuery:
		return true
	default:
		return true // unknown query type, accept
	}
}

func decodeSource(raw []byte) map[string]any {
	if len(raw) == 0 {
		return nil
	}
	var m map[string]any
	if jsonx.Unmarshal(raw, &m) != nil {
		return nil
	}
	return m
}

func toFloat64Any(v any) *float64 {
	switch n := v.(type) {
	case float64:
		return &n
	case float32:
		f := float64(n)
		return &f
	case int:
		f := float64(n)
		return &f
	case int64:
		f := float64(n)
		return &f
	case int32:
		f := float64(n)
		return &f
	default:
		return nil
	}
}

// Compile-time assertion that Engine satisfies esapi.Engine.
var _ esapi.Engine = (*Engine)(nil)

// ---------------------------------------------------------------------------
// P0 search enhancement helpers (es-feature-gap-fill)
// @spec: ./active_specs/2026-10-22-es-feature-gap-fill/02_spec.md#2.3
// ---------------------------------------------------------------------------

// filterByPostFilter filters hits by checking each hit's _source against
// the post_filter DSL query. Unlike postFilterHits (which handles segment-
// level query re-evaluation), this is the ES post_filter semantic: applied
// after aggregations, reduces hits but does not affect aggs or total.
func filterByPostFilter(hits []esapi.SearchHit, pfQuery dsl.Query) []esapi.SearchHit {
	var out []esapi.SearchHit
	for _, h := range hits {
		src := decodeSource(h.Source)
		if src == nil {
			out = append(out, h)
			continue
		}
		if docMatchesQuery(pfQuery, src) {
			out = append(out, h)
		}
	}
	return out
}

// rescoreSpec is one entry in the rescore array.
type rescoreSpec struct {
	Query              jsonx.RawMessage
	WindowSize         int
	QueryWeight        float32
	RescoreQueryWeight float32
	ScoreMode          string
}

// applyRescore re-scores the top-N hits within each rescore window using
// the secondary query, blending primary and rescore scores.
func applyRescore(hits []esapi.SearchHit, rescoreJSON jsonx.RawMessage) []esapi.SearchHit {
	var specs []rescoreSpec
	// rescore can be a single object or an array.
	first := rescoreJSON[0]
	if first == '[' {
		if err := jsonx.Unmarshal(rescoreJSON, &specs); err != nil {
			return hits // malformed rescore: return unchanged
		}
	} else {
		var s rescoreSpec
		if err := jsonx.Unmarshal(rescoreJSON, &s); err != nil {
			return hits
		}
		specs = []rescoreSpec{s}
	}

	for _, spec := range specs {
		if spec.WindowSize <= 0 {
			spec.WindowSize = 10
		}
		if spec.QueryWeight <= 0 {
			spec.QueryWeight = 1.0
		}
		if spec.RescoreQueryWeight <= 0 {
			spec.RescoreQueryWeight = 1.0
		}

		// Parse the rescore query.
		if len(spec.Query) == 0 {
			continue
		}
		// The rescore query is under "query" key inside the spec.
		// In ES format: {"query": {...rescore_query...}, "window_size": N, ...}
		// But the spec.Query here is already the raw JSON of the "query" field.
		rescoreQ, err := dsl.ParseQuery(spec.Query)
		if err != nil || rescoreQ == nil {
			continue
		}

		// Apply rescore to top window hits.
		window := spec.WindowSize
		if window > len(hits) {
			window = len(hits)
		}
		for i := 0; i < window; i++ {
			src := decodeSource(hits[i].Source)
			if src == nil {
				continue
			}
			// Evaluate the rescore query against the document.
			// P0: use a simple match check — if the doc matches, use
			// a rescore score of 1.0; otherwise 0.
			var rescoreScore float32
			if docMatchesQuery(rescoreQ, src) {
				rescoreScore = 1.0
			}
			// Blend: final = queryWeight * primary + rescoreWeight * rescore
			hits[i].Score = spec.QueryWeight*hits[i].Score + spec.RescoreQueryWeight*rescoreScore
		}

		// Re-sort the rescored window.
		sort.Slice(hits[:window], func(i, j int) bool {
			return hits[i].Score > hits[j].Score
		})
	}
	return hits
}

// collapseSpec defines the collapse configuration.
type collapseSpec struct {
	Field     string           `json:"field"`
	InnerHits jsonx.RawMessage `json:"inner_hits,omitempty"`
}

// applyCollapse groups hits by a field value, keeping only the top hit
// per group (ES semantics: collapse is applied before from/size pagination).
func applyCollapse(hits []esapi.SearchHit, collapseJSON jsonx.RawMessage) []esapi.SearchHit {
	var spec collapseSpec
	if err := jsonx.Unmarshal(collapseJSON, &spec); err != nil || spec.Field == "" {
		return hits // malformed collapse: return unchanged
	}

	// Determine max per group (default 1; inner_hits.size if specified).
	maxPerGroup := 1
	if len(spec.InnerHits) > 0 {
		var ih struct {
			Size int `json:"size"`
		}
		if jsonx.Unmarshal(spec.InnerHits, &ih) == nil && ih.Size > 0 {
			maxPerGroup = ih.Size
		}
	}

	// Group hits by field value, keeping top-N per group.
	seen := make(map[string]int) // group key → count so far
	var out []esapi.SearchHit
	for _, h := range hits {
		src := decodeSource(h.Source)
		if src == nil {
			out = append(out, h)
			continue
		}
		key := fmt.Sprintf("%v", src[spec.Field])
		if seen[key] < maxPerGroup {
			out = append(out, h)
			seen[key]++
		}
	}
	return out
}
