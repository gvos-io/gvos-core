// Hourly/daily report rollup and report views.
package api

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/lixuanqun/gvos-core/internal/shared/events"
)

func (s *Server) startReportWorker() {
	if s.pg == nil {
		return
	}
	run := func() {
		ctx, cancel := contextWithTimeoutDuration(context.Background(), 30*time.Second)
		defer cancel()
		for _, day := range []string{todayStr(-1), todayStr(0)} {
			if n, err := s.pg.RollupDaily(ctx, day); err != nil {
				logPrintf("REPORT rollup %s failed: %v", day, err)
			} else if n > 0 {
				logPrintf("REPORT rollup %s: %d account-rows", day, n)
				// Downstream (reporting, webhook consumers) learns the
				// rollup completed — the manual trigger emits the same
				// event via audit().
				s.emit(events.SubjectAdminChanged, map[string]any{
					"action": "rollup", "entity": "usage_daily", "entity_id": day,
					"rows": n,
				})
			}
		}
	}
	go func() {
		run() // immediate first pass
		t := time.NewTicker(time.Hour)
		defer t.Stop()
		for range t.C {
			run()
		}
	}()
}

func todayStr(offsetDays int) string {
	return time.Now().AddDate(0, 0, offsetDays).Format("2006-01-02")
}

// POST /v1/admin/reports {"day":"2026-10-07"} - manual rollup trigger;
// GET /v1/admin/reports?day=&account=&view=daily|gateway - report read.

func (s *Server) handleReports(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	if s.pg == nil {
		httpError(w, 503, "reports need postgres mode")
		return
	}
	switch r.Method {
	case http.MethodPost:
		var req struct {
			Day string `json:"day"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Day == "" {
			req.Day = todayStr(0)
		}
		ctx, cancel := contextWithTimeout(r.Context())
		defer cancel()
		n, err := s.pg.RollupDaily(ctx, req.Day)
		if err != nil {
			httpError(w, 500, err.Error())
			return
		}
		s.audit(r, "rollup", "usage_daily", req.Day, nil, map[string]any{"rows": n})
		writeJSON(w, 200, map[string]any{"day": req.Day, "account_rows": n})
	case http.MethodGet:
		day := r.URL.Query().Get("day")
		account := r.URL.Query().Get("account")
		view := r.URL.Query().Get("view")
		ctx, cancel := contextWithTimeout(r.Context())
		defer cancel()
		if view == "blindzone" || view == "cross" {
			s.handleReportViews(w, r)
			return
		}
		if view == "gateway" {
			rows, err := s.pg.QueryGatewayDaily(ctx, day)
			if err != nil {
				httpError(w, 500, err.Error())
				return
			}
			writeJSON(w, 200, map[string]any{"view": "gateway", "rows": rows, "count": len(rows)})
			return
		}
		rows, err := s.pg.QueryDaily(ctx, day, account)
		if err != nil {
			httpError(w, 500, err.Error())
			return
		}
		writeJSON(w, 200, map[string]any{"view": "daily", "rows": rows, "count": len(rows)})
	default:
		httpError(w, 405, "GET or POST")
	}
}

// POST /v1/admin/packages {"id","cycle":"daily|monthly","free_minutes",
// "discount","min_spend"} + optional {"assign_account":"<id>"}.
// GET /v1/admin/packages -> list definitions.

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// GET /v1/admin/reports?view=blindzone&min_calls=5   盲区分析
// GET /v1/admin/reports?view=cross&day=...           账户×目的地交叉

func (s *Server) handleReportViews(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) || !requireGET(w, r) {
		return
	}
	if s.pg == nil {
		httpError(w, 503, "reports need postgres mode")
		return
	}
	view := r.URL.Query().Get("view")
	ctx, cancel := contextWithTimeout(r.Context())
	defer cancel()
	switch view {
	case "blindzone":
		minCalls := 5
		if v := r.URL.Query().Get("min_calls"); v != "" {
			if n, err := json.Number(v).Int64(); err == nil {
				minCalls = int(n)
			}
		}
		rows, err := s.pg.QueryBlindZones(ctx, minCalls)
		if err != nil {
			httpError(w, 500, err.Error())
			return
		}
		writeJSON(w, 200, map[string]any{"view": "blindzone", "rows": rows, "count": len(rows)})
	case "cross":
		rows, err := s.pg.QueryCrossAnalysis(ctx, r.URL.Query().Get("day"))
		if err != nil {
			httpError(w, 500, err.Error())
			return
		}
		writeJSON(w, 200, map[string]any{"view": "cross", "rows": rows, "count": len(rows)})
	default:
		httpError(w, 400, "view must be blindzone|cross")
	}
}

// requireGET guards read-only endpoints.


// handleReportList serves GET /v1/admin/reports (read side of the
// write/read split).
func (s *Server) handleReportList(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	if s.pg == nil {
		httpError(w, 503, "reports need postgres mode")
		return
	}
	ctx, cancel := contextWithTimeout(r.Context())
	defer cancel()
	day := r.URL.Query().Get("day")
	if day == "" {
		day = todayStr(0)
	}
	var rows any
	var err error
	if account := r.URL.Query().Get("account"); account != "" {
		rows, err = s.pg.QueryDaily(ctx, day, account)
	} else {
		rows, err = s.pg.QueryGatewayDaily(ctx, day)
	}
	if err != nil {
		httpError(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"day": day, "rows": rows})
}

// routeReportsLegacy: old verb route during the deprecation window.
func (s *Server) routeReportsLegacy(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		s.handleReports(w, r)
		return
	}
	http.Redirect(w, r, "/v1/admin/reports", http.StatusTemporaryRedirect)
}
