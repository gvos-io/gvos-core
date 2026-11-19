// Package api is the interface layer: HTTP handlers for the admin,
// signaling-event and stats faces, plus the Server assembly that wires
// the domain packages together. cmd/gvos-core keeps only flags/env
// parsing and lifecycle (ADR-0005).
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/lixuanqun/gvos-core/internal/billingdomain/account"
	"github.com/lixuanqun/gvos-core/internal/billingdomain/pkgplan"
	"github.com/lixuanqun/gvos-core/internal/billingdomain/rate"
	"github.com/lixuanqun/gvos-core/internal/callcore"
	"github.com/lixuanqun/gvos-core/internal/customerdomain/blocklist"
	"github.com/lixuanqun/gvos-core/internal/customerdomain/gateway"
	"github.com/lixuanqun/gvos-core/internal/customerdomain/ipallow"
	"github.com/lixuanqun/gvos-core/internal/customerdomain/regtable"
	"github.com/lixuanqun/gvos-core/internal/customerdomain/riskctrl"
	"github.com/lixuanqun/gvos-core/internal/shared/acl"
	"github.com/lixuanqun/gvos-core/internal/mediamain/mediastream"
	"github.com/lixuanqun/gvos-core/internal/shared/events"
	"github.com/lixuanqun/gvos-core/internal/shared/rds"
	"github.com/lixuanqun/gvos-core/internal/shared/store"
)

type Server struct {
	store         *account.Store
	rates         *rate.Table
	rateList      []rate.Rate // memory-mode source of truth for the table
	calls         *callcore.Manager
	cdrs          []*callcore.CDR
	pg            *store.PG            // nil in memory mode
	userIndex     map[string]string    // SIP username -> account ID (PG mode)
	recordEnabled bool                 // global recording opt-in (admin toggle)
	blocklist     *blocklist.List      // number screening (black/white rules)
	ipallow       *ipallow.List        // source-IP admission (default-deny)
	risk          *riskctrl.Engine     // real-time fraud engine (freq/cost/prefix/reg_freq)
	media         *mediastream.Manager // AI media plane (per-call bidirectional streams)
	pkg           *pkgplan.Engine      // subscription packages (allowance/discount/min-spend)
	gws           *gateway.List        // outbound gateway directory (API-authoritative)
	regs          *regtable.Table      // SIP registration mirror (fed by gvos.reg.event)
	acl           *acl.Store           // API-key registry (Phase B; nil = keys off, legacy fallback)
	idem          *idemStore           // Idempotency-Key registry for money routes
	rds           *rds.Client          // Redis runtime-state authority (nil = off, memory authoritative)
	events        *events.Publisher    // NATS event bus (nil = publishing off)
	webhookURL    string               // webhook dispatcher target ("" = off)
	asyncCdr      bool                 // CDRW worker owns CDR persistence
	asyncAudit    bool                 // AUDIT worker owns the audit trail
	workers       []*events.Consumer   // running async workers (stopped at exit)
	workerNames   []string             // surfaced in /v1/stats
	startAt       time.Time
}

// emit is the single event-bus seam: nil-safe and best-effort. The
// billing answer never waits on the bus (publisher queues internally).
// Three subjects carry a dot-free edge alias (the Kamailio nats module's
// event_route lexer cannot express dots): cfg.reload, risk.alert and
// call.teardown are dual-published so core replicas subscribe to the
// canonical dotted name while the signaling edge subscribes to the
// alias. Payloads are identical — aliases are transport copies, not
// semantic forks.
func (s *Server) emit(subject string, payload any) {
	if s.events == nil {
		return
	}
	s.events.Publish(subject, payload)
	switch subject {
	case events.SubjectCfgReload:
		s.events.Publish(events.SubjectEdgeCfgReload, payload)
	case events.SubjectRiskAlert:
		s.events.Publish(events.SubjectEdgeRiskAlert, payload)
	case events.SubjectCallTeardown:
		s.events.Publish(events.SubjectEdgeTeardown, payload)
	}
}

// rebuildRates recomputes the lookup table. PG mode reloads from the
// database (authoritative); memory mode rebuilds from rateList.
func (s *Server) rebuildRates() {
	if s.pg != nil {
		ctx, cancel := contextWithTimeoutDuration(context.Background(), 5*time.Second)
		defer cancel()
		if tbl, err := s.pg.LoadRates(ctx, "demo"); err == nil {
			s.rates = tbl
			return
		}
		// DB reload failed: fall back to the in-memory list.
	}
	s.rates = rate.NewTable(s.rateList)
}

