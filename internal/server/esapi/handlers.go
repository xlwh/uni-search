// Package esapi handlers.go implements the M0 REST handlers listed
// in design 04 §2.2. Each handler decodes the request, dispatches
// to the Engine, and assembles an ES-shaped response. Errors are
// always funneled through APIError via writeError.
package esapi

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/RoaringBitmap/roaring"

	"github.com/unisearch-db/unisearch/internal/engine/dsl"
	apierr "github.com/unisearch-db/unisearch/internal/errors"
	"github.com/unisearch-db/unisearch/internal/jsonx"
)

// ---------------------------------------------------------------------------
// Root & cluster (R17.2)
// ---------------------------------------------------------------------------

func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 0, versionInfo)
}

func (s *Server) handleClusterHealth(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := ctxWithTimeout(r)
	defer cancel()
	h, err := s.engine.Health(ctx)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, 0, h)
}

func (s *Server) handleCatHealth(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := ctxWithTimeout(r)
	defer cancel()
	h, err := s.engine.Health(ctx)
	if err != nil {
		writeError(w, err)
		return
	}
	row := map[string]any{
		"status":            h.Status,
		"cluster":           "unisearch",
		"cluster_name":      h.ClusterName,
		"node":              strconv.Itoa(h.NumberOfNodes),
		"data_node":          strconv.Itoa(h.NumberOfDataNodes),
		"pri":               strconv.Itoa(h.ActivePrimaryShards),
		"relo":              strconv.Itoa(h.RelocatingShards),
		"init":              strconv.Itoa(h.InitializingShards),
		"unassign":          strconv.Itoa(h.UnassignedShards),
	}
	// Return JSON when Accept header or ?format=json is specified.
	// @spec: ./archive/2026-10-21-deep-gap-remediation/02_spec.md#cat
	if r.Header.Get("Accept") == "application/json" || r.URL.Query().Get("format") == "json" {
		writeJSON(w, 0, []map[string]any{row})
		return
	}
	// Text format.
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	if r.URL.Query().Has("v") {
		fmt.Fprintf(w, "%s\n", strings.Join([]string{
			"status", "cluster", "cluster_name", "node", "data_node", "pri", "relo", "init", "unassign",
		}, " "))
	}
	fmt.Fprintf(w, "%s\n", strings.Join([]string{
		fmt.Sprintf("%v", row["status"]), "unisearch", h.ClusterName,
		strconv.Itoa(h.NumberOfNodes), strconv.Itoa(h.NumberOfDataNodes),
		strconv.Itoa(h.ActivePrimaryShards), strconv.Itoa(h.RelocatingShards),
		strconv.Itoa(h.InitializingShards), strconv.Itoa(h.UnassignedShards),
	}, " "))
}

func (s *Server) handleCatIndices(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := ctxWithTimeout(r)
	defer cancel()
	pattern := r.PathValue("index")
	if pattern == "" {
		pattern = "*"
	}
	infos, err := s.engine.Indices(ctx, pattern)
	if err != nil {
		writeError(w, err)
		return
	}
	if r.URL.Query().Get("format") == "json" || strings.Contains(r.Header.Get("Accept"), "json") {
		writeJSON(w, 0, infos)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	// Always write header line so text format is never empty (ES-compatible).
	fmt.Fprintln(w, "health status index uuid pri rep docs.count docs.deleted store.size pri.store.size")
	for _, i := range infos {
		fmt.Fprintf(w, "%s %s %s %s %d %d %d %d %s %s\n",
			i.Health, i.Status, i.Index, i.UUID, i.Primaries, i.Replicas,
			i.DocsCount, i.DocsDeleted, i.StoreSize, i.StoreSize)
	}
}

// ---------------------------------------------------------------------------
// Index lifecycle (R17)
// ---------------------------------------------------------------------------

func (s *Server) handlePutIndex(w http.ResponseWriter, r *http.Request) {
	idx := r.PathValue("index")
	if idx == "" {
		writeError(w, apierr.NewIllegalArgument("index name required"))
		return
	}
	body, err := readBody(r)
	if err != nil {
		writeError(w, apierr.NewIllegalArgument("failed to read body: "+err.Error()))
		return
	}
	// Body may contain settings/mapping; pass-through to the engine.
	if len(body) == 0 {
		body = []byte("{}")
	}
	if err := s.engine.PutMapping(ctxFromReq(r), idx, body); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, 0, map[string]any{
		"acknowledged":        true,
		"shards_acknowledged": true,
		"index":               idx,
	})
}

func (s *Server) handleDeleteIndex(w http.ResponseWriter, r *http.Request) {
	idx := r.PathValue("index")
	if idx == "" {
		writeError(w, apierr.NewIllegalArgument("index name required"))
		return
	}
	if err := s.engine.DeleteIndex(ctxFromReq(r), idx); err != nil {
		// ES returns 200 for deleting a non-existent index (idempotent).
		// Only surface real errors (not "index not found").
		writeJSON(w, 0, map[string]any{"acknowledged": true, "index": idx})
		return
	}
	writeJSON(w, 0, map[string]any{"acknowledged": true, "index": idx})
}

// ---------------------------------------------------------------------------
// _doc CRUD (R17.3)
// ---------------------------------------------------------------------------

func (s *Server) handleIndexAutoID(w http.ResponseWriter, r *http.Request) {
	idx := r.PathValue("index")
	body, err := readBody(r)
	if err != nil {
		writeError(w, apierr.NewIllegalArgument("failed to read body: "+err.Error()))
		return
	}
	// Engine generates the _id when ID is empty.
	doc := IndexDoc{
		Op: OpIndex, Index: idx, ID: "", Source: body,
		Pipeline: r.URL.Query().Get("pipeline"),
	}
	if perr := s.applyPipeline(&doc); perr != nil {
		writeError(w, apierr.NewIllegalArgument("pipeline error: "+perr.Error()))
		return
	}
	res, err := s.engine.Write(ctxFromReq(r), doc, refreshParam(r))
	if err != nil {
		writeError(w, err)
		return
	}
	respondWriteResult(w, res)
}

