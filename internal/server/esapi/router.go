// Package esapi router.go wires the Go 1.22+ ServeMux routes for the
// M0 REST surface (design 04 §2.2). Middleware (panic recovery,
// request logging) wraps every handler.
package esapi

import (
	"context"
	stderrors "errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	apierr "github.com/unisearch-db/unisearch/internal/errors"
	"github.com/unisearch-db/unisearch/internal/ingest"
	"github.com/unisearch-db/unisearch/internal/jsonx"
	"github.com/unisearch-db/unisearch/internal/observability"
	"github.com/unisearch-db/unisearch/internal/snapshot"
	"github.com/unisearch-db/unisearch/internal/synonyms"
)

// Server bundles the registered mux and bound Engine.
type Server struct {
	engine         Engine
	mux            *http.ServeMux
	pitMgr         *PITManager
	scrollMgr      *ScrollManager
	templates      sync.Map               // @spec: deep-gap-remediation 02_spec.md#2.3.3
	ilmPolicies    sync.Map               // @spec: deep-gap-remediation 02_spec.md#2.3.3
	synonymStore   *synonyms.SynonymStore // @spec: es-feature-gap-fill 02_spec.md#2.9
	asyncSearchMgr *AsyncSearchManager    // @spec: es-feature-gap-fill 02_spec.md#2.8
	pipelineStore  *ingest.PipelineStore  // @spec: es-feature-gap-fill 02_spec.md#2.7
	snapshotRepos  *snapshot.RepoRegistry // @spec: es-feature-gap-fill 02_spec.md#2.6
}

// NewServer registers every route and returns a Server ready to
// hand to http.Server.
func NewServer(engine Engine) *Server {
	pitMgr := NewPITManager()
	s := &Server{
		engine:         engine,
		mux:            http.NewServeMux(),
		pitMgr:         pitMgr,
		scrollMgr:      NewScrollManager(pitMgr),
		synonymStore:   synonyms.NewSynonymStore(),
		asyncSearchMgr: NewAsyncSearchManager(engine),
		pipelineStore:  ingest.NewPipelineStore(),
		snapshotRepos:  snapshot.NewRepoRegistry(""),
	}
	s.register()
	return s
}

