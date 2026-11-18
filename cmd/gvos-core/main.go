// Command gvos-core is the billing control plane: it exposes the
// event endpoints Kamailio calls (call start/answer/end) plus health.
// Without -dsn it runs fully in memory with demo seed data (dev/test);
// with -dsn it loads accounts/rates from PostgreSQL and persists CDRs,
// ledger and audit rows.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/lixuanqun/gvos-core/internal/account"
	"github.com/lixuanqun/gvos-core/internal/blocklist"
	"github.com/lixuanqun/gvos-core/internal/callstate"
	"github.com/lixuanqun/gvos-core/internal/events"
	"github.com/lixuanqun/gvos-core/internal/mediastream"
	"github.com/lixuanqun/gvos-core/internal/pkgplan"
	"github.com/lixuanqun/gvos-core/internal/rate"
	"github.com/lixuanqun/gvos-core/internal/riskctrl"
	"github.com/lixuanqun/gvos-core/internal/store"
)

// Server bundles the billing control plane.
type Server struct {
	store         *account.Store
	rates         *rate.Table
	rateList      []rate.Rate // memory-mode source of truth for the table
	calls         *callstate.Manager
	cdrs          []*callstate.CDR
	pg            *store.PG            // nil in memory mode
	userIndex     map[string]string    // SIP username -> account ID (PG mode)
	recordEnabled bool                 // global recording opt-in (admin toggle)
	blocklist     *blocklist.List      // number screening (black/white rules)
	risk          *riskctrl.Engine     // real-time fraud engine (freq/cost/prefix)
	media         *mediastream.Manager // AI media plane (per-call bidirectional streams)
	pkg           *pkgplan.Engine      // subscription packages (allowance/discount/min-spend)
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
func (s *Server) emit(subject string, payload any) {
	if s.events != nil {
		s.events.Publish(subject, payload)
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
	s := &Server{store: account.NewStore(), blocklist: blocklist.New(), risk: riskctrl.New(), media: mediastream.New(), pkg: pkgplan.New(), startAt: time.Now()}
	s.userIndex = map[string]string{}

	// Demo seed: one tenant chain and a small rate table. The tenant
	// root is postpaid (it invoices); agents and users are prepaid.
	s.store.Put(&account.Account{ID: "tenant", Name: "Demo Tenant", RateGroup: "demo", Postpaid: true})
	s.store.Put(&account.Account{ID: "agent1", ParentID: "tenant", Credit: 1000, RateGroup: "demo"})
	s.store.Put(&account.Account{ID: "1000", ParentID: "agent1", Name: "Demo SIP user", Credit: 10, RateGroup: "demo"})

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
	s := &Server{store: account.NewStore(), pg: pg, blocklist: blocklist.New(), risk: riskctrl.New(), media: mediastream.New(), pkg: pkgplan.New(), startAt: time.Now()}
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
func (s *Server) newCallManager() *callstate.Manager {
	// Teardown: the callback only marks and publishes — the TEARDOWN
	// durable worker (or the direct fallback below) owns dispatch, so
	// a down executor holds instructions in the stream instead of
	// losing them to a reply timeout.
	m := callstate.NewManager(s.store, func(callID, reason string) {
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
func (s *Server) handleStart(w http.ResponseWriter, r *http.Request) {
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

func httpError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func main() {
	listen := flag.String("listen", ":8080", "HTTP listen address")
	dsn := flag.String("dsn", "", "PostgreSQL DSN; empty = in-memory demo mode")
	natsURL := flag.String("nats", "", "NATS server URL; empty = event bus off")
	webhook := flag.String("webhook", "", "Webhook endpoint URL (cdr.completed events; requires -nats)")
	tlsCert := flag.String("tls-cert", os.Getenv("GVOS_TLS_CERT"), "TLS cert (enables HTTPS)")
	tlsKey := flag.String("tls-key", os.Getenv("GVOS_TLS_KEY"), "TLS key")
	flag.Parse()

	// Compose/deploy environments pass the DSN via env; the flag wins
	// when both are set.
	dsnVal := *dsn
	if dsnVal == "" {
		dsnVal = os.Getenv("GVOS_DB_DSN")
	}
	natsVal := *natsURL
	if natsVal == "" {
		natsVal = os.Getenv("GVOS_NATS_URL")
	}
	webhookVal := *webhook
	if webhookVal == "" {
		webhookVal = os.Getenv("GVOS_WEBHOOK_URL")
	}

	var s *Server
	if dsnVal != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		var err error
		s, err = NewPGServer(ctx, dsnVal)
		cancel()
		if err != nil {
			log.Fatalf("postgres mode failed: %v", err)
		}
	} else {
		s = NewServer()
	}
	// Event bus is auxiliary, not authority: a missing/unreachable
	// broker must not take the control plane down (it reconnects on
	// its own; publishes just log until the broker appears).
	var webhookConsumer *events.Consumer
	if natsVal != "" {
		if pub, err := events.New(natsVal); err != nil {
			log.Printf("NATS event bus disabled (connect failed: %v)", err)
		} else {
			s.events = pub
			s.webhookURL = webhookVal
			startAsyncWorkers(s)
			startCfgReloadSubscriber(s)
			startCallSvc(s)
			startStatsSvc(s)
			if webhookVal != "" {
				webhookConsumer = startWebhookDispatcher(s)
			}
		}
	}
	log.Printf("gvos-core listening on %s (workers=%v)", *listen, s.workerNames)
	var runErr error
	if *tlsCert != "" && *tlsKey != "" {
		log.Printf("TLS enabled (%s)", *tlsCert)
		runErr = http.ListenAndServeTLS(*listen, *tlsCert, *tlsKey, muxFor(s))
	} else {
		runErr = http.ListenAndServe(*listen, muxFor(s))
	}
	if webhookConsumer != nil {
		webhookConsumer.Stop() // drain in-flight webhook deliveries
	}
	for _, c := range s.workers {
		c.Stop() // drain in-flight async writes before closing the bus
	}
	if s.events != nil {
		s.events.Close() // flush pending events before exit
	}
	if runErr != nil {
		log.Fatal(runErr)
	}
}

// muxFor builds the HTTP routing table. Path-only patterns (Go 1.21
// ServeMux has no method-aware patterns); handlers enforce verbs.
func muxFor(s *Server) *http.ServeMux {
	mux := http.NewServeMux()
	// Kamailio event plane.
	mux.HandleFunc("/v1/call/start", s.handleStart)
	mux.HandleFunc("/v1/call/answer", s.handleAnswer)
	mux.HandleFunc("/v1/call/end", s.handleEnd)
	mux.HandleFunc("/v1/stats", s.handleStats)
	// Admin plane (bearer token guarded).
	mux.HandleFunc("/v1/admin/accounts", s.routeAccounts)
	mux.HandleFunc("/v1/admin/topup", s.handleTopUp)
	mux.HandleFunc("/v1/admin/rates", s.routeRates)
	mux.HandleFunc("/v1/admin/cdrs", s.handleCDRList)
	mux.HandleFunc("/v1/admin/record", s.handleRecordToggle)
	mux.HandleFunc("/v1/admin/blocklist", s.handleBlocklist)
	mux.HandleFunc("/v1/admin/risk", s.handleRiskRules)
	mux.HandleFunc("/v1/admin/stream-push", s.handleStreamPush)
	mux.HandleFunc("/v1/admin/reports", s.handleReports)
	mux.HandleFunc("/v1/admin/packages", s.handlePackages)
	mux.HandleFunc("/v1/admin/packages/usage", s.handlePackageUsage)
	mux.HandleFunc("/v1/stream/", s.handleMediaStream)
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
	out := []*callstate.CDR{}
	for i := len(s.cdrs) - 1; i >= 0 && len(out) < limit; i-- {
		out = append(out, s.cdrs[i])
	}
	writeJSON(w, 200, map[string]any{"cdrs": out, "count": len(out)})
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