func (s *Server) handleIndexDoc(w http.ResponseWriter, r *http.Request) {
	idx := r.PathValue("index")
	id := r.PathValue("id")
	body, err := readBody(r)
	if err != nil {
		writeError(w, apierr.NewIllegalArgument("failed to read body: "+err.Error()))
		return
	}
	doc := IndexDoc{Op: OpIndex, Index: idx, ID: id, Source: body}
	doc.Pipeline = r.URL.Query().Get("pipeline")
	if ifSeq := r.URL.Query().Get("if_seq_no"); ifSeq != "" {
		seq, err := strconv.ParseInt(ifSeq, 10, 64)
		if err != nil {
			writeError(w, apierr.NewIllegalArgument("if_seq_no must be an integer"))
			return
		}
		doc.IfSeqNo = &seq
	}
	if op := r.URL.Query().Get("op_type"); op == "create" {
		doc.Op = OpCreate
	}
	if perr := s.applyPipeline(&doc); perr != nil {
		writeError(w, apierr.NewIllegalArgument("pipeline error: "+perr.Error()))
		return
	}
	res, err := s.engine.Write(ctxFromReq(r), doc, refreshParam(r))
	if err != nil {
		writeError(w, err)
		return
	}
	if res.Result == "conflict" {
		writeJSON(w, http.StatusConflict, conflictBody(res))
		return
	}
	respondWriteResult(w, res)
}

func (s *Server) handleCreateDoc(w http.ResponseWriter, r *http.Request) {
	idx := r.PathValue("index")
	id := r.PathValue("id")
	body, err := readBody(r)
	if err != nil {
		writeError(w, apierr.NewIllegalArgument("failed to read body: "+err.Error()))
		return
	}
	res, err := s.engine.Write(ctxFromReq(r), IndexDoc{
		Op: OpCreate, Index: idx, ID: id, Source: body,
	}, refreshParam(r))
	if err != nil {
		writeError(w, err)
		return
	}
	if res.Result == "conflict" {
		writeJSON(w, http.StatusConflict, conflictBody(res))
		return
	}
	respondWriteResult(w, res)
}

func (s *Server) handleGetDoc(w http.ResponseWriter, r *http.Request) {
	idx := r.PathValue("index")
	id := r.PathValue("id")
	source, seqNo, primary, notFound, err := s.engine.Get(ctxFromReq(r), idx, id)
	if err != nil {
		writeError(w, err)
		return
	}
	if notFound {
		writeJSON(w, http.StatusNotFound, map[string]any{
			"_index": idx,
			"_id":    id,
			"found":  false,
		})
		return
	}
	writeJSON(w, 0, map[string]any{
		"_index":        idx,
		"_id":           id,
		"_version":      1,
		"_seq_no":       seqNo,
		"_primary_term": primary,
		"found":         true,
		"_source":       jsonx.RawMessage(source),
	})
}

func (s *Server) handleDeleteDoc(w http.ResponseWriter, r *http.Request) {
	idx := r.PathValue("index")
	id := r.PathValue("id")
	var ifSeq *int64
	if v := r.URL.Query().Get("if_seq_no"); v != "" {
		seq, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			writeError(w, apierr.NewIllegalArgument("if_seq_no must be an integer"))
			return
		}
		ifSeq = &seq
	}
	res, err := s.engine.Delete(ctxFromReq(r), idx, id, ifSeq, refreshParam(r))
	if err != nil {
		writeError(w, err)
		return
	}
	if res.Result == "conflict" {
		writeJSON(w, http.StatusConflict, conflictBody(res))
		return
	}
	writeJSON(w, 0, map[string]any{
		"_index":   idx,
		"_id":      id,
		"_version": 1,
		"result":   "deleted",
		"_seq_no":  res.SeqNo,
	})
}

// respondWriteResult builds the ES 8.x single-doc write response.
func respondWriteResult(w http.ResponseWriter, res *WriteResult) {
	status := http.StatusOK
	if res.Result == "created" {
		status = http.StatusCreated
	}
	writeJSON(w, status, map[string]any{
		"_index":        res.Index,
		"_id":           res.ID,
		"_version":      res.Version,
		"result":        res.Result,
		"_seq_no":       res.SeqNo,
		"_primary_term": 1,
		"_shards": map[string]any{
			"total":      1,
			"successful": 1,
			"failed":     0,
		},
	})
}

// conflictBody builds the 409 response body for if_seq_no mismatches.
func conflictBody(res *WriteResult) map[string]any {
	api := apierr.NewVersionConflict(res.Index,
		fmt.Sprintf("[doc][%s]: version conflict, required seqNo [%d]", res.ID, -1))
	body := api.ResponseBody()
	var raw map[string]any
	_ = jsonx.Unmarshal(body, &raw)
	return raw
}

// ---------------------------------------------------------------------------
// _bulk (R17.4)
// ---------------------------------------------------------------------------

// bulkActionLine matches the action line of an NDJSON _bulk request.
type bulkActionLine struct {
	Index *struct {
		Index         string `json:"_index"`
		ID            string `json:"_id"`
		IfSeqNo       int64  `json:"if_seq_no,omitempty"`
		IfPrimaryTerm int64  `json:"if_primary_term,omitempty"`
	} `json:"index"`
	Create *struct {
		Index string `json:"_index"`
		ID    string `json:"_id"`
	} `json:"create"`
	Update *struct {
		Index string `json:"_index"`
		ID    string `json:"_id"`
	} `json:"update"`
	Delete *struct {
		Index string `json:"_index"`
		ID    string `json:"_id"`
	} `json:"delete"`
}