// ServeHTTP dispatches through the mux. M0 single-process.
// Paths under /_synonyms/ and /_async_search/ with an id suffix are
// intercepted here to avoid Go 1.22 ServeMux pattern conflicts with
// /{index}/_mapping and /{index}/_doc/{id} (wildcard-vs-literal ambiguity).
// @spec: ./archive/2026-10-22-es-feature-gap-fill/02_spec.md#2.8
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	mw := chain(withRecovery, withMetricsAdapter, withTracingAdapter, withRequestLog)

	path := r.URL.Path
	// Intercept /_synonyms/{id} and /_synonyms/{id}/{rule_id} paths.
	if strings.HasPrefix(path, "/_synonyms/") && len(path) > len("/_synonyms/") {
		rest := path[len("/_synonyms/"):]
		if rest != "" {
			// Check for single-rule path: /_synonyms/{id}/{rule_id}
			if slashIdx := strings.Index(rest, "/"); slashIdx > 0 {
				setID := rest[:slashIdx]
				ruleID := rest[slashIdx+1:]
				if ruleID != "" {
					switch r.Method {
					case http.MethodPut:
						mw(s.makePutSynonymRuleHandler(setID, ruleID)).ServeHTTP(w, r)
						return
					case http.MethodGet:
						mw(s.makeGetSynonymRuleHandler(setID, ruleID)).ServeHTTP(w, r)
						return
					case http.MethodDelete:
						mw(s.makeDeleteSynonymRuleHandler(setID, ruleID)).ServeHTTP(w, r)
						return
					}
				}
			}
			// Set-level path: /_synonyms/{id}
			switch r.Method {
			case http.MethodPut:
				mw(s.makeSynonymPutHandler(rest)).ServeHTTP(w, r)
				return
			case http.MethodGet:
				mw(s.makeSynonymGetHandler(rest)).ServeHTTP(w, r)
				return
			case http.MethodDelete:
				mw(s.makeSynonymDeleteHandler(rest)).ServeHTTP(w, r)
				return
			}
		}
	}
	// Intercept /_async_search/{id} paths (but not /_async_search/status/{id}
	// which is registered in the mux).
	if strings.HasPrefix(path, "/_async_search/") &&
		!strings.HasPrefix(path, "/_async_search/status/") &&
		len(path) > len("/_async_search/") {
		id := path[len("/_async_search/"):]
		if id != "" {
			switch r.Method {
			case http.MethodGet:
				mw(s.makeAsyncSearchGetHandler(id)).ServeHTTP(w, r)
				return
			case http.MethodDelete:
				mw(s.makeAsyncSearchDeleteHandler(id)).ServeHTTP(w, r)
				return
			}
		}
	}
	// Intercept /_snapshot/ paths to avoid Go 1.22 ServeMux pattern conflicts.
	// @spec: ./archive/2026-10-22-es-feature-gap-fill/02_spec.md#2.6
	if strings.HasPrefix(path, "/_snapshot/") && len(path) > len("/_snapshot/") {
		rest := path[len("/_snapshot/"):]
		parts := strings.SplitN(rest, "/", 2)
		repo := parts[0]
		if repo == "" {
			s.mux.ServeHTTP(w, r)
			return
		}
		if len(parts) == 1 {
			// /_snapshot/{repo} — repo CRUD
			switch r.Method {
			case http.MethodPut:
				mw(s.makePutSnapshotRepoHandler(repo)).ServeHTTP(w, r)
				return
			case http.MethodGet:
				mw(s.makeGetSnapshotsHandler(repo)).ServeHTTP(w, r)
				return
			case http.MethodDelete:
				mw(s.makeDeleteSnapshotRepoHandler(repo)).ServeHTTP(w, r)
				return
			}
		} else {
			// /_snapshot/{repo}/{snapshot} or /_snapshot/{repo}/{snapshot}/_restore
			snapshot := parts[1]
			if strings.HasSuffix(snapshot, "/_restore") {
				snap := snapshot[:len(snapshot)-len("/_restore")]
				mw(s.makeRestoreSnapshotHandler(repo, snap)).ServeHTTP(w, r)
				return
			}
			if strings.HasSuffix(snapshot, "/_status") {
				snap := snapshot[:len(snapshot)-len("/_status")]
				mw(s.makeSnapshotStatusHandler(repo, snap)).ServeHTTP(w, r)
				return
			}
			if snapshot == "_verify" && r.Method == http.MethodPost {
				mw(s.makeVerifyRepoHandler(repo)).ServeHTTP(w, r)
				return
			}
			switch r.Method {
			case http.MethodPut:
				mw(s.makeCreateSnapshotHandler(repo, snapshot)).ServeHTTP(w, r)
				return
			case http.MethodGet:
				mw(s.makeGetSnapshotHandler(repo, snapshot)).ServeHTTP(w, r)
				return
			case http.MethodDelete:
				mw(s.makeDeleteSnapshotHandler(repo, snapshot)).ServeHTTP(w, r)
				return
			}
		}
	}
	// Intercept /_scripts/ paths to avoid Go 1.22 ServeMux pattern conflicts
	// with /{index}/_mapping (wildcard {id} at position 1 conflicts).
	// @spec: ./archive/2026-10-22-es-feature-gap-fill/02_spec.md#2.3.5
	if strings.HasPrefix(path, "/_scripts/") && len(path) > len("/_scripts/") {
		id := path[len("/_scripts/"):]
		if id != "" {
			switch r.Method {
			case http.MethodPut:
				mw(s.makePutScriptHandler(id)).ServeHTTP(w, r)
				return
			case http.MethodGet:
				mw(s.makeGetScriptHandler(id)).ServeHTTP(w, r)
				return
			case http.MethodDelete:
				mw(s.makeDeleteScriptHandler(id)).ServeHTTP(w, r)
				return
			}
		}
	}
	s.mux.ServeHTTP(w, r)
}

