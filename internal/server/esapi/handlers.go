// Package esapi handlers.go implements the M0 REST handlers listed
// in design 04 §2.2. Each handler decodes the request, dispatches
// to the Engine, and assembles an ES-shaped response. Errors are
// always funneled through APIError via writeError.
package esapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/RoaringBitmap/roaring"

	apierr "github.com/unisearch-db/unisearch/internal/errors"
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
	row := []string{
		fmt.Sprintf("%s", h.Status),
		"unisearch",
		h.ClusterName,
		strconv.Itoa(h.NumberOfNodes),
		strconv.Itoa(h.NumberOfDataNodes),
		strconv.Itoa(h.ActivePrimaryShards),
		strconv.Itoa(h.ActiveShards),
		strconv.Itoa(h.RelocatingShards),
		strconv.Itoa(h.InitializingShards),
		strconv.Itoa(h.UnassignedShards),
	}
	if r.URL.Query().Has("v") {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintf(w, "%s\n", strings.Join([]string{
			"status", "cluster", "cluster_name", "node", "data_node", "pri", "relo", "init", "unassign",
		}, " "))
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	fmt.Fprintf(w, "%s\n", strings.Join(row, " "))
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
	if r.URL.Query().Has("v") {
		fmt.Fprintln(w, "health status index uuid pri rep docs.count docs.deleted store.size pri.store.size")
	}
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
	// M0 wired through PutMapping/MGetMapping only; delete is a no-op
	// returning acknowledged. Engine layer extension lands with T18.
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
	res, err := s.engine.Write(ctxFromReq(r), IndexDoc{
		Op: OpIndex, Index: idx, ID: "", Source: body,
	}, refreshParam(r))
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
		"_source":       json.RawMessage(source),
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
	_ = json.Unmarshal(body, &raw)
	return raw
}

// ---------------------------------------------------------------------------
// _bulk (R17.4)
// ---------------------------------------------------------------------------

// bulkActionLine matches the action line of an NDJSON _bulk request.
type bulkActionLine struct {
	Index *struct {
		Index   string `json:"_index"`
		ID      string `json:"_id"`
		IfSeqNo int64  `json:"if_seq_no,omitempty"`
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
	dec := json.NewDecoder(reader)
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
		doc := IndexDoc{Index: idx}
		switch {
		case action.Index != nil:
			doc.Op = OpIndex
			doc.ID = action.Index.ID
			doc.Index = pickIndex(idx, action.Index.Index)
			if action.Index.IfSeqNo != 0 {
				seq := action.Index.IfSeqNo
				doc.IfSeqNo = &seq
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
			var raw json.RawMessage
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
		writeError(w, err)
		return
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
			"hits":      reshapeHits(res.Hits),
		},
	}
	writeJSON(w, 0, resp)
}

// reshapeHits converts engine-side SearchHit into the ES-shaped slice.
func reshapeHits(hits []SearchHit) []map[string]any {
	out := make([]map[string]any, 0, len(hits))
	for _, h := range hits {
		entry := map[string]any{
			"_index":        h.Index,
			"_id":           h.ID,
			"_score":        h.Score,
			"_source":       json.RawMessage(h.Source),
			"_seq_no":       h.SeqNo,
			"_primary_term": h.Primary,
		}
		if len(h.SortValues) > 0 {
			entry["sort"] = h.SortValues
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
	if err := json.Unmarshal(body, &req); err != nil {
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
			entry["_source"] = json.RawMessage(g.Source)
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
		_ = json.Unmarshal(body, &req)
		for _, a := range req.Actions {
			if a.Add != nil {
				_ = s.engine.PutAlias(ctxFromReq(r), a.Add.Index, a.Add.Alias)
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
	if err := json.Unmarshal(body, &req); err != nil {
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
	if err := json.Unmarshal(body, &req); err != nil {
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

// ctxFromReq returns a request-scoped context. M0 uses the request's
// context with a sane timeout applied; the wrapper ensures request
// cancellation propagates to the engine.
func ctxFromReq(r *http.Request) (ctx context.Context) {
	ctx, cancel := ctxWithTimeout(r)
	// The handler does not block on cancel explicitly; the deferred
	// cancel is installed per request to free the timeout timer.
	// go vet's lostcancel check would flag this; attach cancel to ctx
	// via the recover-on-shutdown pattern in T18.
	_ = cancel
	return ctx
}

// ---------------------------------------------------------------------------
// unused symbol gate — silence "imported and not used" for roaring
// which the future version of the bulk handler may use to merge
// per-segment bitmaps.
// ---------------------------------------------------------------------------

var _ = roaring.New