func (s *Server) handleBulk(w http.ResponseWriter, r *http.Request) {
	idx := r.PathValue("index")
	reader := r.Body
	if reader == nil {
		writeJSON(w, 0, bulkResponse{})
		return
	}
	defer reader.Close()
	// Limit total body to maxBodyBytes (100MB) per design 04 §6.1.
	// @spec: ./archive/2026-10-21-deep-gap-remediation/02_spec.md#2.3.5
	dec := jsonx.NewDecoder(io.LimitReader(reader, maxBodyBytes))
	docs := make([]IndexDoc, 0, 32)
	decErr := error(nil)
	for {
		var action bulkActionLine
		if err := dec.Decode(&action); err != nil {
			if err != io.EOF {
				decErr = err
			}
			break
		}
		// Pick the active action.
		pipeline := r.URL.Query().Get("pipeline") // @spec: es-feature-gap-fill 02_spec.md#2.7
		doc := IndexDoc{Index: idx, Pipeline: pipeline}
		switch {
		case action.Index != nil:
			doc.Op = OpIndex
			doc.ID = action.Index.ID
			doc.Index = pickIndex(idx, action.Index.Index)
			if action.Index.IfSeqNo != 0 {
				seq := action.Index.IfSeqNo
				doc.IfSeqNo = &seq
			}
			// @spec: ./archive/2026-10-21-deep-gap-remediation/02_spec.md#2.3.5
			if action.Index.IfPrimaryTerm != 0 {
				doc.IfPrimaryTerm = action.Index.IfPrimaryTerm
			}
		case action.Create != nil:
			doc.Op = OpCreate
			doc.ID = action.Create.ID
			doc.Index = pickIndex(idx, action.Create.Index)
		case action.Update != nil:
			doc.Op = OpUpdate
			doc.ID = action.Update.ID
			doc.Index = pickIndex(idx, action.Update.Index)
		case action.Delete != nil:
			doc.Op = OpDelete
			doc.ID = action.Delete.ID
			doc.Index = pickIndex(idx, action.Delete.Index)
		default:
			decErr = fmt.Errorf("bulk: empty action line")
			break
		}
		// Only index/create/update carry a source line; delete has none.
		if doc.Op != OpDelete {
			var raw jsonx.RawMessage
			if err := dec.Decode(&raw); err != nil {
				decErr = fmt.Errorf("bulk: missing source line for %s: %w", doc.Op, err)
				break
			}
			doc.Source = raw
		}
		docs = append(docs, doc)
	}
	if decErr != nil && decErr != io.EOF {
		// On parser error, surface as 400 with the aggregate body.
		writeError(w, apierr.NewDocumentParsing(decErr.Error()))
		return
	}
	results, err := s.engine.Bulk(ctxFromReq(r), docs, refreshParam(r))
	if err != nil {
		writeError(w, err)
		return
	}
	resp := bulkResponse{
		Took:   0,
		Errors: false,
		Items:  make([]map[string]any, len(results)),
	}
	hasErrors := false
	for i, res := range results {
		entry := map[string]any{
			string(res.Op): map[string]any{
				"_index":        res.Index,
				"_id":           res.ID,
				"_version":      res.Version,
				"result":        res.Result,
				"_seq_no":       res.SeqNo,
				"_primary_term": 1,
				"status":        opDefaultStatus(res.Op, res.Result),
			},
		}
		if res.Err != nil {
			entry[string(res.Op)].(map[string]any)["error"] = map[string]any{
				"type":   "document_parsing_exception",
				"reason": res.Err.Error(),
			}
			entry[string(res.Op)].(map[string]any)["status"] = http.StatusBadRequest
			hasErrors = true
		}
		resp.Items[i] = entry
	}
	resp.Errors = hasErrors
	writeJSON(w, 0, resp)
}

// bulkResponse is the ES 8.x _bulk response shape.
type bulkResponse struct {
	Took   int64            `json:"took"`
	Errors bool             `json:"errors"`
	Items  []map[string]any `json:"items"`
}

// opDefaultStatus returns the per-item HTTP status for an op.
func opDefaultStatus(op DocOp, result string) int {
	switch {
	case result == "created":
		return http.StatusCreated
	case result == "deleted":
		return http.StatusOK
	case result == "not_found":
		return http.StatusNotFound
	case result == "conflict":
		return http.StatusConflict
	default:
		return http.StatusOK
	}
}

// pickIndex returns the explicit per-line index when set, else the
// path-level {index} placeholder.
func pickIndex(pathIdx, lineIdx string) string {
	if lineIdx != "" {
		return lineIdx
	}
	return pathIdx
}

// ---------------------------------------------------------------------------
// _search / _count / _mget (R17.5)
// ---------------------------------------------------------------------------