// register installs the route table. Method+path combos use Go 1.22
// pattern syntax; index placeholders are decoded by handlers.
func (s *Server) register() {
	mw := chain(withRecovery, withMetricsAdapter, withTracingAdapter, withRequestLog)

	// Prometheus metrics endpoint (not wrapped by mw to avoid self-instrumentation).
	s.mux.Handle("GET /_prometheus/metrics", observability.MetricsHandler())

	s.mux.HandleFunc("GET /{$}", mw(s.handleRoot))
	s.mux.HandleFunc("GET /_cluster/health", mw(s.handleClusterHealth))

	s.mux.HandleFunc("GET /_cat/health", mw(s.handleCatHealth))
	s.mux.HandleFunc("GET /_cat/indices", mw(s.handleCatIndices))
	s.mux.HandleFunc("GET /_cat/indices/{index}", mw(s.handleCatIndices))

	// Index lifecycle: PUT /{index} creates; DELETE /{index} drops.
	s.mux.HandleFunc("PUT /{index}", mw(s.handlePutIndex))
	s.mux.HandleFunc("DELETE /{index}", mw(s.handleDeleteIndex))

	// _doc CRUD (R17.3).
	s.mux.HandleFunc("POST /{index}/_doc", mw(s.handleIndexAutoID))
	s.mux.HandleFunc("POST /{index}/_doc/{id}", mw(s.handleIndexDoc))
	s.mux.HandleFunc("PUT /{index}/_doc/{id}", mw(s.handleIndexDoc))
	s.mux.HandleFunc("POST /{index}/_create/{id}", mw(s.handleCreateDoc))
	s.mux.HandleFunc("GET /{index}/_doc/{id}", mw(s.handleGetDoc))
	s.mux.HandleFunc("DELETE /{index}/_doc/{id}", mw(s.handleDeleteDoc))

	// _bulk (R17.4).
	s.mux.HandleFunc("POST /_bulk", mw(s.handleBulk))
	s.mux.HandleFunc("POST /{index}/_bulk", mw(s.handleBulk))

	// _search (R17.5).
	s.mux.HandleFunc("POST /{index}/_search", mw(s.handleSearch))
	s.mux.HandleFunc("POST /_search", mw(s.handleSearch))
	s.mux.HandleFunc("GET /{index}/_search", mw(s.handleSearch))
	s.mux.HandleFunc("GET /_search", mw(s.handleSearch))
	s.mux.HandleFunc("POST /{index}/_knn_search", mw(s.handleSearch))

	// _percolate: match a submitted document against the index's
	// registered percolator queries (ES 1.x-style convenience API).
	s.mux.HandleFunc("POST /{index}/_percolate", mw(s.handlePercolate))

	// _count and _mget.
	s.mux.HandleFunc("POST /{index}/_count", mw(s.handleCount))
	s.mux.HandleFunc("POST /_count", mw(s.handleCount))
	s.mux.HandleFunc("GET /{index}/_count", mw(s.handleCount))
	s.mux.HandleFunc("GET /_count", mw(s.handleCount))
	s.mux.HandleFunc("POST /_mget", mw(s.handleMGet))
	s.mux.HandleFunc("POST /{index}/_mget", mw(s.handleMGet))

	// mapping/alias/settings/analyze/refresh (R17.5).
	s.mux.HandleFunc("PUT /{index}/_mapping", mw(s.handlePutMapping))
	s.mux.HandleFunc("GET /{index}/_mapping", mw(s.handleGetMapping))
	s.mux.HandleFunc("PUT /_mapping", mw(s.handlePutMapping)) // global, applies to all
	s.mux.HandleFunc("PUT /{index}/_alias", mw(s.handlePutAlias))
	s.mux.HandleFunc("PUT /{index}/_alias/{name}", mw(s.handlePutAlias))
	s.mux.HandleFunc("GET /{index}/_alias", mw(s.handleGetAliases))
	s.mux.HandleFunc("GET /_alias", mw(s.handleGetAliases))
	s.mux.HandleFunc("POST /_aliases", mw(s.handlePostAliases))
	s.mux.HandleFunc("PUT /{index}/_settings", mw(s.handlePutSettings))
	s.mux.HandleFunc("GET /{index}/_settings", mw(s.handleGetSettings))
	s.mux.HandleFunc("POST /{index}/_analyze", mw(s.handleAnalyze))
	s.mux.HandleFunc("POST /_analyze", mw(s.handleAnalyze))
	s.mux.HandleFunc("POST /{index}/_refresh", mw(s.handleRefresh))
	s.mux.HandleFunc("POST /_refresh", mw(s.handleRefresh))

	// M1 new routes (W5.8).
	s.mux.HandleFunc("POST /_msearch", mw(s.handleMSearch))
	s.mux.HandleFunc("POST /{index}/_msearch", mw(s.handleMSearch))
	s.mux.HandleFunc("GET /{index}/_field_caps", mw(s.handleFieldCaps))
	s.mux.HandleFunc("GET /_field_caps", mw(s.handleFieldCaps))
	s.mux.HandleFunc("GET /{index}/_terms_enum", mw(s.handleTermsEnum))
	s.mux.HandleFunc("GET /{index}/_explain/{id}", mw(s.handleExplain))
	s.mux.HandleFunc("POST /{index}/_delete_by_query", mw(s.handleDeleteByQuery))
	s.mux.HandleFunc("POST /{index}/_update_by_query", mw(s.handleUpdateByQuery))
	s.mux.HandleFunc("POST /_reindex", mw(s.handleReindex))
	s.mux.HandleFunc("PUT /_index_template", mw(s.handleIndexTemplate))
	s.mux.HandleFunc("GET /_index_template", mw(s.handleIndexTemplate))
	s.mux.HandleFunc("PUT /_ilm/policy", mw(s.handleILM))
	s.mux.HandleFunc("GET /_ilm/policy", mw(s.handleILM))

	// M1 additional routes (deep-gap-remediation).
	// @spec: ./archive/2026-10-21-deep-gap-remediation/02_spec.md#2.3.1
	s.mux.HandleFunc("GET /_cat/shards", mw(s.handleCatShards))
	s.mux.HandleFunc("GET /_cat/nodes", mw(s.handleCatNodes))
	s.mux.HandleFunc("POST /{index}/_flush", mw(s.handleFlush))
	s.mux.HandleFunc("POST /_flush", mw(s.handleFlush))
	s.mux.HandleFunc("POST /{index}/_forcemerge", mw(s.handleForceMerge))
	s.mux.HandleFunc("POST /_forcemerge", mw(s.handleForceMerge))

	// PIT / scroll (W5.4).
	s.mux.HandleFunc("POST /{index}/_pit", mw(s.handleCreatePIT))
	s.mux.HandleFunc("DELETE /pit", mw(s.handleDeletePIT))
	s.mux.HandleFunc("POST /_search/scroll", mw(s.handleScroll))
	s.mux.HandleFunc("DELETE /_search/scroll", mw(s.handleDeleteScroll))

	// Synonyms (es-feature-gap-fill P0).
	// @spec: ./archive/2026-10-22-es-feature-gap-fill/02_spec.md#2.9
	// NOTE: /_synonyms/{id} routes are intercepted in ServeHTTP before
	// the mux to avoid Go 1.22 ServeMux pattern conflicts with
	// /{index}/_mapping and /{index}/_doc/{id}. Only /_synonyms (list)
	// is registered here.
	s.mux.HandleFunc("GET /_synonyms", mw(s.handleGetSynonyms))

	// Async search (es-feature-gap-fill P0).
	// @spec: ./archive/2026-10-22-es-feature-gap-fill/02_spec.md#2.8
	// NOTE: /_async_search/{id} routes are intercepted in ServeHTTP.
	// Only POST /_async_search (submit) and GET /_async_search/status/{id}
	// are registered here (no pattern conflict).
	s.mux.HandleFunc("POST /_async_search", mw(s.handleSubmitAsyncSearch))
	s.mux.HandleFunc("GET /_async_search/status/{id}", mw(s.handleGetAsyncSearchStatus))

	// Term Vectors (es-feature-gap-fill P0).
	// @spec: ./archive/2026-10-22-es-feature-gap-fill/02_spec.md#2.10
	s.mux.HandleFunc("GET /{index}/_termvectors/{id}", mw(s.handleTermVectors))
	s.mux.HandleFunc("POST /{index}/_termvectors/{id}", mw(s.handleTermVectors))
	s.mux.HandleFunc("GET /_mtermvectors", mw(s.handleMTermVectors))
	s.mux.HandleFunc("POST /_mtermvectors", mw(s.handleMTermVectors))

	// Index admin (es-feature-gap-fill P0+P1).
	// @spec: ./archive/2026-10-22-es-feature-gap-fill/02_spec.md#2.4
	s.mux.HandleFunc("POST /{index}/_open", mw(s.handleOpenIndex))
	s.mux.HandleFunc("POST /{index}/_close", mw(s.handleCloseIndex))
	s.mux.HandleFunc("GET /{index}/_stats", mw(s.handleIndexStats))
	s.mux.HandleFunc("GET /_stats", mw(s.handleIndexStats))
	s.mux.HandleFunc("GET /{index}/_segments", mw(s.handleSegments))
	s.mux.HandleFunc("GET /_segments", mw(s.handleSegments))
	s.mux.HandleFunc("GET /_resolve/index/{name}", mw(s.handleResolveIndex))

	// Cluster ops (es-feature-gap-fill P0+P1).
	// @spec: ./archive/2026-10-22-es-feature-gap-fill/02_spec.md#2.5
	s.mux.HandleFunc("GET /_cluster/state", mw(s.handleClusterState))
	s.mux.HandleFunc("GET /_cluster/state/{metric}", mw(s.handleClusterState))
	s.mux.HandleFunc("GET /_cluster/stats", mw(s.handleClusterStats))
	s.mux.HandleFunc("GET /_nodes/stats", mw(s.handleNodesStats))
	s.mux.HandleFunc("GET /_nodes/info", mw(s.handleNodesInfo))
	s.mux.HandleFunc("GET /_nodes", mw(s.handleNodesInfo))
	s.mux.HandleFunc("GET /_cluster/settings", mw(s.handleGetClusterSettings))
	s.mux.HandleFunc("PUT /_cluster/settings", mw(s.handlePutClusterSettings))

	// _cat extensions (es-feature-gap-fill P1).
	s.mux.HandleFunc("GET /_cat/aliases", mw(s.handleCatAliases))
	s.mux.HandleFunc("GET /_cat/segments", mw(s.handleCatSegments))
	s.mux.HandleFunc("GET /_cat/templates", mw(s.handleCatTemplates))
	s.mux.HandleFunc("GET /_cat/recovery", mw(s.handleCatRecovery))
	s.mux.HandleFunc("GET /_cat/tasks", mw(s.handleCatTasks))
	s.mux.HandleFunc("GET /_cat/thread_pool", mw(s.handleCatThreadPool))

	// _validate/query (es-feature-gap-fill P1).
	s.mux.HandleFunc("GET /{index}/_validate/query", mw(s.handleValidateQuery))
	s.mux.HandleFunc("POST /{index}/_validate/query", mw(s.handleValidateQuery))

	// _search_shards (es-feature-gap-fill P2).
	s.mux.HandleFunc("GET /{index}/_search_shards", mw(s.handleSearchShards))
	s.mux.HandleFunc("POST /{index}/_search_shards", mw(s.handleSearchShards))
	s.mux.HandleFunc("GET /_search_shards", mw(s.handleSearchShards))

	// search_template (es-feature-gap-fill P2).
	s.mux.HandleFunc("POST /_search/template", mw(s.handleSearchTemplate))
	s.mux.HandleFunc("POST /{index}/_search/template", mw(s.handleSearchTemplate))
	s.mux.HandleFunc("POST /_render/template", mw(s.handleRenderTemplate))

	// Index ops (es-feature-gap-fill P2+P3).
	s.mux.HandleFunc("POST /{index}/_rollover", mw(s.handleRolloverIndex))
	s.mux.HandleFunc("POST /{index}/_rollover/{target}", mw(s.handleRolloverIndex))
	s.mux.HandleFunc("POST /{index}/_reload_search_analyzers", mw(s.handleReloadSearchAnalyzers))

	// P3 cluster handlers (es-feature-gap-fill P3).
	s.mux.HandleFunc("GET /_nodes/usage", mw(s.handleNodesUsage))
	s.mux.HandleFunc("GET /_nodes/capabilities", mw(s.handleNodesCapabilities))
	s.mux.HandleFunc("GET /_remote/info", mw(s.handleRemoteInfo))

	// Index ops P2: shrink/split/clone (es-feature-gap-fill P2).
	s.mux.HandleFunc("POST /{index}/_shrink/{target}", mw(s.handleShrinkIndex))
	s.mux.HandleFunc("POST /{index}/_split/{target}", mw(s.handleSplitIndex))
	s.mux.HandleFunc("POST /{index}/_clone/{target}", mw(s.handleCloneIndex))

	// Ingest Pipeline (es-feature-gap-fill P0).
	// @spec: ./archive/2026-10-22-es-feature-gap-fill/02_spec.md#2.7
	s.mux.HandleFunc("PUT /_ingest/pipeline/{id}", mw(s.handlePutPipeline))
	s.mux.HandleFunc("GET /_ingest/pipeline", mw(s.handleGetPipelines))
	s.mux.HandleFunc("GET /_ingest/pipeline/{id}", mw(s.handleGetPipeline))
	s.mux.HandleFunc("DELETE /_ingest/pipeline/{id}", mw(s.handleDeletePipeline))
	s.mux.HandleFunc("POST /_ingest/pipeline/{id}/_simulate", mw(s.handleSimulatePipeline))
	s.mux.HandleFunc("POST /_ingest/pipeline/_simulate", mw(s.handleSimulatePipeline))

	// Snapshot (es-feature-gap-fill P0).
	// @spec: ./archive/2026-10-22-es-feature-gap-fill/02_spec.md#2.6
	// NOTE: All /_snapshot/ sub-paths are intercepted in ServeHTTP to
	// avoid Go 1.22 ServeMux pattern conflicts with /{index}/_doc/{id}
	// (wildcard {repo} at position 1 conflicts with literal _doc).
	// Only GET /_snapshot (list repos) is registered here.
	s.mux.HandleFunc("GET /_snapshot", mw(s.handleListSnapshotRepos))
}

