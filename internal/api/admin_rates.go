// Rate table administration: upsert, list.
package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/gvos-io/gvos-core/internal/billingdomain/billing"
	"github.com/gvos-io/gvos-core/internal/billingdomain/money"

	"github.com/gvos-io/gvos-core/internal/billingdomain/rate"
	"github.com/gvos-io/gvos-core/internal/shared/events"
)

func (s *Server) handleRateUpsert(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	var req struct {
		Prefix  string  `json:"prefix"`
		Price   float64 `json:"price"`
		InitDur int     `json:"init_dur"`
		StepDur int     `json:"step_dur"`
		MinDur  int     `json:"min_dur"`
		Group   string  `json:"group"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Prefix == "" || req.Price < 0 {
		httpError(w, 400, "prefix and non-negative price required")
		return
	}
	priceAmt, err := money.FromYuan(req.Price)
	if err != nil {
		httpErrorC(w, 400, "BAD_AMOUNT", err.Error())
		return
	}
	if req.InitDur == 0 {
		req.InitDur = 60
	}
	if req.StepDur == 0 {
		req.StepDur = 6
	}

	group := req.Group
	if group == "" {
		group = "demo"
	}
	if s.pg != nil {
		gid, ok, err := s.pg.ResolveRateGroup(r.Context(), group)
		if err != nil {
			httpError(w, 500, err.Error())
			return
		} else if !ok {
			httpErrorC(w, 400, "BAD_REQUEST", "unknown rate group "+group)
			return
		}
		if err := s.pg.UpsertRate(r.Context(), gid, req.Prefix, priceAmt, req.InitDur, req.StepDur, req.MinDur); err != nil {
			httpError(w, 500, err.Error())
			return
		}
	} else if group != "demo" {
		httpErrorC(w, 400, "BAD_REQUEST", "memory mode prices from the single demo group")
		return
	} else {
		// Memory mode: upsert into rateList so rebuildRates sees it.
		s.cfgMu.Lock()
		replaced := false
		for i, rr := range s.rateList {
			if rr.Prefix == req.Prefix {
				s.rateList[i] = rate.Rate{
					Prefix: req.Prefix, Price: priceAmt,
					InitDur: req.InitDur, StepDur: req.StepDur, MinDur: req.MinDur,
				}
				replaced = true
				break
			}
		}
		if !replaced {
			s.rateList = append(s.rateList, rate.Rate{
				Prefix: req.Prefix, Price: priceAmt,
				InitDur: req.InitDur, StepDur: req.StepDur, MinDur: req.MinDur,
			})
		}
		s.cfgMu.Unlock()
	}
	// Live rate table rebuild: the next call uses the new price.
	s.rebuildRates()

	s.audit(r, "upsert", "rate", req.Prefix, nil, req)
	// Broadcast to peer replicas: they rebuild their snapshot from the
	// authoritative store too ("60s fee window" is now instant).
	s.emit(events.SubjectCfgReload, map[string]string{"entity": "rate"})
	// Rate changes re-price the gateways: re-broadcast the LCR order
	// (cheapest-first may have flipped) alongside the rate event.
	s.emit(events.SubjectCfgReload, map[string]any{"entity": "gateway", "gateways": s.lcrGateways()})
	writeJSON(w, 200, map[string]any{"prefix": req.Prefix, "price": req.Price})
}

// GET /v1/admin/rates

func (s *Server) handleRateList(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	group := r.URL.Query().Get("group")
	type rowOut struct {
		Group   string `json:"group"`
		Prefix  string `json:"prefix"`
		Price   any    `json:"price"`
		InitDur int    `json:"init_dur"`
		StepDur int    `json:"step_dur"`
		MinDur  int    `json:"min_dur"`
	}
	if s.pg != nil {
		ctx, cancel := contextWithTimeout(r.Context())
		defer cancel()
		groups := map[string][]rate.Rate{}
		if group != "" {
			if rows, err := s.pg.LoadRateRows(ctx, group); err == nil {
				groups[group] = rows
			}
		} else if all, err := s.pg.LoadRateGroups(ctx); err == nil {
			groups = all
		}
		rows := make([]map[string]any, 0)
		for g, list := range groups {
			for _, rr := range list {
				rows = append(rows, map[string]any{
					"group": g, "prefix": rr.Prefix, "price": rr.Price.String(),
					"init_dur": rr.InitDur, "step_dur": rr.StepDur, "min_dur": rr.MinDur,
				})
			}
		}
		writeJSON(w, 200, map[string]any{"rates": rows, "items": rows, "count": len(rows)})
		return
	}
	if group != "" && group != "demo" {
		httpErrorC(w, 400, "BAD_REQUEST", "memory mode has a single implicit demo group")
		return
	}
	s.cfgMu.RLock()
	rows := make([]map[string]any, 0, len(s.rateList))
	for _, rr := range s.rateList {
		rows = append(rows, map[string]any{
			"group": "demo", "prefix": rr.Prefix, "price": rr.Price.String(),
			"init_dur": rr.InitDur, "step_dur": rr.StepDur, "min_dur": rr.MinDur,
		})
	}
	s.cfgMu.RUnlock()
	writeJSON(w, 200, map[string]any{"rates": rows, "items": rows, "count": len(rows)})
}

// audit records one admin mutation. The admin.changed event IS the
// audit record: with the AUDIT worker running (asyncAudit) the event
// alone is authoritative (at-least-once write by the consumer);
// without it, PG mode writes the row inline as the fallback. The
// event always flows — demo mode (no PG) streams too, so downstream
// consumers see one uniform feed.

// DELETE /v1/admin/rates/{prefix}: retire the rate (PG rows keep
// history via ended_at; the memory list drops it). Broadcast entity=rate
// — peers rebuild from PG (authoritative) or their own list (memory
// mode, single-process demo).
func (s *Server) handleRateDelete(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	prefix := r.PathValue("prefix")
	if s.pg != nil {
		var (
			tag int
			err error
		)
		if group := r.URL.Query().Get("group"); group != "" {
			tag, err = s.pg.EndRateRowsInGroup(r.Context(), group, prefix)
		} else {
			tag, err = s.pg.EndRateRows(r.Context(), prefix)
		}
		if err != nil {
			httpError(w, 500, err.Error())
			return
		}
		if tag == 0 {
			httpErrorC(w, 404, "NOT_FOUND", "no live rate for prefix "+prefix)
			return
		}
	} else {
		s.cfgMu.Lock()
		kept := s.rateList[:0]
		found := false
		for _, rr := range s.rateList {
			if rr.Prefix == prefix {
				found = true
				continue
			}
			kept = append(kept, rr)
		}
		s.rateList = kept
		s.rates = rate.NewTable(s.rateList)
		s.cfgMu.Unlock()
		if !found {
			httpErrorC(w, 404, "NOT_FOUND", "no live rate for prefix "+prefix)
			return
		}
	}
	s.audit(r, "delete", "rate", prefix, nil, nil)
	s.emit(events.SubjectCfgReload, map[string]any{"entity": "rate"})
	writeJSON(w, 200, map[string]any{"deleted": prefix})
}

// GET /v1/admin/rate-groups — the defined groups with live-rate
// counts (memory mode: the single implicit demo group).
func (s *Server) handleRateGroupList(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	if s.pg == nil {
		out := []map[string]any{{"name": "demo", "live_rates": len(s.rateList)}}
		writeJSON(w, 200, map[string]any{"groups": out, "items": out, "count": 1, "mode": "memory"})
		return
	}
	groups, err := s.pg.ListRateGroups(r.Context())
	if err != nil {
		httpError(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"groups": groups, "items": groups, "count": len(groups)})
}

// POST /v1/admin/rate-groups {"name"}: define a group (idempotent).
// PG mode owns the table; memory mode has a single implicit group.
func (s *Server) handleRateGroupCreate(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) || !requirePOST(w, r) {
		return
	}
	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Name == "" {
		httpErrorC(w, 400, "BAD_REQUEST", "name required")
		return
	}
	req.Name = strings.ToLower(strings.TrimSpace(req.Name))
	if len(req.Name) > 64 {
		httpErrorC(w, 400, "BAD_REQUEST", "name too long")
		return
	}
	if s.pg == nil {
		httpErrorC(w, 503, "UNAVAILABLE", "rate groups require PG mode (memory mode prices from the single demo group)")
		return
	}
	if err := s.pg.CreateRateGroup(r.Context(), req.Name); err != nil {
		httpError(w, 500, err.Error())
		return
	}
	s.audit(r, "create", "rate_group", req.Name, nil, req)
	writeJSON(w, 201, map[string]any{"name": req.Name})
}

// DELETE /v1/admin/rate-groups/{name}: removed only when it carries
// no live rates AND no account references it (the check constraint is
// pricing integrity, not just tidiness).
func (s *Server) handleRateGroupDelete(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	name := r.PathValue("name")
	if name == "demo" {
		httpErrorC(w, 409, "PROTECTED", "the demo group is the pricing fallback and cannot be removed")
		return
	}
	if s.pg == nil {
		httpErrorC(w, 503, "UNAVAILABLE", "rate groups require PG mode")
		return
	}
	if accs, err := s.store.List(); err == nil {
		for _, a := range accs {
			if a != nil && a.RateGroup == name {
				httpErrorC(w, 409, "IN_USE", "accounts still reference group "+name)
				return
			}
		}
	}
	if n, err := s.pg.DeleteRateGroup(r.Context(), name); err != nil {
		httpError(w, 500, err.Error())
		return
	} else if n == 0 {
		if ok, _ := s.pg.RateGroupExists(r.Context(), name); ok {
			httpErrorC(w, 409, "IN_USE", "group still carries live rates; retire them first")
			return
		}
		httpErrorC(w, 404, "NOT_FOUND", "no rate group "+name)
		return
	}
	s.audit(r, "delete", "rate_group", name, nil, nil)
	writeJSON(w, 200, map[string]any{"deleted": name})
}

// POST /v1/admin/rates/quote {dest, seconds, account_id?|group?}: the
// list-price estimate the dialer/operator sees before dialing (the
// package allowance applies only at settlement).
func (s *Server) handleRateQuote(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) || !requirePOST(w, r) {
		return
	}
	var req struct {
		Dest      string `json:"dest"`
		Seconds   int    `json:"seconds"`
		AccountID string `json:"account_id"`
		Group     string `json:"group"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Dest == "" {
		httpErrorC(w, 400, "BAD_REQUEST", "dest required")
		return
	}
	if req.Seconds <= 0 || req.Seconds > 86400 {
		httpErrorC(w, 400, "BAD_REQUEST", "seconds must be 1..86400")
		return
	}
	group := req.Group
	if group == "" && req.AccountID == "" {
		group = "demo"
	}
	if req.AccountID != "" {
		acc, err := s.store.Get(req.AccountID)
		if err != nil {
			s.cfgMu.RLock()
			mapped, ok := s.userIndex[req.AccountID]
			s.cfgMu.RUnlock()
			if ok {
				acc, err = s.store.Get(mapped)
			}
		}
		if err != nil {
			httpErrorC(w, 404, "NOT_FOUND", "no account "+req.AccountID)
			return
		}
		group = acc.RateGroup
	}
	if group == "" {
		group = "demo"
	}
	now := time.Now()
	s.cfgMu.RLock()
	tbl := s.rates
	if s.rateTables != nil {
		if t := s.rateTables[group]; t != nil {
			tbl = t
		}
	}
	s.cfgMu.RUnlock()
	entry := tbl.Lookup(req.Dest, int(now.Weekday()), now.Hour()*60+now.Minute())
	if entry == nil {
		httpErrorC(w, 404, "NOT_FOUND", "no rate for destination "+req.Dest+" in group "+group)
		return
	}
	est := (&billing.Charger{}).EstimatedCost(entry, req.Seconds)
	writeJSON(w, 200, map[string]any{
		"dest": req.Dest, "group": group, "seconds": req.Seconds,
		"prefix": entry.Prefix, "price_per_min": entry.Price.String(),
		"init_dur": entry.InitDur, "step_dur": entry.StepDur, "min_dur": entry.MinDur,
		"est_cost": est.String(),
	})
}