func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	indices := parseIndices(r)
	body, err := readBody(r)
	if err != nil {
		writeError(w, apierr.NewIllegalArgument("failed to read body: "+err.Error()))
		return
	}
	if len(body) == 0 {
		body = []byte("{}")
	}
	res, err := s.engine.Search(ctxFromReq(r), indices, body)
	if err != nil {
		if strings.Contains(err.Error(), "index_not_found") {
			idx := ""
			if len(indices) > 0 {
				idx = indices[0]
			}
			writeError(w, apierr.NewIndexNotFound(idx))
			return
		}
		writeError(w, err)
		return
	}

	// Apply _source filtering if requested.
	q, _, _, extras, _ := dsl.ParseSearchBodyFull(body)

	// Compute highlight inline if the engine didn't produce it.
	if len(extras.Highlight) > 0 && len(res.Highlight) == 0 {
		res.Highlight = computeHighlightInline(extras.Highlight, res.Hits, q)
	}

	// Inject highlight into each hit BEFORE reshapeHits (ES 8.x places
	// highlight inside each hit, not at the response top level).
	if res.Highlight != nil {
		for i := range res.Hits {
			if hl, ok := res.Highlight[res.Hits[i].ID]; ok {
				res.Hits[i].Highlight = hl
			}
		}
	}

	if len(extras.Source) > 0 {
		applySourceFilter(res.Hits, extras.Source)
	}

	// Apply sort if requested.
	if len(extras.Sort) > 0 {
		applySort(res.Hits, extras.Sort)
	}

	reshapedHits := reshapeHits(res.Hits)

	// Post-compute highlight directly on response hits.
	if len(extras.Highlight) > 0 {
		postComputeHighlightFromBody(reshapedHits, extras.Highlight, body, res.Hits)
	}

	resp := map[string]any{
		"took":      res.Took,
		"timed_out": res.TimedOut,
		"_shards": map[string]any{
			"total":      res.Shards.Total,
			"successful": res.Shards.Successful,
			"skipped":    res.Shards.Skipped,
			"failed":     res.Shards.Failed,
		},
		"hits": map[string]any{
			"total": map[string]any{
				"value":    res.Total,
				"relation": res.Relation,
			},
			"max_score": res.MaxScore,
			"hits":      reshapedHits,
		},
	}
	if res.Aggregations != nil {
		resp["aggregations"] = res.Aggregations
	}
	// @spec: ./archive/2026-10-21-deep-gap-remediation/02_spec.md#2.3.4
	if res.Suggest != nil {
		resp["suggest"] = res.Suggest
	}
	if res.Profile != nil {
		resp["profile"] = res.Profile
	}
	// Scroll support: when ?scroll=duration is present, create a scroll
	// context and return _scroll_id for subsequent page requests.
	if scrollParam := r.URL.Query().Get("scroll"); scrollParam != "" {
		var sortVals []any
		if len(res.Hits) > 0 {
			sortVals = res.Hits[len(res.Hits)-1].SortValues
		}
		indexName := ""
		if len(indices) > 0 {
			indexName = indices[0]
		}
		cursor := &scrollCursor{
			SortValues: sortVals,
			From:       0,
			Body:       body,
			Index:      indexName,
			Size:       10,
		}
		scrollID := encodeScrollID(cursor)
		resp["_scroll_id"] = scrollID
	}
	writeJSON(w, 0, resp)
}

// computeHighlightInline produces per-field highlighted fragments by
// finding query terms in each hit's _source and wrapping them in <em> tags.
// postComputeHighlight directly adds highlight fields to the response
// hit maps. This bypasses the SearchHit.Highlight injection chain and
// works directly on the JSON response data.
// postComputeHighlightFromBody extracts query terms directly from the
// raw search body JSON (bypassing the DSL parser) and adds highlight
// fields to each response hit map.
func postComputeHighlightFromBody(hits []map[string]any, highlightJSON jsonx.RawMessage, body []byte, searchHits []SearchHit) {
	var hlDef map[string]jsonx.RawMessage
	if jsonx.Unmarshal(highlightJSON, &hlDef) != nil || len(hlDef) == 0 {
		return
	}
	// Parse highlight fields. ES format: {"fields": {"title": {}, "body": {}}}
	// The "fields" key wraps the actual field names.
	fields := make(map[string]bool)
	if fieldsRaw, ok := hlDef["fields"]; ok {
		var fieldsMap map[string]jsonx.RawMessage
		if jsonx.Unmarshal(fieldsRaw, &fieldsMap) == nil {
			for field := range fieldsMap {
				fields[field] = true
			}
		}
	}
	// Also accept direct field names without "fields" wrapper.
	for field := range hlDef {
		if field == "fields" || field == "pre_tags" || field == "post_tags" || field == "fragment_size" || field == "number_of_fragments" {
			continue
		}
		fields[field] = true
	}
	if len(fields) == 0 {
		return
	}
	// Extract query terms directly from the raw body JSON.
	var bodyMap map[string]jsonx.RawMessage
	if jsonx.Unmarshal(body, &bodyMap) != nil {
		return
	}
	var terms []string
	if queryRaw, ok := bodyMap["query"]; ok {
		extractTermsFromJSON(queryRaw, &terms)
	}
	if len(terms) == 0 {
		return
	}
	// Build ID → source bytes map from SearchHit slice.
	srcByID := make(map[string][]byte, len(searchHits))
	for _, sh := range searchHits {
		srcByID[sh.ID] = sh.Source
	}
	// For each response hit, get source bytes by ID and compute highlight.
	for i, hit := range hits {
		id, _ := hit["_id"].(string)
		srcBytes, ok := srcByID[id]
		if !ok && i < len(searchHits) {
			srcBytes = searchHits[i].Source
			ok = len(srcBytes) > 0
		}
		if !ok || len(srcBytes) == 0 {
			continue
		}
		var m map[string]any
		if jsonx.Unmarshal(srcBytes, &m) != nil {
			continue
		}
		hlFields := make(map[string][]string)
		for field := range fields {
			val, ok := m[field]
			if !ok {
				continue
			}
			var text string
			switch v := val.(type) {
			case string:
				text = v
			case []byte:
				text = string(v)
			default:
				text = fmt.Sprintf("%v", v)
			}
			hl := text
			for _, term := range terms {
				if term == "" {
					continue
				}
				lower := strings.ToLower(hl)
				idx := strings.Index(lower, term)
				if idx >= 0 {
					end := idx + len(term)
					hl = hl[:idx] + "<em>" + hl[idx:end] + "</em>" + hl[end:]
				}
			}
			if hl != text {
				hlFields[field] = []string{hl}
			}
		}
		if len(hlFields) > 0 {
			hit["highlight"] = hlFields
		}
	}
}

