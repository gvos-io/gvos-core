// Rate table administration: upsert, list.
package api

import (
	"github.com/lixuanqun/gvos-core/internal/billingdomain/money"
	"encoding/json"
	"net/http"

	"github.com/lixuanqun/gvos-core/internal/billingdomain/rate"
	"github.com/lixuanqun/gvos-core/internal/shared/events"
)

func (s *Server) handleRateUpsert(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) || !requirePOST(w, r) {
		return
	}
	var req struct {
		Prefix  string  `json:"prefix"`
		Price   float64 `json:"price"`
		InitDur int     `json:"init_dur"`
		StepDur int     `json:"step_dur"`
		MinDur  int     `json:"min_dur"`
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

	if s.pg != nil {
		if err := s.pg.UpsertRate(r.Context(), demoRateGroup, req.Prefix, priceAmt, req.InitDur, req.StepDur, req.MinDur); err != nil {
			httpError(w, 500, err.Error())
			return
		}
	} else {
		// Memory mode: upsert into rateList so rebuildRates sees it.
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
	}
	// Live rate table rebuild: the next call uses the new price.
	s.rebuildRates()

	s.audit(r, "upsert", "rate", req.Prefix, nil, req)
	// Broadcast to peer replicas: they rebuild their snapshot from the
	// authoritative store too ("60s fee window" is now instant).
	s.emit(events.SubjectCfgReload, map[string]string{"entity": "rate"})
	writeJSON(w, 200, map[string]any{"prefix": req.Prefix, "price": req.Price})
}

// GET /v1/admin/rates

func (s *Server) handleRateList(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	rows := []map[string]any{}
	for _, rr := range s.rateList {
		rows = append(rows, map[string]any{
			"prefix": rr.Prefix, "price": rr.Price,
			"init_dur": rr.InitDur, "step_dur": rr.StepDur, "min_dur": rr.MinDur,
		})
	}
	writeJSON(w, 200, map[string]any{"rates": rows, "count": len(rows)})
}

// audit records one admin mutation. The admin.changed event IS the
// audit record: with the AUDIT worker running (asyncAudit) the event
// alone is authoritative (at-least-once write by the consumer);
// without it, PG mode writes the row inline as the fallback. The
// event always flows — demo mode (no PG) streams too, so downstream
// consumers see one uniform feed.

const demoRateGroup = "00000000-0000-0000-0000-000000000010"