// NewServer builds the in-memory demo server (dev/test mode).
func NewServer() *Server {
	s := &Server{store: account.NewStore(), blocklist: blocklist.New(), ipallow: ipallow.New(), risk: riskctrl.New(), media: mediastream.New(), pkg: pkgplan.New(), gws: gateway.New(), regs: regtable.New(), acl: acl.NewStore(), idem: newIdemStore(), startAt: time.Now()}
	s.userIndex = map[string]string{}

	// Demo seed: one tenant chain and a small rate table. The tenant
	// root is postpaid (it invoices); agents and users are prepaid.
	s.store.Put(&account.Account{ID: "tenant", Name: "Demo Tenant", RateGroup: "demo", Postpaid: true})
	s.store.Put(&account.Account{ID: "agent1", ParentID: "tenant", Credit: 1000, RateGroup: "demo"})
	s.store.Put(&account.Account{ID: "1000", ParentID: "agent1", Name: "Demo SIP user", Credit: 10, RateGroup: "demo"})

	// Demo gateway set: mirrors migrations/004 seed so the signaling
	// edge has a routable target out of the box (NATS broadcast from
	// startAsyncWorkers ships it once the bus is up).
	s.gws.Reload([]gateway.Gateway{
		{ID: "demo-gw", Name: "demo-gw", SipAddr: "demo-gw:5060", Protocol: "udp", Priority: 5, Weight: 100, Enabled: true},
		{ID: "demo-gw-backup", Name: "demo-gw-backup", SipAddr: "demo-gw:5060", Protocol: "udp", Priority: 10, Weight: 100, Enabled: true},
	})

	s.rateList = demoRates()
	s.rates = rate.NewTable(s.rateList)
	s.calls = s.newCallManager()
	return s
}

// NewPGServer loads accounts and the rate table from PostgreSQL.
func NewPGServer(ctx context.Context, dsn string) (*Server, error) {
	pg, err := store.Open(ctx, dsn)
	if err != nil {
		return nil, err
	}
	s := &Server{store: account.NewStore(), pg: pg, blocklist: blocklist.New(), ipallow: ipallow.New(), risk: riskctrl.New(), media: mediastream.New(), pkg: pkgplan.New(), gws: gateway.New(), regs: regtable.New(), acl: acl.NewStore(), idem: newIdemStore(), startAt: time.Now()}
	n, err := pg.LoadAccounts(ctx, s.store)
	if err != nil {
		pg.Close()
		return nil, err
	}
	s.rates, err = pg.LoadRates(ctx, "demo")
	if err != nil {
		pg.Close()
		return nil, err
	}
	if n, err := pg.LoadBlocklist(ctx, s.blocklist); err != nil {
		log.Printf("blocklist load failed: %v", err)
	} else if n > 0 {
		log.Printf("loaded %d screening rules", n)
	}
	var ipRules []ipallow.Rule
	if err := pg.GetSetting(ctx, "ip_allowlist", &ipRules); err == nil {
		s.ipallow.Reload(ipRules)
		log.Printf("restored %d IP allowlist rules from settings (default-deny)", s.ipallow.Count())
	} else if !errors.Is(err, store.ErrSettingNotFound) {
		log.Printf("IP allowlist setting load failed: %v", err)
	}
	// Global runtime settings: admin toggles persisted across the bus
	// broadcasts (a restart or new replica resumes the recorded state).
	var recSetting struct {
		Enabled bool `json:"enabled"`
	}
	if err := pg.GetSetting(ctx, "recording", &recSetting); err == nil {
		s.recordEnabled = recSetting.Enabled
		log.Printf("recording toggle restored from settings (enabled=%v)", s.recordEnabled)
	} else if !errors.Is(err, store.ErrSettingNotFound) {
		log.Printf("recording setting load failed: %v", err)
	}
	var riskRules []riskctrl.Rule
	if err := pg.GetSetting(ctx, "risk_rules", &riskRules); err == nil {
		s.risk.SetRules(riskRules)
		log.Printf("loaded %d risk rules from settings", len(riskRules))
	} else if !errors.Is(err, store.ErrSettingNotFound) {
		log.Printf("risk rules setting load failed: %v", err)
	}
	var pkgSnap pkgplan.Snapshot
	if err := pg.GetSetting(ctx, "packages", &pkgSnap); err == nil {
		s.pkg.Restore(pkgSnap)
		log.Printf("restored %d package definitions (%d assignments) from settings",
			len(pkgSnap.Packages), len(pkgSnap.Assignments))
	} else if !errors.Is(err, store.ErrSettingNotFound) {
		log.Printf("packages setting load failed: %v", err)
	}
	// Gateway directory: settings first (API-authoritative once the
	// admin plane has written once), the legacy gateways table as the
	// one-time bootstrap fallback.
	var gwList []gateway.Gateway
	if err := pg.GetSetting(ctx, "gateways", &gwList); err == nil {
		if n, err2 := s.gws.Reload(gwList); err2 == nil {
			log.Printf("restored %d gateways from settings", n)
		}
	} else if !errors.Is(err, store.ErrSettingNotFound) {
		log.Printf("gateways setting load failed: %v", err)
	}
	if s.gws.Count() == 0 {
		if fallback, err := pg.LoadGateways(ctx); err == nil && len(fallback) > 0 {
			if n, _ := s.gws.Reload(fallback); n > 0 {
				log.Printf("bootstrapped %d gateways from the legacy table (first run; the settings key owns it from now on)", n)
			}
		} else if err != nil {
			log.Printf("gateway table fallback load failed: %v", err)
		}
	}
	s.userIndex = map[string]string{}
	for _, a := range mustList(s.store) {
		s.userIndex[a.Username] = a.ID
	}
	log.Printf("loaded %d accounts from PostgreSQL", n)
	s.startReportWorker()
	s.calls = s.newCallManager()
	return s, nil
}