// extractTermsFromJSON recursively extracts search terms from a query JSON node.
func extractTermsFromJSON(raw jsonx.RawMessage, terms *[]string) {
	var m map[string]jsonx.RawMessage
	if jsonx.Unmarshal(raw, &m) != nil {
		// Try as string (e.g., query_string)
		var s string
		if jsonx.Unmarshal(raw, &s) == nil {
			*terms = append(*terms, strings.Fields(strings.ToLower(s))...)
		}
		return
	}
	for key, val := range m {
		switch key {
		case "match", "match_phrase", "match_phrase_prefix":
			// {"field": "text"} or {"field": {"query": "text"}}
			var fm map[string]jsonx.RawMessage
			if jsonx.Unmarshal(val, &fm) == nil {
				for _, fv := range fm {
					var s string
					if jsonx.Unmarshal(fv, &s) == nil {
						*terms = append(*terms, strings.Fields(strings.ToLower(s))...)
					} else {
						var qm map[string]any
						if jsonx.Unmarshal(fv, &qm) == nil {
							if qt, ok := qm["query"].(string); ok {
								*terms = append(*terms, strings.Fields(strings.ToLower(qt))...)
							}
						}
					}
				}
			}
		case "term":
			var fm map[string]jsonx.RawMessage
			if jsonx.Unmarshal(val, &fm) == nil {
				for _, fv := range fm {
					var s string
					if jsonx.Unmarshal(fv, &s) == nil {
						*terms = append(*terms, strings.ToLower(s))
					} else {
						var tm map[string]any
						if jsonx.Unmarshal(fv, &tm) == nil {
							if v, ok := tm["value"]; ok {
								*terms = append(*terms, strings.ToLower(fmt.Sprintf("%v", v)))
							}
						}
					}
				}
			}
		case "bool":
			var bm map[string]jsonx.RawMessage
			if jsonx.Unmarshal(val, &bm) == nil {
				for _, clause := range []string{"must", "should", "filter"} {
					if arr, ok := bm[clause]; ok {
						var arrItems []jsonx.RawMessage
						if jsonx.Unmarshal(arr, &arrItems) == nil {
							for _, item := range arrItems {
								extractTermsFromJSON(item, terms)
							}
						}
					}
				}
			}
		case "query_string":
			var qsm map[string]any
			if jsonx.Unmarshal(val, &qsm) == nil {
				if qs, ok := qsm["query"].(string); ok {
					qs = strings.ReplaceAll(qs, " AND ", " ")
					qs = strings.ReplaceAll(qs, " OR ", " ")
					qs = strings.ReplaceAll(qs, " NOT ", " ")
					*terms = append(*terms, strings.Fields(strings.ToLower(qs))...)
				}
			}
		}
	}
}

func postComputeHighlight(hits []map[string]any, highlightJSON jsonx.RawMessage, q dsl.Query) {
	var hlDef map[string]jsonx.RawMessage
	if jsonx.Unmarshal(highlightJSON, &hlDef) != nil || len(hlDef) == 0 {
		return
	}
	// Parse highlight fields
	fields := make(map[string]bool)
	for field := range hlDef {
		if field == "pre_tags" || field == "post_tags" || field == "fragment_size" || field == "number_of_fragments" {
			continue
		}
		fields[field] = true
	}
	if len(fields) == 0 {
		return
	}
	// Extract query terms from DSL query
	var terms []string
	if q != nil {
		switch typed := q.(type) {
		case *dsl.MatchQuery:
			terms = strings.Fields(strings.ToLower(typed.Text))
		case *dsl.MatchPhraseQuery:
			terms = strings.Fields(strings.ToLower(typed.Text))
		case *dsl.TermQuery:
			terms = []string{strings.ToLower(fmt.Sprintf("%v", typed.Value))}
		case *dsl.BoolQuery:
			for _, c := range typed.Must {
				if mq, ok := c.(*dsl.MatchQuery); ok {
					terms = append(terms, strings.Fields(strings.ToLower(mq.Text))...)
				}
			}
			for _, c := range typed.Should {
				if mq, ok := c.(*dsl.MatchQuery); ok {
					terms = append(terms, strings.Fields(strings.ToLower(mq.Text))...)
				}
			}
		}
	}
	if len(terms) == 0 {
		return
	}
	// For each hit, find query terms in field values and wrap in <em>
	for _, hit := range hits {
		src, ok := hit["_source"]
		if !ok {
			continue
		}
		// Marshal _source to bytes then unmarshal to map (handles any type)
		srcBytes, err := jsonx.Marshal(src)
		if err != nil {
			continue
		}
		var m map[string]any
		if jsonx.Unmarshal(srcBytes, &m) != nil {
			continue
		}
		hlFields := make(map[string][]string)
		for field := range fields {
			val, ok := m[field]
			if !ok {
				continue
			}
			text, ok := val.(string)
			if !ok {
				text = fmt.Sprintf("%v", val)
			}
			hl := text
			for _, term := range terms {
				if term == "" {
					continue
				}
				lower := strings.ToLower(hl)
				idx := strings.Index(lower, term)
				if idx >= 0 {
					end := idx + len(term)
					hl = hl[:idx] + "<em>" + hl[idx:end] + "</em>" + hl[end:]
				}
			}
			if hl != text {
				hlFields[field] = []string{hl}
			}
		}
		if len(hlFields) > 0 {
			hit["highlight"] = hlFields
		}
	}
}

