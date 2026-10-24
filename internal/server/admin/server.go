package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/unisearch-db/unisearch/internal/server/esapi"
	"github.com/unisearch-db/unisearch/internal/tenant"
)

// AdminServer serves the web admin console on a separate port. It embeds
// the Vue 3 frontend via go:embed, proxies ES API requests to the main
// server, and exposes new admin-only endpoints (tenant CRUD, dashboard
// aggregation, JSON metrics).
type AdminServer struct {
	engine    esapi.Engine
	tenantMgr *tenant.Manager
	proxy     http.Handler
	static    *staticHandler
	mux       *http.ServeMux
}

// NewAdminServer creates a new admin server. esapiAddr is the address of
// the ES API server (e.g. "localhost:9200") for reverse proxying.
func NewAdminServer(engine esapi.Engine, mgr *tenant.Manager, esapiAddr string) (*AdminServer, error) {
	proxy, err := newReverseProxy(esapiAddr)
	if err != nil {
		return nil, fmt.Errorf("admin: create reverse proxy: %w", err)
	}

	s := &AdminServer{
		engine:    engine,
		tenantMgr: mgr,
		proxy:     proxy,
		static:    newStaticHandler(),
		mux:       http.NewServeMux(),
	}
	s.registerRoutes()
	return s, nil
}

func (s *AdminServer) registerRoutes() {
	s.mux.HandleFunc("/api/proxy/", s.handleProxy)
	s.mux.HandleFunc("/api/tenants", s.handleTenants)
	s.mux.HandleFunc("/api/tenants/", s.handleTenantByID)
	s.mux.HandleFunc("/api/dashboard", s.handleDashboard)
	s.mux.HandleFunc("/api/metrics", s.handleMetrics)
	// Catch-all for static files and SPA fallback.
	s.mux.HandleFunc("/", s.handleStatic)
}

// ServeHTTP dispatches incoming requests to the appropriate handler.
func (s *AdminServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

func (s *AdminServer) handleProxy(w http.ResponseWriter, r *http.Request) {
	s.proxy.ServeHTTP(w, r)
}

func (s *AdminServer) handleStatic(w http.ResponseWriter, r *http.Request) {
	// Only allow GET for static files.
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	s.static.ServeHTTP(w, r)
}

// --- helpers ---

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Error("admin: write json error", "err", err)
	}
}

func writeError(w http.ResponseWriter, status int, errType, message string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{
			"type":   errType,
			"reason": message,
		},
	})
}

func (s *AdminServer) handleDashboard(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "only GET is supported")
		return
	}
	s.serveDashboard(w, r)
}

func (s *AdminServer) handleMetrics(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "only GET is supported")
		return
	}
	s.serveMetrics(w, r)
}

func (s *AdminServer) handleTenants(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.listTenants(w, r)
	case http.MethodPost:
		s.createTenant(w, r)
	default:
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "only GET and POST are supported")
	}
}

func (s *AdminServer) handleTenantByID(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/api/tenants/")
	if id == "" {
		writeError(w, http.StatusBadRequest, "illegal_argument", "tenant id is required")
		return
	}

	switch r.Method {
	case http.MethodGet:
		s.getTenant(w, r, id)
	case http.MethodPut:
		s.updateTenant(w, r, id)
	case http.MethodDelete:
		s.deleteTenant(w, r, id)
	default:
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "only GET, PUT, and DELETE are supported")
	}
}

// dashboardData is the aggregated response for GET /api/dashboard.
type dashboardData struct {
	Health      *esapi.ClusterHealth `json:"health"`
	Indices     []esapi.IndexInfo    `json:"indices"`
	IndexCount  int                  `json:"index_count"`
	DocCount    int64                `json:"doc_count"`
	StoreSize   string               `json:"store_size"`
	TenantCount int                  `json:"tenant_count"`
}

func (s *AdminServer) serveDashboard(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	data := dashboardData{}

	if health, err := s.engine.Health(ctx); err == nil {
		data.Health = health
	} else {
		slog.Warn("admin: dashboard health error", "err", err)
	}

	if indices, err := s.engine.Indices(ctx, ""); err == nil {
		data.Indices = indices
		data.IndexCount = len(indices)
		for _, idx := range indices {
			data.DocCount += idx.DocsCount
		}
	} else {
		slog.Warn("admin: dashboard indices error", "err", err)
	}

	if s.tenantMgr != nil {
		data.TenantCount = len(s.tenantMgr.List())
	}

	writeJSON(w, http.StatusOK, data)
}

// serveMetrics reads the Prometheus registry and returns a JSON snapshot.
func (s *AdminServer) serveMetrics(w http.ResponseWriter, r *http.Request) {
	s.serveMetricsJSON(w, r)
}

// ensure ctx is used to avoid lint issues in dashboard handler
var _ = context.Background
