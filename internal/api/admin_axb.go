// AXB privacy-number administration (the VOS AXB module's open
// equivalent): bind/unbind/list virtual numbers.
package api

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/fastvoip/gvos-core/internal/customerdomain/axb"
)

// handleAXBBind serves POST /v1/admin/axb/bindings
// {"a_number","b_number","ttl_hours"}: allocates X, returns the
// binding. Repeat calls with the same pair reuse the live binding.
func (s *Server) handleAXBBind(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	var req struct {
		ANumber  string  `json:"a_number"`
		BNumber  string  `json:"b_number"`
		TTLHours float64 `json:"ttl_hours"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.ANumber == "" || req.BNumber == "" {
		httpErrorC(w, 400, "BAD_REQUEST", "a_number and b_number required")
		return
	}
	ttl := time.Duration(req.TTLHours * float64(time.Hour))
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	bd, err := s.axb.Bind(req.ANumber, req.BNumber, ttl)
	if err != nil {
		httpErrorC(w, 400, "BAD_REQUEST", err.Error())
		return
	}
	s.audit(r, "create", "axb_binding", bd.ID, nil, bd)
	writeJSON(w, 201, bd)
}

// handleAXBList serves GET /v1/admin/axb/bindings?a=&b= (optional
// filters; active bindings only).
func (s *Server) handleAXBList(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	a, b := r.URL.Query().Get("a"), r.URL.Query().Get("b")
	out := []axb.Binding{}
	for _, bd := range s.axb.List() {
		if a != "" && bd.ANumber != a {
			continue
		}
		if b != "" && bd.BNumber != b {
			continue
		}
		out = append(out, bd)
	}
	writeJSON(w, 200, map[string]any{"bindings": out, "count": len(out)})
}

// handleAXBDel serves DELETE /v1/admin/axb/bindings/{x}: revoke by
// virtual number (immediate; in-flight calls settle normally).
func (s *Server) handleAXBDel(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	x := r.PathValue("x")
	if s.axb.RevokeByX(x) == 0 {
		httpErrorC(w, 404, "NOT_FOUND", "no active binding for "+x)
		return
	}
	s.audit(r, "revoke", "axb_binding", x, nil, nil)
	writeJSON(w, 200, map[string]any{"revoked": x})
}

// handleAXBStats serves GET /v1/admin/axb/stats (binding count).
func (s *Server) handleAXBStats(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	writeJSON(w, 200, map[string]any{"active_bindings": s.axb.Count()})
}
