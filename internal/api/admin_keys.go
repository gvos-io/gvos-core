// API-key administration (Phase B): create/revoke keys, bootstrap
// the first admin key from GVOS_ADMIN_TOKEN at startup.
package api

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/fastvoip/gvos-core/internal/shared/acl"
)

// handleKeyCreate serves POST /v1/admin/keys {"role","tenant_id"}.
// The plaintext is returned exactly once and never stored.
func (s *Server) handleKeyCreate(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	var req struct {
		Role     string `json:"role"`
		TenantID string `json:"tenant_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Role == "" {
		httpErrorC(w, 400, "BAD_REQUEST", "role required")
		return
	}
	role := acl.Role(req.Role)
	switch role {
	case acl.RoleAdmin, acl.RoleOperator, acl.RoleTenant, acl.RoleAgent:
	default:
		httpErrorC(w, 400, "BAD_REQUEST", "role must be admin|operator|tenant|agent")
		return
	}
	tenant := req.TenantID
	if tenant == "" {
		tenant = whoAmI(r).TenantID // inherit the creator's tenant
	}
	pt, prefix, hash := acl.GenerateKey(role)
	s.acl.Add(acl.Key{
		ID: prefix, TenantID: tenant, Role: role, Prefix: prefix,
		Hash: hash, CreatedBy: whoAmI(r).Prefix,
	})
	s.audit(r, "create", "api_key", prefix, nil, map[string]any{"role": role, "tenant_id": tenant})
	writeJSON(w, 201, map[string]any{
		"key": pt, // one-time display
		"prefix": prefix, "role": role, "tenant_id": tenant,
		"note": "store this key now; it is not retrievable later",
	})
}

// handleKeyList serves GET /v1/admin/keys.
func (s *Server) handleKeyList(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	out := []map[string]any{}
	for _, k := range s.acl.List() {
		row := map[string]any{"prefix": k.Prefix, "role": string(k.Role), "tenant_id": k.TenantID, "created_by": k.CreatedBy}
		if k.LastUsed != nil {
			row["last_used"] = k.LastUsed.Format(time.RFC3339)
		}
		out = append(out, row)
	}
	writeJSON(w, 200, map[string]any{"keys": out, "count": len(out)})
}

// handleKeyRevoke serves DELETE /v1/admin/keys/{prefix}.
func (s *Server) handleKeyRevoke(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	prefix := r.PathValue("prefix")
	if n := s.acl.Revoke(prefix); n == 0 {
		httpErrorC(w, 404, "NOT_FOUND", "no live key with prefix "+prefix)
		return
	}
	s.audit(r, "revoke", "api_key", prefix, nil, nil)
	writeJSON(w, 200, map[string]any{"revoked": prefix})
}

// BootstrapKey registers the first admin key at startup when no key
// exists. GVOS_ADMIN_TOKEN (when set) becomes the key material so
// scripted deployments are deterministic: the env value must already
// be a full gvos_admin_<hex> key. Without it, a fresh key is
// generated and printed once.
func BootstrapKey(s *Server) {
	if s.acl == nil || s.acl.Count() > 0 {
		return
	}
	var pt, prefix, hash string
	if env := os.Getenv("GVOS_ADMIN_TOKEN"); env != "" {
		pt = env
		prefix = env[:min(len(env), 16)] // display handle from the key head
		hash = acl.HashKey(env)
	} else {
		pt, prefix, hash = acl.GenerateKey(acl.RoleAdmin)
		log.Printf("BOOTSTRAP admin API key (one-time, store now): %s", pt)
	}
	s.acl.Add(acl.Key{
		ID: prefix, TenantID: demoTenant, Role: acl.RoleAdmin, Prefix: prefix,
		Hash: hash, CreatedBy: "bootstrap",
	})
}

// routeKeysLegacy stub for /v1/admin/keys verb routes (not previously
// existing — only resource routes).
func (s *Server) routeKeysLegacy(w http.ResponseWriter, r *http.Request) {
	s.handleKeyList(w, r)
}
