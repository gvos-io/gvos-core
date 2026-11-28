// Tenant administration (gap analysis A1): the tenants table existed
// since migration 001 with one seeded row and no API — multi-tenancy
// was schema-only. PG mode owns the table; memory mode is the demo
// tenant read-only (tenant creation without PG persistence would fork
// the tree authority).
package api

import (
	"encoding/json"
	"net/http"
	"strings"
)

// handleTenantAdminList serves GET /v1/admin/tenants (optional
// ?deleted=1 includes soft-deleted rows).
func (s *Server) handleTenantAdminList(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	if s.pg == nil {
		writeJSON(w, 200, map[string]any{"tenants": []map[string]any{{"id": demoTenant, "name": "demo", "currency": "CNY"}}, "items": []map[string]any{{"id": demoTenant, "name": "demo", "currency": "CNY"}}, "count": 1, "mode": "memory"})
		return
	}
	rows, err := s.pg.ListTenants(r.Context(), r.URL.Query().Get("deleted") == "1")
	if err != nil {
		httpError(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"tenants": rows, "items": rows, "count": len(rows)})
}

// handleTenantAdminCreate serves POST /v1/admin/tenants {name,
// currency?}: provisions a tree root. Currency is per-tenant (CDR
// money stays decimal-string; the currency rides CDR/report rows).
func (s *Server) handleTenantAdminCreate(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) || !requirePOST(w, r) {
		return
	}
	if s.pg == nil {
		httpErrorC(w, 503, "UNAVAILABLE", "tenant provisioning requires PG mode (memory mode is the single demo tenant)")
		return
	}
	var req struct {
		Name     string `json:"name"`
		Currency string `json:"currency"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Name == "" {
		httpErrorC(w, 400, "BAD_REQUEST", "name required")
		return
	}
	if len(req.Name) > 128 {
		httpErrorC(w, 400, "BAD_REQUEST", "name too long")
		return
	}
	if req.Currency == "" {
		req.Currency = "CNY"
	}
	if len(req.Currency) != 3 {
		httpErrorC(w, 400, "BAD_REQUEST", "currency must be a 3-letter code")
		return
	}
	id, err := s.pg.CreateTenant(r.Context(), req.Name, req.Currency)
	if err != nil {
		if strings.Contains(err.Error(), "duplicate") {
			httpErrorC(w, 409, "DUPLICATE", "tenant name already exists")
			return
		}
		httpError(w, 500, err.Error())
		return
	}
	s.audit(r, "create", "tenant", id, nil, req)
	writeJSON(w, 201, map[string]any{"id": id, "name": req.Name, "currency": req.Currency})
}

// handleTenantAdminPatch serves PATCH /v1/admin/tenants/{id}.
func (s *Server) handleTenantAdminPatch(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) || !requirePATCH(w, r) {
		return
	}
	if s.pg == nil {
		httpErrorC(w, 503, "UNAVAILABLE", "tenant administration requires PG mode")
		return
	}
	id := r.PathValue("id")
	var req struct {
		Name     string `json:"name"`
		Currency string `json:"currency"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Name == "" {
		httpErrorC(w, 400, "BAD_REQUEST", "name required")
		return
	}
	if len(req.Currency) > 3 {
		httpErrorC(w, 400, "BAD_REQUEST", "currency must be a 3-letter code")
		return
	}
	if req.Currency == "" {
		req.Currency = "CNY"
	}
	if n, err := s.pg.UpdateTenant(r.Context(), id, req.Name, req.Currency); err != nil {
		httpError(w, 500, err.Error())
		return
	} else if n == 0 {
		httpErrorC(w, 404, "NOT_FOUND", "no such tenant")
		return
	}
	s.audit(r, "update", "tenant", id, nil, req)
	writeJSON(w, 200, map[string]any{"updated": id})
}

// handleTenantAdminDelete serves DELETE /v1/admin/tenants/{id}:
// soft-delete, refused while live accounts reference the tree root.
func (s *Server) handleTenantAdminDelete(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	if s.pg == nil {
		httpErrorC(w, 503, "UNAVAILABLE", "tenant administration requires PG mode")
		return
	}
	id := r.PathValue("id")
	if n, err := s.pg.DeleteTenant(r.Context(), id); err != nil {
		httpError(w, 500, err.Error())
		return
	} else if n == 0 {
		if rows, lerr := s.pg.ListTenants(r.Context(), true); lerr == nil {
			for _, t := range rows {
				if t.ID == id {
					httpErrorC(w, 409, "IN_USE", "tenant still has live accounts; delete or move them first")
					return
				}
			}
		}
		httpErrorC(w, 404, "NOT_FOUND", "no such tenant")
		return
	}
	s.audit(r, "delete", "tenant", id, nil, nil)
	writeJSON(w, 200, map[string]any{"deleted": id})
}

// handleSettingsList serves GET /v1/admin/settings: the runtime
// settings keys with their shapes — read-only by design. Every
// mutable setting has a dedicated owning API (gateways/dids/recording/
// packages/risk_rules/ip_allowlist); a generic write face would fork
// the authority those handlers own.
func (s *Server) handleSettingsList(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	if s.pg == nil {
		httpErrorC(w, 503, "UNAVAILABLE", "settings inspection requires PG mode")
		return
	}
	keys := []string{"gateways", "dids", "recording", "packages", "risk_rules", "ip_allowlist"}
	out := map[string]json.RawMessage{}
	for _, k := range keys {
		var raw json.RawMessage
		if err := s.pg.GetSetting(r.Context(), k, &raw); err == nil {
			out[k] = raw
		} else {
			out[k] = json.RawMessage(`null`)
		}
	}
	writeJSON(w, 200, map[string]any{"settings": out, "items": out, "mode": "readonly", "note": "mutations go through their owning APIs (/v1/admin/gateways, /dids, /recording, /packages, /risk-rules, /ipallow)"})
}
