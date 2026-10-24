package admin

import (
	"encoding/json"
	"net/http"

	"github.com/unisearch-db/unisearch/internal/tenant"
)

// tenantSummary is the JSON representation of a tenant's quota and usage.
type tenantSummary struct {
	ID            string `json:"id"`
	StorageBytes  int64  `json:"storage_bytes"`
	VectorCount   int64  `json:"vector_count"`
	QueryQPS      int    `json:"query_qps"`
	WriteTPS      int    `json:"write_tps"`
	StorageUsed   int64  `json:"storage_used"`
	VectorsUsed   int64  `json:"vectors_used"`
}

// createTenantRequest is the body for POST /api/tenants.
type createTenantRequest struct {
	TenantID string         `json:"tenant_id"`
	Quota    tenant.Quota   `json:"quota"`
}

func rgToSummary(rg *tenant.ResourceGroup) tenantSummary {
	q := rg.Quota()
	storageUsed, vectorsUsed := rg.Usage()
	return tenantSummary{
		ID:           rg.TenantID(),
		StorageBytes: q.StorageBytes,
		VectorCount:  q.VectorCount,
		QueryQPS:     q.QueryQPS,
		WriteTPS:     q.WriteTPS,
		StorageUsed:  storageUsed,
		VectorsUsed:  vectorsUsed,
	}
}

func (s *AdminServer) listTenants(w http.ResponseWriter, r *http.Request) {
	if s.tenantMgr == nil {
		writeJSON(w, http.StatusOK, []tenantSummary{})
		return
	}
	rgs := s.tenantMgr.List()
	summaries := make([]tenantSummary, 0, len(rgs))
	for _, rg := range rgs {
		summaries = append(summaries, rgToSummary(rg))
	}
	writeJSON(w, http.StatusOK, summaries)
}

func (s *AdminServer) createTenant(w http.ResponseWriter, r *http.Request) {
	var req createTenantRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "parse_exception", "invalid JSON body: "+err.Error())
		return
	}
	if req.TenantID == "" {
		writeError(w, http.StatusBadRequest, "illegal_argument", "tenant_id is required")
		return
	}

	s.tenantMgr.Register(req.TenantID, req.Quota)
	rg := s.tenantMgr.Get(req.TenantID)
	if rg == nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "tenant registration failed")
		return
	}
	writeJSON(w, http.StatusCreated, rgToSummary(rg))
}

func (s *AdminServer) getTenant(w http.ResponseWriter, r *http.Request, id string) {
	rg := s.tenantMgr.Get(id)
	if rg == nil {
		writeError(w, http.StatusNotFound, "not_found", "tenant not found: "+id)
		return
	}
	writeJSON(w, http.StatusOK, rgToSummary(rg))
}

func (s *AdminServer) updateTenant(w http.ResponseWriter, r *http.Request, id string) {
	var req createTenantRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "parse_exception", "invalid JSON body: "+err.Error())
		return
	}
	// Register overwrites the existing resource group with a new quota.
	s.tenantMgr.Register(id, req.Quota)
	rg := s.tenantMgr.Get(id)
	if rg == nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "tenant update failed")
		return
	}
	writeJSON(w, http.StatusOK, rgToSummary(rg))
}

func (s *AdminServer) deleteTenant(w http.ResponseWriter, r *http.Request, id string) {
	if !s.tenantMgr.Delete(id) {
		writeError(w, http.StatusNotFound, "not_found", "tenant not found: "+id)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
