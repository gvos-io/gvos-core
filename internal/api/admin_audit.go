// Audit-trail and ledger reads (P1 M7): the write paths existed since
// migration 001 (every admin mutation appends, every balance movement
// lands in balance_ledger) with no reader — "谁改了什么" and 对账
// needed a DB console. Both faces are PG-mode only: memory mode keeps
// no audit/journal store, and pretending otherwise would return
// silently empty trails.
package api

import (
	"encoding/json"
	"net/http"
)

// handleAuditList serves GET /v1/admin/audit
// ?entity=&entity_id=&tenant_id=&limit=&cursor= (newest first,
// keyset cursor).
func (s *Server) handleAuditList(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	if s.pg == nil {
		httpErrorC(w, 503, "UNAVAILABLE", "audit history requires PG mode (memory mode keeps no trail)")
		return
	}
	limit := 50
	if l := r.URL.Query().Get("limit"); l != "" {
		if n, err := json.Number(l).Int64(); err == nil && n > 0 && n <= 500 {
			limit = int(n)
		}
	}
	rows, next, err := s.pg.QueryAudit(r.Context(),
		r.URL.Query().Get("entity"), r.URL.Query().Get("entity_id"),
		r.URL.Query().Get("tenant_id"), limit, r.URL.Query().Get("cursor"))
	if err != nil {
		if len(err.Error()) > 9 && err.Error()[:9] == "bad cursor" {
			httpErrorC(w, 400, "BAD_CURSOR", err.Error())
			return
		}
		httpError(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"audit": rows, "items": rows, "count": len(rows), "next_cursor": next})
}

// handleAccountLedger serves GET /v1/admin/accounts/{id}/ledger
// ?limit=&cursor= — the account's balance movements (topup, call
// settle, refund, adjustment) with post-movement balances.
func (s *Server) handleAccountLedger(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	if s.pg == nil {
		httpErrorC(w, 503, "UNAVAILABLE", "balance ledger history requires PG mode (memory mode keeps no journal)")
		return
	}
	id := r.PathValue("id")
	if mapped, ok := s.userIndex[id]; ok {
		id = mapped // username → account id
	}
	if _, err := s.store.Get(id); err != nil {
		httpErrorC(w, 404, "NOT_FOUND", "no account "+id)
		return
	}
	limit := 50
	if l := r.URL.Query().Get("limit"); l != "" {
		if n, err := json.Number(l).Int64(); err == nil && n > 0 && n <= 500 {
			limit = int(n)
		}
	}
	rows, next, err := s.pg.QueryLedger(r.Context(), id, limit, r.URL.Query().Get("cursor"))
	if err != nil {
		if len(err.Error()) > 9 && err.Error()[:9] == "bad cursor" {
			httpErrorC(w, 400, "BAD_CURSOR", err.Error())
			return
		}
		httpError(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ledger": rows, "items": rows, "count": len(rows), "next_cursor": next})
}