// writeJSON sets Content-Type and writes a JSON body. Status defaults
// to 200; pass 0 to keep the default. Errors are surfaced through APIError.
// body may be a JSON-marshalable value, a raw []byte of pre-encoded
// JSON, or a json.RawMessage — the latter two are written verbatim.
func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if status != 0 {
		w.WriteHeader(status)
	}
	switch v := body.(type) {
	case []byte:
		_, _ = w.Write(v)
		if len(v) == 0 || v[len(v)-1] != '\n' {
			_, _ = w.Write([]byte("\n"))
		}
		return
	case jsonx.RawMessage:
		_, _ = w.Write(v)
		_, _ = w.Write([]byte("\n"))
		return
	}
	enc := jsonx.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(body)
}

// writeError emits an ES-shaped error body via APIError.
// setDeprecationWarning adds the ES-style deprecation Warning header
// (design 04 §8: `299 unisearch-8.12 "..."`). Used for doc_values:false,
// norms, scroll→PIT migration, and other deprecated features.
// @spec: ./archive/2026-10-21-deep-gap-remediation/02_spec.md#2.3.5
func setDeprecationWarning(w http.ResponseWriter, msg string) {
	w.Header().Add("Warning", fmt.Sprintf(`299 unisearch-8.12 "%s"`, msg))
}