func mustList(st *account.Store) []*account.Account {
	l, err := st.List()
	if err != nil {
		return nil
	}
	return l
}
func (s *Server) newCallManager() *callcore.Manager {
	// Teardown: the callback only marks and publishes — the TEARDOWN
	// durable worker (or the direct fallback below) owns dispatch, so
	// a down executor holds instructions in the stream instead of
	// losing them to a reply timeout.
	m := callcore.NewManager(s.store, func(callID, reason string) {
		log.Printf("TEARDOWN %s reason=%s", callID, reason)
		s.emit(events.SubjectCallTeardown, map[string]string{"call_id": callID, "reason": reason})
		if s.events == nil {
			// No bus: pre-NATS behavior, executor runs inline.
			dispatchTeardown(callID, reason)
		}
	}, 10*time.Second)
	// Zombie sweeper: calls whose BYE never arrives (SIPp timeout
	// kills, gateway drops) settle after 10 minutes instead of
	// leaking their pre-answer locks.
	m.StartSweeper(10*time.Minute, 30*time.Second)
	// Interim driver: the 10s deduction slices of the architecture doc.
	// In-process time slices until RTPengine activity events replace
	// them; teardown rides the gvos.call.teardown event queue.
	m.StartInterimDriver(0)
	return m
}

func demoRates() []rate.Rate {
	return []rate.Rate{
		{Prefix: "86", Price: 0.06, InitDur: 60, StepDur: 6},
		{Prefix: "8613", Price: 0.05, InitDur: 60, StepDur: 6},
		{Prefix: "1", Price: 0.03, InitDur: 60, StepDur: 6},
		{Prefix: "91", Price: 0.9, InitDur: 60, StepDur: 6},
	}
}

// handleStart is the pre-answer lock: Kamailio calls it on INVITE.
// Thin wrapper — the logic lives in callStart (shared with the NATS
// service endpoint).
// requireInternal enforces the X-GVOS-Internal token on the signaling
// event face (Phase A, fail-closed): no token configured means the
// call plane refuses new events rather than accepting unauthenticated
// billing traffic. Kamailio cfg sends the header on every call
// event once GVOS_INTERNAL_TOKEN is set.
func requireInternal(w http.ResponseWriter, r *http.Request) bool {
	want := os.Getenv("GVOS_INTERNAL_TOKEN")
	if want == "" {
		httpErrorC(w, 503, "INTERNAL_AUTH_UNSET", "signaling face disabled (GVOS_INTERNAL_TOKEN unset)")
		return false
	}
	if r.Header.Get("X-GVOS-Internal") != want {
		httpErrorC(w, 401, "UNAUTHORIZED", "bad internal token")
		return false
	}
	return true
}