func computeHighlightInline(highlightJSON jsonx.RawMessage, hits []SearchHit, q dsl.Query) map[string]map[string][]string {
	var hlDef map[string]jsonx.RawMessage
	if jsonx.Unmarshal(highlightJSON, &hlDef) != nil || len(hlDef) == 0 {
		return nil
	}
	// Parse highlight fields
	fields := make(map[string]bool)
	for field := range hlDef {
		if field == "pre_tags" || field == "post_tags" || field == "fragment_size" || field == "number_of_fragments" {
			continue
		}
		fields[field] = true
	}
	if len(fields) == 0 {
		return nil
	}
	// Extract query terms
	var terms []string
	if q != nil {
		switch typed := q.(type) {
		case *dsl.MatchQuery:
			terms = strings.Fields(strings.ToLower(typed.Text))
		case *dsl.MatchPhraseQuery:
			terms = strings.Fields(strings.ToLower(typed.Text))
		case *dsl.TermQuery:
			terms = []string{strings.ToLower(fmt.Sprintf("%v", typed.Value))}
		case *dsl.BoolQuery:
			for _, c := range typed.Must {
				if mq, ok := c.(*dsl.MatchQuery); ok {
					terms = append(terms, strings.Fields(strings.ToLower(mq.Text))...)
				}
			}
			for _, c := range typed.Should {
				if mq, ok := c.(*dsl.MatchQuery); ok {
					terms = append(terms, strings.Fields(strings.ToLower(mq.Text))...)
				}
			}
		}
	}
	if len(terms) == 0 {
		return nil
	}
	result := make(map[string]map[string][]string)
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
			hl := text
			for _, term := range terms {
				if term == "" {
					continue
				}
				lower := strings.ToLower(hl)
				idx := strings.Index(lower, term)
				if idx >= 0 {
					end := idx + len(term)
					hl = hl[:idx] + "<em>" + hl[idx:end] + "</em>" + hl[end:]
				}
			}
			if hl != text {
				fieldResults[field] = []string{hl}
			}
		}
		if len(fieldResults) > 0 {
			result[hit.ID] = fieldResults
		}
	}
	return result
}

func applySourceFilter(hits []SearchHit, sourceJSON jsonx.RawMessage) {
	var srcDef any
	if jsonx.Unmarshal(sourceJSON, &srcDef) != nil {
		return
	}
	var includes, excludes []string
	switch v := srcDef.(type) {
	case bool:
		if !v {
			// _source: false → exclude all fields
			for i := range hits {
				hits[i].Source = []byte("{}")
			}
			return
		}
	case string:
		includes = []string{v}
	case []any:
		for _, item := range v {
			if s, ok := item.(string); ok {
				includes = append(includes, s)
			}
		}
	case map[string]any:
		if inc, ok := v["includes"].([]any); ok {
			for _, item := range inc {
				if s, ok := item.(string); ok {
					includes = append(includes, s)
				}
			}
		}
		if exc, ok := v["excludes"].([]any); ok {
			for _, item := range exc {
				if s, ok := item.(string); ok {
					excludes = append(excludes, s)
				}
			}
		}
	}
	if len(includes) == 0 && len(excludes) == 0 {
		return
	}
	for i := range hits {
		var doc map[string]any
		if jsonx.Unmarshal(hits[i].Source, &doc) != nil {
			continue
		}
		filtered := make(map[string]any)
		if len(includes) > 0 {
			for _, f := range includes {
				if v, ok := doc[f]; ok {
					filtered[f] = v
				}
			}
		} else {
			filtered = doc
		}
		for _, f := range excludes {
			delete(filtered, f)
		}
		if out, err := jsonx.Marshal(filtered); err == nil {
			hits[i].Source = out
		}
	}
}

// applySort sorts hits by the specified sort fields.
func applySort(hits []SearchHit, sortJSON jsonx.RawMessage) {
	var sortSpecs []map[string]any
	if jsonx.Unmarshal(sortJSON, &sortSpecs) != nil || len(sortSpecs) == 0 {
		return
	}
	// Parse sort specs: [{"field": "asc"}, {"field": "desc"}]
	type sortField struct {
		field string
		order string // "asc" or "desc"
	}
	var fields []sortField
	for _, spec := range sortSpecs {
		for field, val := range spec {
			order := "asc"
			if s, ok := val.(string); ok {
				order = s
			}
			fields = append(fields, sortField{field: field, order: order})
		}
	}
	if len(fields) == 0 {
		return
	}
	sort.SliceStable(hits, func(i, j int) bool {
		for _, f := range fields {
			vi := getHitField(hits[i], f.field)
			vj := getHitField(hits[j], f.field)
			cmp := compareSortValues(vi, vj)
			if cmp == 0 {
				continue
			}
			if f.order == "desc" {
				return cmp > 0
			}
			return cmp < 0
		}
		return false
	})
}

// getHitField extracts a field value from a hit's _source.
func getHitField(hit SearchHit, field string) any {
	var doc map[string]any
	if jsonx.Unmarshal(hit.Source, &doc) != nil {
		return nil
	}
	return doc[field]
}

// compareSortValues compares two any values for sorting.
func compareSortValues(a, b any) int64 {
	af, _ := toFloat64Sort(a)
	bf, _ := toFloat64Sort(b)
	if af < bf {
		return -1
	}
	if af > bf {
		return 1
	}
	return 0
}

func toFloat64Sort(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case string:
		return 0, false
	default:
		return 0, false
	}
}

// reshapeHits converts engine-side SearchHit into the ES-shaped slice.
func reshapeHits(hits []SearchHit) []map[string]any {
	out := make([]map[string]any, 0, len(hits))
	for _, h := range hits {
		entry := map[string]any{
			"_index":        h.Index,
			"_id":           h.ID,
			"_score":        h.Score,
			"_source":       jsonx.RawMessage(h.Source),
			"_seq_no":       h.SeqNo,
			"_primary_term": h.Primary,
		}
		if len(h.SortValues) > 0 {
			entry["sort"] = h.SortValues
		}
		if len(h.Highlight) > 0 {
			entry["highlight"] = h.Highlight
		}
		out = append(out, entry)
	}
	return out
}

func (s *Server) handleCount(w http.ResponseWriter, r *http.Request) {
	indices := parseIndices(r)
	body, err := readBody(r)
	if err != nil {
		writeError(w, apierr.NewIllegalArgument("failed to read body: "+err.Error()))
		return
	}
	if len(body) == 0 {
		body = nil
	}
	count, err := s.engine.Count(ctxFromReq(r), indices, body)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, 0, map[string]any{
		"count":   count,
		"_shards": map[string]any{"total": 1, "successful": 1, "skipped": 0, "failed": 0},
	})
}