func writeError(w http.ResponseWriter, err error) {
	if err == nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	var apiErr *apierr.APIError
	if stderrors.As(err, &apiErr) {
		writeJSON(w, apiErr.Status, apiErr.ResponseBody())
		return
	}
	apiErr = apierr.NewInternal(err.Error())
	writeJSON(w, apiErr.Status, apiErr.ResponseBody())
}

// withRecovery catches panics from any handler and converts them
// to a 500 APIError so a single buggy handler never kills the process.
func withRecovery(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				slog.Error("panic in HTTP handler",
					"path", r.URL.Path,
					"method", r.Method,
					"panic", rec,
					"stack", string(debug.Stack()),
				)
				apiErr := apierr.NewInternal("internal server error")
				writeJSON(w, apiErr.Status, apiErr.ResponseBody())
			}
		}()
		next(w, r)
	}
}

// withRequestLog emits one slog line per request at Info level
// (method, path, duration, status). M0 keeps it light; M2 will
// add trace_id propagation.
func withRequestLog(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rw := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next(rw, r)
		slog.Info("http",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rw.status,
			"duration_ms", time.Since(start).Milliseconds(),
		)
	}
}

// withMetricsAdapter wraps the observability.WithMetrics middleware
// to match the chain's func(http.HandlerFunc) http.HandlerFunc signature.
func withMetricsAdapter(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		observability.WithMetrics(next).ServeHTTP(w, r)
	}
}

