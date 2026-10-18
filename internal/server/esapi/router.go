// Package esapi router.go wires the Go 1.22+ ServeMux routes for the
// M0 REST surface (design 04 §2.2). Middleware (panic recovery,
// request logging) wraps every handler.
package esapi

import (
	"context"
	stderrors "errors"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strings"
	"time"

	apierr "github.com/unisearch-db/unisearch/internal/errors"
)

// Server bundles the registered mux and bound Engine.
type Server struct {
	engine Engine
	mux    *http.ServeMux
}

// NewServer registers every M0 route and returns a Server ready to
// hand to http.Server.
func NewServer(engine Engine) *Server {
	s := &Server{engine: engine, mux: http.NewServeMux()}
	s.register()
	return s
}

// ServeHTTP dispatches through the mux. M0 single-process.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

// register installs the route table. Method+path combos use Go 1.22
// pattern syntax; index placeholders are decoded by handlers.
func (s *Server) register() {
	mw := chain(withRecovery, withRequestLog)

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
	case json.RawMessage:
		_, _ = w.Write(v)
		_, _ = w.Write([]byte("\n"))
		return
	}
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(body)
}

// writeError emits an ES-shaped error body via APIError.
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
// whitespace is stripped.
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
			out = append(out, p)
		}
	}
	return out
}

// refreshParam returns the value of ?refresh (true|wait_for|false).
func refreshParam(r *http.Request) string {
	return r.URL.Query().Get("refresh")
}

// ctxWithTimeout returns a context with the request as parent and a
// sensible M0 budget. Production wiring will allow override via
// ?timeout=.
func ctxWithTimeout(r *http.Request) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), 30*time.Second)
}