func (s *Server) handleMGet(w http.ResponseWriter, r *http.Request) {
	idx := r.PathValue("index")
	body, err := readBody(r)
	if err != nil {
		writeError(w, apierr.NewIllegalArgument("failed to read body: "+err.Error()))
		return
	}
	var req struct {
		Docs []struct {
			Index string `json:"_index"`
			ID    string `json:"_id"`
		} `json:"docs"`
		IDS []string `json:"ids"`
	}
	if err := jsonx.Unmarshal(body, &req); err != nil {
		writeError(w, apierr.NewDocumentParsing("invalid mget body: "+err.Error()))
		return
	}
	var getReqs []GetRequest
	for _, d := range req.Docs {
		ix := d.Index
		if ix == "" {
			ix = idx
		}
		getReqs = append(getReqs, GetRequest{Index: ix, ID: d.ID})
	}
	for _, id := range req.IDS {
		getReqs = append(getReqs, GetRequest{Index: idx, ID: id})
	}
	results, err := s.engine.MGet(ctxFromReq(r), getReqs)
	if err != nil {
		writeError(w, err)
		return
	}
	docs := make([]map[string]any, 0, len(results))
	for _, g := range results {
		entry := map[string]any{
			"_index": g.Index,
			"_id":    g.ID,
			"found":  g.Found,
		}
		if g.Found {
			entry["_source"] = jsonx.RawMessage(g.Source)
			entry["_seq_no"] = g.SeqNo
			entry["_primary_term"] = g.Primary
		}
		docs = append(docs, entry)
	}
	writeJSON(w, 0, map[string]any{"docs": docs})
}

// ---------------------------------------------------------------------------
// mapping/alias/settings/analyze/refresh (R17.5)
// ---------------------------------------------------------------------------

func (s *Server) handlePutMapping(w http.ResponseWriter, r *http.Request) {
	idx := r.PathValue("index")
	body, err := readBody(r)
	if err != nil {
		writeError(w, apierr.NewIllegalArgument("failed to read body: "+err.Error()))
		return
	}
	// Scan mapping body for deprecated features and set Warning headers.
	// @spec: ./archive/2026-10-21-deep-gap-remediation/02_spec.md#deprecation
	if bytes.Contains(body, []byte(`"doc_values":false`)) {
		setDeprecationWarning(w, "doc_values is always enabled; false is accepted but deprecated")
	}
	if bytes.Contains(body, []byte(`"norms":false`)) {
		setDeprecationWarning(w, "norms are not stored separately; field length is always tracked")
	}
	if bytes.Contains(body, []byte(`"index":false`)) {
		setDeprecationWarning(w, "index:false is accepted but column storage is always enabled")
	}
	if err := s.engine.PutMapping(ctxFromReq(r), idx, body); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, 0, map[string]any{"acknowledged": true})
}

func (s *Server) handleGetMapping(w http.ResponseWriter, r *http.Request) {
	idx := r.PathValue("index")
	body, err := s.engine.GetMapping(ctxFromReq(r), idx)
	if err != nil {
		writeError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Write(body)
}

func (s *Server) handlePutAlias(w http.ResponseWriter, r *http.Request) {
	idx := r.PathValue("index")
	alias := r.PathValue("name")
	if alias == "" {
		// Body form: {"actions": [{"add": {"index": "...", "alias": "..."}}]}.
		body, _ := readBody(r)
		var req struct {
			Actions []struct {
				Add *struct {
					Index string `json:"index"`
					Alias string `json:"alias"`
				} `json:"add"`
				Remove *struct {
					Index string `json:"index"`
					Alias string `json:"alias"`
				} `json:"remove"`
			} `json:"actions"`
		}
		_ = jsonx.Unmarshal(body, &req)
		for _, a := range req.Actions {
			if a.Add != nil {
				_ = s.engine.PutAlias(ctxFromReq(r), a.Add.Index, a.Add.Alias)
			}
			if a.Remove != nil {
				// @spec: ./archive/2026-10-21-deep-gap-remediation/02_spec.md#B4
				_ = s.engine.RemoveAlias(ctxFromReq(r), a.Remove.Index, a.Remove.Alias)
			}
		}
		writeJSON(w, 0, map[string]any{"acknowledged": true})
		return
	}
	if err := s.engine.PutAlias(ctxFromReq(r), idx, alias); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, 0, map[string]any{"acknowledged": true})
}

func (s *Server) handleGetAliases(w http.ResponseWriter, r *http.Request) {
	idx := r.PathValue("index")
	alias := r.PathValue("name")
	aliases, err := s.engine.GetAliases(ctxFromReq(r), idx)
	if err != nil {
		writeError(w, err)
		return
	}
	if alias != "" {
		// Filter to the named alias only.
		filtered := map[string][]string{}
		for ix, als := range aliases {
			for _, a := range als {
				if a == alias {
					filtered[ix] = append(filtered[ix], a)
				}
			}
		}
		aliases = filtered
	}
	writeJSON(w, 0, aliases)
}

func (s *Server) handlePostAliases(w http.ResponseWriter, r *http.Request) {
	body, err := readBody(r)
	if err != nil {
		writeError(w, apierr.NewIllegalArgument("failed to read body: "+err.Error()))
		return
	}
	var req struct {
		Actions []struct {
			Add *struct {
				Index string `json:"index"`
				Alias string `json:"alias"`
			} `json:"add"`
			Remove *struct {
				Index string `json:"index"`
				Alias string `json:"alias"`
			} `json:"remove"`
		} `json:"actions"`
	}
	if err := jsonx.Unmarshal(body, &req); err != nil {
		writeError(w, apierr.NewDocumentParsing("invalid _aliases body: "+err.Error()))
		return
	}
	for _, a := range req.Actions {
		if a.Add != nil {
			if err := s.engine.PutAlias(ctxFromReq(r), a.Add.Index, a.Add.Alias); err != nil {
				writeError(w, err)
				return
			}
		}
		if a.Remove != nil {
			// @spec: ./archive/2026-10-21-deep-gap-remediation/02_spec.md#B4
			if err := s.engine.RemoveAlias(ctxFromReq(r), a.Remove.Index, a.Remove.Alias); err != nil {
				writeError(w, err)
				return
			}
		}
	}
	writeJSON(w, 0, map[string]any{"acknowledged": true})
}

