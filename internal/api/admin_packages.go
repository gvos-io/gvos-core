// Subscription package administration and usage.
package api

import (
	"encoding/json"
	"net/http"

	"github.com/lixuanqun/gvos-core/internal/billingdomain/pkgplan"
	"github.com/lixuanqun/gvos-core/internal/shared/events"
)

func (s *Server) handlePackages(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) || !requirePOST(w, r) {
		return
	}
	var req struct {
		ID          string  `json:"id"`
		Name        string  `json:"name"`
		Cycle       string  `json:"cycle"`
		FreeMinutes int     `json:"free_minutes"`
		Discount    float64 `json:"discount"`
		MinSpend    float64 `json:"min_spend"`
		Assign      string  `json:"assign_account"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.ID == "" {
		httpError(w, 400, "id required")
		return
	}
	c := pkgplan.Daily
	if req.Cycle == "monthly" {
		c = pkgplan.Monthly
	}
	pkg := pkgplan.Package{
		ID: req.ID, Name: firstNonEmpty(req.Name, req.ID), Cycle: c,
		FreeMinutes: req.FreeMinutes, Discount: req.Discount, MinSpend: req.MinSpend,
	}
	s.pkg.Define(pkg)
	s.audit(r, "define", "package", req.ID, nil, req)
	s.emit(events.SubjectCfgReload, map[string]any{"entity": "package", "op": "define", "package": pkg})
	if req.Assign != "" {
		// Resolve the assign target to the account's internal ID: the
		// billing path (pkg.Apply) keys on AccountID, and callers
		// naturally pass the SIP username (same resolution rule as the
		// caller path in callStart).
		assignID := req.Assign
		if _, err := s.store.Get(assignID); err != nil {
			if id, ok := s.userIndex[assignID]; ok {
				assignID = id
			} else {
				httpError(w, 400, "assign_account not found: "+req.Assign)
				return
			}
		}
		if err := s.pkg.Assign(assignID, req.ID); err != nil {
			httpError(w, 400, err.Error())
			return
		}
		s.audit(r, "assign", "package", req.ID, nil, map[string]any{"account": assignID})
		s.emit(events.SubjectCfgReload, map[string]any{"entity": "package", "op": "assign", "account": assignID, "package_id": req.ID})
	}
	// One durability write covers both mutations (define+assign).
	if s.pg != nil {
		if err := s.pg.PutSetting(r.Context(), "packages", s.pkg.Snapshot()); err != nil {
			httpError(w, 500, err.Error())
			return
		}
	}
	writeJSON(w, 200, map[string]any{"id": req.ID, "assigned": req.Assign})
}

// GET /v1/admin/packages/usage?account= - cycle usage snapshot.

func (s *Server) handlePackageUsage(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	acc := r.URL.Query().Get("account")
	free, spend, ok := s.pkg.Usage(acc)
	short := s.pkg.MinSpendShortfall(acc)
	writeJSON(w, 200, map[string]any{
		"account": acc, "assigned": ok,
		"free_secs_used": free, "cycle_spend": spend, "min_spend_shortfall": short,
	})
}