// withTracingAdapter wraps the observability.WithTracing middleware.
func withTracingAdapter(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		observability.WithTracing(next).ServeHTTP(w, r)
	}
}

// statusRecorder captures the response status for request logging.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// chain composes middleware right-to-left so the first item in the
// list runs outermost (first to install, last to tear down).
func chain(mws ...func(http.HandlerFunc) http.HandlerFunc) func(http.HandlerFunc) http.HandlerFunc {
	return func(h http.HandlerFunc) http.HandlerFunc {
		for i := len(mws) - 1; i >= 0; i-- {
			h = mws[i](h)
		}
		return h
	}
}

// readBody returns the request body as bytes, capped at maxBodyBytes
// (100MB per ES _bulk line limit, applied loosely here at the whole
// request level for simplicity).
const maxBodyBytes = 100 * 1024 * 1024

func readBody(r *http.Request) ([]byte, error) {
	if r.Body == nil {
		return nil, nil
	}
	return io.ReadAll(io.LimitReader(r.Body, maxBodyBytes))
}

// parseIndices expands the {index} path param: comma-separated names,
// "_all" / "*" stay literal for the engine to expand. Trailing
// whitespace is stripped. Brace expansion (e.g. "logs-{a,b}-*") is
// applied before comma splitting to avoid mis-splitting brace groups.
// @spec: ./archive/2026-10-21-deep-gap-remediation/02_spec.md#B5
func parseIndices(r *http.Request) []string {
	idx := r.PathValue("index")
	if idx == "" || idx == "_all" || idx == "*" {
		return nil
	}
	parts := strings.Split(idx, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, expandBraces(p)...)
		}
	}
	return out
}