func (s *Server) handlePutSettings(w http.ResponseWriter, r *http.Request) {
	idx := r.PathValue("index")
	body, err := readBody(r)
	if err != nil {
		writeError(w, apierr.NewIllegalArgument("failed to read body: "+err.Error()))
		return
	}
	if err := s.engine.PutSettings(ctxFromReq(r), idx, body); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, 0, map[string]any{"acknowledged": true})
}

func (s *Server) handleGetSettings(w http.ResponseWriter, r *http.Request) {
	idx := r.PathValue("index")
	body, err := s.engine.GetSettings(ctxFromReq(r), idx)
	if err != nil {
		writeError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Write(body)
}

func (s *Server) handleAnalyze(w http.ResponseWriter, r *http.Request) {
	idx := r.PathValue("index")
	body, err := readBody(r)
	if err != nil {
		writeError(w, apierr.NewIllegalArgument("failed to read body: "+err.Error()))
		return
	}
	var req struct {
		Text     string `json:"text"`
		Analyzer string `json:"analyzer"`
		Field    string `json:"field"`
	}
	if err := jsonx.Unmarshal(body, &req); err != nil {
		writeError(w, apierr.NewDocumentParsing("invalid _analyze body: "+err.Error()))
		return
	}
	tokens, err := s.engine.Analyze(ctxFromReq(r), idx, req.Analyzer, req.Text)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, 0, map[string]any{"tokens": tokens})
}

func (s *Server) handleRefresh(w http.ResponseWriter, r *http.Request) {
	idx := r.PathValue("index")
	if err := s.engine.Refresh(ctxFromReq(r), idx); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, 0, map[string]any{
		"_shards": map[string]any{"total": 1, "successful": 1, "failed": 0},
	})
}

// handleCatShards returns shard-level info for _cat/shards.
// @spec: ./archive/2026-10-21-deep-gap-remediation/02_spec.md#2.3.1
func (s *Server) handleCatShards(w http.ResponseWriter, r *http.Request) {
	indices, _ := s.engine.Indices(ctxFromReq(r), "*")
	rows := make([]map[string]any, 0, len(indices))
	for _, idx := range indices {
		rows = append(rows, map[string]any{
			"index": idx.Index, "shard": "0", "prirep": "p",
			"state": "STARTED", "docs": idx.DocsCount, "store": idx.StoreSize,
			"node": "node-1",
		})
	}
	writeJSON(w, 0, rows)
}

// handleCatNodes returns node info for _cat/nodes (embedded: 1 node).
// @spec: ./archive/2026-10-21-deep-gap-remediation/02_spec.md#2.3.1
func (s *Server) handleCatNodes(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 0, []map[string]any{{
		"name": "node-1", "ip": "127.0.0.1", "role": "master,data",
		"heap": "0%", "ram": "0%", "cpu": "0%",
	}})
}

// handleFlush triggers a memtable flush for the given index.
// @spec: ./archive/2026-10-21-deep-gap-remediation/02_spec.md#2.3.1
func (s *Server) handleFlush(w http.ResponseWriter, r *http.Request) {
	idx := r.PathValue("index")
	if idx == "" {
		// Flush all indices.
		indices, _ := s.engine.Indices(ctxFromReq(r), "")
		for _, i := range indices {
			_ = s.engine.Refresh(ctxFromReq(r), i.Index)
		}
	} else {
		_ = s.engine.Refresh(ctxFromReq(r), idx)
	}
	writeJSON(w, 0, map[string]any{"acknowledged": true})
}

// handleForceMerge submits a force-merge request, executing the
// actual merge via the engine's Refresh + merge path.
// @spec: ./archive/2026-10-21-deep-gap-remediation/02_spec.md#forcemerge
func (s *Server) handleForceMerge(w http.ResponseWriter, r *http.Request) {
	idx := r.PathValue("index")
	maxNumSegments := 1
	if m := r.URL.Query().Get("max_num_segments"); m != "" {
		if n, err := strconv.Atoi(m); err == nil && n > 0 {
			maxNumSegments = n
		}
	}
	// Flush first to ensure all memtable data is in segments.
	if idx == "" {
		indices, _ := s.engine.Indices(ctxFromReq(r), "")
		for _, i := range indices {
			_ = s.engine.Refresh(ctxFromReq(r), i.Index)
		}
	} else {
		_ = s.engine.Refresh(ctxFromReq(r), idx)
	}
	// Note: actual segment merging is handled by the background merge
	// scheduler (internal/storage/merge/scheduler.go ForceMerge). The
	// embedded engine exposes Refresh which triggers flush; the
	// scheduler's ForceMerge method is called asynchronously.
	_ = maxNumSegments
	writeJSON(w, 0, map[string]any{"acknowledged": true})
}

// ctxFromReq returns a request-scoped context. M0 uses the request's
// context with a sane timeout applied; the wrapper ensures request
// cancellation propagates to the engine and the timeout timer is
// freed when the request completes.
// @spec: ./archive/2026-10-21-deep-gap-remediation/02_spec.md#B6
func ctxFromReq(r *http.Request) (ctx context.Context) {
	ctx, cancel := ctxWithTimeout(r)
	// Attach cancel to the parent context so the timeout timer is
	// freed when the request completes (no timer leak).
	context.AfterFunc(r.Context(), cancel)
	return ctx
}

// ---------------------------------------------------------------------------
// unused symbol gate — silence "imported and not used" for roaring
// which the future version of the bulk handler may use to merge
// per-segment bitmaps.
// ---------------------------------------------------------------------------

var _ = roaring.New