func (s *Server) handleStart(w http.ResponseWriter, r *http.Request) {
	if !requireInternal(w, r) {
		return
	}
	if !requirePOST(w, r) {
		return
	}
	var req callStartReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.CallID == "" || req.To == "" {
		httpError(w, 400, "call_id and to_user required")
		return
	}
	code, body := s.callStart(req)
	if code == 429 {
		w.Header().Set("Retry-After", "1")
	}
	writeJSON(w, code, body)
}

// handleAnswer switches the call from lock to live.
func (s *Server) handleAnswer(w http.ResponseWriter, r *http.Request) {
	if !requireInternal(w, r) {
		return
	}
	if !requirePOST(w, r) {
		return
	}
	var req struct {
		CallID string `json:"call_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.CallID == "" {
		httpError(w, 400, "call_id required")
		return
	}
	code, body := s.callAnswer(req.CallID)
	writeJSON(w, code, body)
}

// handleEnd finalizes billing and returns the CDR.
func (s *Server) handleEnd(w http.ResponseWriter, r *http.Request) {
	if !requireInternal(w, r) {
		return
	}
	if !requirePOST(w, r) {
		return
	}
	var req callEndReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.CallID == "" {
		httpError(w, 400, "call_id required")
		return
	}
	code, body := s.callEnd(r.Context(), req)
	writeJSON(w, code, body)
}

// handleStats is a monitoring hook (Prometheus comes later). The same
// snapshot serves the gvos.svc.stats request/reply face (startStatsSvc).
func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, s.statsBody())
}

func (s *Server) statsBody() map[string]any {
	resp := map[string]any{
		"live_calls": s.calls.Live(),
		"cdrs":       len(s.cdrs),
		"uptime_s":   int(time.Since(s.startAt).Seconds()),
		"mode":       "memory",
	}
	if s.pg != nil {
		resp["mode"] = "postgres"
	}
	resp["nats"] = "off"
	if s.events != nil {
		resp["nats"] = s.events.Mode()
	}
	resp["webhook"] = "off"
	if s.events != nil && s.webhookURL != "" {
		resp["webhook"] = "on"
	}
	resp["workers"] = s.workerNames
	resp["redis"] = "off"
	if s.rds != nil {
		resp["redis"] = "on"
	}
	if bal, err := account.Balance(s.store, "1000"); err == nil {
		resp["caller_bal"] = bal
	}
	return resp
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// errorEnvelope is the Phase A error contract (api-conventions §3):
// {"error":{"code","message"}}. Codes are stable, machine-readable
// SCREAMING_SNAKE strings the callers branch on; message is the
// human detail.
type errorEnvelope struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// errCodeFor maps HTTP status codes to the stable error code set.
// 403 keeps three distinct causes (balance/blocklist/risk) which the
// specific callers raise via httpErrorC.
func errCodeFor(code int) string {
	switch code {
	case 400:
		return "BAD_REQUEST"
	case 401:
		return "UNAUTHORIZED"
	case 403:
		return "FORBIDDEN"
	case 404:
		return "NOT_FOUND"
	case 405:
		return "METHOD_NOT_ALLOWED"
	case 429:
		return "RATE_LIMITED"
	case 500:
		return "INTERNAL"
	case 503:
		return "UNAVAILABLE"
	}
	return "ERROR"
}

func httpError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]errorEnvelope{"error": {Code: errCodeFor(code), Message: msg}})
}

// httpErrorC raises an explicit error code that differs from the
// status-derived default (403 insufficient-balance vs blocklist vs
// risk-blocked).
func httpErrorC(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]errorEnvelope{"error": {Code: code, Message: msg}})
}

func MuxFor(s *Server) *http.ServeMux {
	mux := http.NewServeMux()

	// Signaling event plane (internal token, fail-closed — Phase A).
	mux.HandleFunc("POST /v1/call/start", s.handleStart)
	mux.HandleFunc("POST /v1/call/answer", s.handleAnswer)
	mux.HandleFunc("POST /v1/call/end", s.handleEnd)
	mux.HandleFunc("GET /v1/stats", s.handleStats)

	// Admin plane: resource routes (Go 1.22 method patterns). The
	// verb-dispatcher routes are gone; 405/404 come from net/http.
	mux.HandleFunc("POST /v1/admin/accounts", s.withIdempotency(s.handleAccountCreate))
	mux.HandleFunc("GET /v1/admin/accounts", s.handleAccountList)
	mux.HandleFunc("POST /v1/admin/accounts/{id}/topup", s.withIdempotency(s.handleTopUp))
	mux.HandleFunc("GET /v1/admin/rates", s.handleRateList)
	mux.HandleFunc("PUT /v1/admin/rates", s.handleRateUpsert)
	mux.HandleFunc("GET /v1/admin/risk-rules", s.handleRiskRules)
	mux.HandleFunc("PUT /v1/admin/risk-rules", s.handleRiskRules)
	mux.HandleFunc("GET /v1/admin/gateways", s.handleGatewayList)
	mux.HandleFunc("PUT /v1/admin/gateways", s.handleGatewaySet)
	mux.HandleFunc("DELETE /v1/admin/gateways/{id}", s.handleGatewayDelete)
	mux.HandleFunc("GET /v1/admin/registrations", s.handleRegList)
	mux.HandleFunc("GET /v1/admin/cdrs", s.handleCDRList)
	mux.HandleFunc("POST /v1/admin/recording", s.handleRecordToggle)
	mux.HandleFunc("GET /v1/admin/recording", s.handleRecordToggle)
	mux.HandleFunc("GET /v1/admin/blocklist-entries", s.handleBlocklistList)
	mux.HandleFunc("POST /v1/admin/blocklist-entries", s.handleBlocklist)
	mux.HandleFunc("DELETE /v1/admin/blocklist-entries/{id}", s.handleBlocklistDelete)
	mux.HandleFunc("GET /v1/admin/ipallow/rules", s.handleIPAllowList)
	mux.HandleFunc("POST /v1/admin/ipallow/rules", s.handleIPAllow)
	mux.HandleFunc("DELETE /v1/admin/ipallow/rules/{id}", s.handleIPAllowDelete)
	mux.HandleFunc("GET /v1/admin/packages", s.handlePackages)
	mux.HandleFunc("PUT /v1/admin/packages", s.handlePackages)
	mux.HandleFunc("GET /v1/admin/packages/usage", s.handlePackageUsage)
	mux.HandleFunc("POST /v1/admin/reports/rollup", s.handleReports)
	mux.HandleFunc("GET /v1/admin/reports", s.handleReportList)
	mux.HandleFunc("GET /v1/admin/reports/views", s.handleReportViews)
	// API-key administration (Phase B).
	mux.HandleFunc("POST /v1/admin/keys", s.handleKeyCreate)
	mux.HandleFunc("GET /v1/admin/keys", s.handleKeyList)
	mux.HandleFunc("DELETE /v1/admin/keys/{prefix}", s.handleKeyRevoke)

	// Test hook; x-internal, stays out of the public OpenAPI.
	mux.HandleFunc("POST /v1/admin/stream-push", s.handleStreamPush)

	// Internal control plane (SIP edge pollers).
	mux.HandleFunc("GET /v1/internal/ipallow-snapshot", s.handleIPAllowSnapshot)

	// Media stream (WebSocket).
	mux.HandleFunc("/v1/stream/", s.handleMediaStream)

	// Deprecated spike routes (Phase A migration window): 307 to the
	// resource form; removed next version. Deprecation header set.
	deprecated := func(target string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Deprecation", "true")
			w.Header().Set("Sunset", "0")
			http.Redirect(w, r, target, http.StatusTemporaryRedirect)
		}
	}
	mux.HandleFunc("/v1/admin/topup", deprecated("/v1/admin/accounts/{id}/topup"))
	mux.HandleFunc("/v1/admin/rates", s.routeRatesLegacy)
	mux.HandleFunc("/v1/admin/record", deprecated("/v1/admin/recording"))
	mux.HandleFunc("/v1/admin/blocklist", s.handleBlocklistLegacy)
	mux.HandleFunc("/v1/admin/risk", s.routeRiskLegacy)
	mux.HandleFunc("/v1/admin/ipallow", s.handleIPAllowLegacy)
	mux.HandleFunc("/v1/admin/gateways", s.routeGatewaysLegacy)
	mux.HandleFunc("/v1/admin/reports", s.routeReportsLegacy)

	// Static admin console (token entered in the UI).
	mux.HandleFunc("/ui", s.handleUI)
	mux.HandleFunc("/ui/", s.handleUI)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		fmt.Fprintln(w, "ok")
	})
	return mux
}

// GET /v1/admin/cdrs?limit=50 - latest CDRs (in-memory ring; PG mode
// reads would come from the DB, same shape).
// handleCDRList serves GET /v1/admin/cdrs with cursor pagination
// (api-conventions §4): newest first, cursor = index into the
// in-memory ring (PG mode later switches to keyset on
// start_time,call_id). Default limit 50, max 500.
func (s *Server) handleCDRList(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	limit := 50
	if l := r.URL.Query().Get("limit"); l != "" {
		if n, err := json.Number(l).Int64(); err == nil && n > 0 && n <= 500 {
			limit = int(n)
		}
	}
	// cursor = ring index one past the last returned row (0 = start
	// from newest). Page i returns [len-cursor-limit, len-cursor).
	cursor := 0
	if c := r.URL.Query().Get("cursor"); c != "" {
		if n, err := json.Number(c).Int64(); err == nil && n >= 0 && int(n) <= len(s.cdrs) {
			cursor = int(n)
		} else {
			httpErrorC(w, 400, "BAD_CURSOR", "cursor out of range")
			return
		}
	}
	out := []*callcore.CDR{}
	for i := len(s.cdrs) - 1 - cursor; i >= 0 && len(out) < limit; i-- {
		out = append(out, s.cdrs[i])
	}
	next := ""
	if cursor+len(out) < len(s.cdrs) {
		next = json.Number(fmt.Sprintf("%d", cursor+len(out))).String()
	}
	writeJSON(w, 200, map[string]any{"cdrs": out, "count": len(out), "next_cursor": next})
}

// handleUI serves the embedded single-file admin console.
func (s *Server) handleUI(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(adminHTML))
}

// routeAccounts dispatches by verb (Go 1.21: no method patterns).
func (s *Server) routeAccounts(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		s.handleAccountCreate(w, r)
	case http.MethodGet:
		s.handleAccountList(w, r)
	default:
		httpError(w, 405, "POST or GET")
	}
}

// routeRates dispatches by verb.
func (s *Server) routeRates(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		s.handleRateUpsert(w, r)
	case http.MethodGet:
		s.handleRateList(w, r)
	default:
		httpError(w, 405, "POST or GET")
	}
}

// routeGateways dispatches by verb.
func (s *Server) routeGateways(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		s.handleGatewaySet(w, r)
	case http.MethodGet:
		s.handleGatewayList(w, r)
	default:
		httpError(w, 405, "POST or GET")
	}
}

// requirePOST rejects non-POST calls (helper for the spike routes).
func requirePOST(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != http.MethodPost {
		httpError(w, http.StatusMethodNotAllowed, "POST required")
		return false
	}
	return true
}

func roundUp7f(p float64) float64 {
	if p <= 0 {
		return p
	}
	scaled := p * 1e7
	whole := float64(int64(scaled))
	if scaled-whole > 1e-9 {
		whole++
	}
	return whole / 1e7
}

// AttachBus wires the event publisher and webhook target onto the
// server (the cmd layer calls this instead of poking fields).
func AttachBus(s *Server, pub *events.Publisher, webhookURL string) {
	s.events = pub
	s.webhookURL = webhookURL
}

// WorkerNames lists the running async workers for the startup log.
func WorkerNames(s *Server) []string { return s.workerNames }

// Shutdown drains the async workers and closes the event bus.
func Shutdown(s *Server) {
	for _, c := range s.workers {
		c.Stop()
	}
	if s.events != nil {
		s.events.Close()
	}
}

// SetClock overrides the call manager's clock (test seam).
func SetClock(s *Server, now func() time.Time) { s.calls.SetClock(now) }

// routeRatesLegacy: old verb route (POST=upsert, GET=list) kept for
// the deprecation window.
func (s *Server) routeRatesLegacy(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		s.handleRateUpsert(w, r)
	case http.MethodGet:
		s.handleRateList(w, r)
	default:
		httpError(w, 405, "POST or GET")
	}
}

// routeRiskLegacy: old POST-only route; GET goes to risk-rules.
func (s *Server) routeRiskLegacy(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		s.handleRiskRules(w, r)
		return
	}
	http.Redirect(w, r, "/v1/admin/risk-rules", http.StatusTemporaryRedirect)
}

// AttachRedis wires the Redis runtime-state authority (Phase 0
// wiring; domain packages adopt it in Phases 1-5).
func AttachRedis(s *Server, c *rds.Client) { s.rds = c }
