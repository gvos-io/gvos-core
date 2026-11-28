// Capability endpoints (gap analysis B-series): internal machinery
// that already existed — the account charging chain, per-account live
// concurrency, media stream stats and the LCR route preview — with
// nothing reading it. Each handler is a thin projection over the
// existing domain call.
package api

import (
	"encoding/json"
	"net/http"

	"github.com/gvos-io/gvos-core/internal/billingdomain/rate"
	"github.com/gvos-io/gvos-core/internal/customerdomain/gateway"
)

// handleAccountTree serves GET /v1/admin/accounts/{id}/tree: the
// charging chain end-account → agents → tenant root (charge order).
func (s *Server) handleAccountTree(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	id := r.PathValue("id")
	if mapped, ok := s.userIndex[id]; ok {
		id = mapped
	}
	chain, err := s.store.Chain(id)
	if err != nil {
		httpErrorC(w, 404, "NOT_FOUND", "no account "+id)
		return
	}
	writeJSON(w, 200, map[string]any{"chain": chain, "items": chain, "count": len(chain)})
}

// handleAccountLive serves GET /v1/admin/accounts/{id}/live: live
// calls against the account's limits (concurrent cap, cps cap) — the
// pair the ops dashboards page asks for first.
func (s *Server) handleAccountLive(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	id := r.PathValue("id")
	if mapped, ok := s.userIndex[id]; ok {
		id = mapped
	}
	acc, err := s.store.Get(id)
	if err != nil {
		httpErrorC(w, 404, "NOT_FOUND", "no account "+id)
		return
	}
	live := s.calls.LiveByAccounts(map[string]bool{id: true})
	writeJSON(w, 200, map[string]any{
		"account_id": id, "live_calls": live,
		"max_concurrent": acc.Concurrent, "max_cps": acc.CPS,
	})
}

// handleMediaStats serves GET /v1/admin/media/stats: global active
// stream count, or one call's stream health (chunks/bytes/RTT budget)
// when ?call_id= is given.
func (s *Server) handleMediaStats(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	if callID := r.URL.Query().Get("call_id"); callID != "" {
		st := s.media.Get(callID)
		if st == nil {
			httpErrorC(w, 404, "NOT_FOUND", "no live stream for "+callID)
			return
		}
		writeJSON(w, 200, map[string]any{"call_id": callID, "stats": st.Stats()})
		return
	}
	writeJSON(w, 200, map[string]any{
		"active_streams": s.media.Count(),
		"live_calls":     s.calls.Live(),
	})
}

// handleRoutePreview serves POST /v1/admin/routes/preview {dest,
// account_id?}: the LCR order the next broadcast would ship for this
// destination (cheapest first per the account's rate group, failure
// cooldown applied) — dry-run, routes nothing.
func (s *Server) handleRoutePreview(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) || !requirePOST(w, r) {
		return
	}
	var req struct {
		Dest      string `json:"dest"`
		AccountID string `json:"account_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Dest == "" {
		httpErrorC(w, 400, "BAD_REQUEST", "dest required")
		return
	}
	gws := s.gws.Gateways()
	if len(gws) == 0 {
		writeJSON(w, 200, map[string]any{"dest": req.Dest, "routes": []any{}, "count": 0})
		return
	}
	group := "demo"
	if req.AccountID != "" {
		if mapped, ok := s.userIndex[req.AccountID]; ok {
			req.AccountID = mapped
		}
		if acc, err := s.store.Get(req.AccountID); err == nil && acc.RateGroup != "" {
			group = acc.RateGroup
		}
	}
	rater := &lcrRater{s: s, cache: map[string]*rate.Table{}}
	ordered := gateway.LCR(gws, req.Dest, rater)
	out := make([]map[string]any, 0, len(ordered))
	for i, g := range ordered {
		row := map[string]any{"rank": i, "id": g.ID, "name": g.Name, "priority": g.Priority}
		if cost := rater.BestRate(g.CostRate, req.Dest); cost != nil {
			row["cost_per_min"] = cost.String()
		}
		if s.gwBreaker != nil {
			row["breaker"] = s.gwBreaker.View(g.ID)
		}
		out = append(out, row)
	}
	writeJSON(w, 200, map[string]any{"dest": req.Dest, "group": group, "routes": out, "items": out, "count": len(out)})
}