// resolveWildcards expands wildcard patterns (e.g. "logs-*") against
// the engine's index list. Non-wildcard names pass through unchanged.
// @spec: ./archive/2026-10-21-deep-gap-remediation/02_spec.md#2.3.6
func (s *Server) resolveWildcards(ctx context.Context, indices []string) []string {
	hasWildcard := false
	for _, idx := range indices {
		if strings.ContainsAny(idx, "*?") {
			hasWildcard = true
			break
		}
	}
	if !hasWildcard {
		return indices
	}
	all, _ := s.engine.Indices(ctx, "")
	if len(all) == 0 {
		return indices
	}
	resolved := make([]string, 0, len(indices))
	for _, pattern := range indices {
		if strings.ContainsAny(pattern, "*?") {
			for _, info := range all {
				if globMatch(pattern, info.Index) {
					resolved = append(resolved, info.Index)
				}
			}
		} else {
			resolved = append(resolved, pattern)
		}
	}
	return resolved
}

// globMatch matches a simple glob pattern (supports * and ?).
func globMatch(pattern, name string) bool {
	pi, ni := 0, 0
	starPI, starNI := -1, -1
	for ni < len(name) {
		if pi < len(pattern) && (pattern[pi] == name[ni] || pattern[pi] == '?') {
			pi++
			ni++
		} else if pi < len(pattern) && pattern[pi] == '*' {
			starPI = pi
			starNI = ni
			pi++
		} else if starPI != -1 {
			pi = starPI + 1
			starNI++
			ni = starNI
		} else {
			return false
		}
	}
	for pi < len(pattern) && pattern[pi] == '*' {
		pi++
	}
	return pi == len(pattern)
}

// expandBraces expands brace notation like "logs-{a,b}-*" into
// ["logs-a-*", "logs-b-*"]. No-op when no braces are present.
// @spec: ./archive/2026-10-21-deep-gap-remediation/02_spec.md#B5
func expandBraces(s string) []string {
	start := strings.IndexByte(s, '{')
	if start < 0 {
		return []string{s}
	}
	end := strings.IndexByte(s[start+1:], '}')
	if end < 0 {
		return []string{s} // no closing brace, return as-is
	}
	end = start + 1 + end
	if end <= start+1 {
		return []string{s} // empty braces
	}
	prefix := s[:start]
	suffix := s[end+1:]
	options := strings.Split(s[start+1:end], ",")
	result := make([]string, 0, len(options))
	for _, opt := range options {
		opt = strings.TrimSpace(opt)
		if opt != "" {
			result = append(result, prefix+opt+suffix)
		}
	}
	if len(result) == 0 {
		return []string{s}
	}
	return result
}

// refreshParam returns the value of ?refresh (true|wait_for|false).
func refreshParam(r *http.Request) string {
	return r.URL.Query().Get("refresh")
}

// ctxWithTimeout returns a context with the request as parent and a
// sensible budget. _bulk uses 60s (design 00 §6.2), other requests
// default to 30s. The ?timeout query parameter overrides the default
// (e.g. ?timeout=10s).
// @spec: ./archive/2026-10-21-deep-gap-remediation/02_spec.md#1.2.4
func ctxWithTimeout(r *http.Request) (context.Context, context.CancelFunc) {
	d := 30 * time.Second
	if strings.Contains(r.URL.Path, "_bulk") {
		d = 60 * time.Second
	}
	if raw := r.URL.Query().Get("timeout"); raw != "" {
		if parsed, err := time.ParseDuration(raw); err == nil && parsed > 0 {
			d = parsed
		}
	}
	return context.WithTimeout(r.Context(), d)
}
