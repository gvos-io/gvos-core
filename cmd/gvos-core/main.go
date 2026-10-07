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
	"strings"
	"time"

	"github.com/gvos/gvos-core/internal/account"
	"github.com/gvos/gvos-core/internal/blocklist"
	"github.com/gvos/gvos-core/internal/callstate"
	"github.com/gvos/gvos-core/internal/mediastream"
	"github.com/gvos/gvos-core/internal/pkgplan"
	"github.com/gvos/gvos-core/internal/riskctrl"
	"github.com/gvos/gvos-core/internal/rate"
	"github.com/gvos/gvos-core/internal/store"
)

// Server bundles the billing control plane.
type Server struct {
	store     *account.Store
	rates     *rate.Table
	rateList  []rate.Rate // memory-mode source of truth for the table
	calls     *callstate.Manager
	cdrs      []*callstate.CDR
	pg        *store.PG   // nil in memory mode
	userIndex map[string]string // SIP username -> account ID (PG mode)
	recordEnabled bool          // global recording opt-in (admin toggle)
	blocklist *blocklist.List  // number screening (black/white rules)
	risk      *riskctrl.Engine // real-time fraud engine (freq/cost/prefix)
	media     *mediastream.Manager // AI media plane (per-call bidirectional streams)
	pkg       *pkgplan.Engine      // subscription packages (allowance/discount/min-spend)
	startAt   time.Time
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
	// Teardown in production sends CANCEL/BYE via Kamailio.
	m := callstate.NewManager(s.store, func(callID, reason string) {
		log.Printf("TEARDOWN %s reason=%s", callID, reason)
	}, 10*time.Second)
	// Zombie sweeper: calls whose BYE never arrives (SIPp timeout
	// kills, gateway drops) settle after 10 minutes instead of
	// leaking their pre-answer locks.
	m.StartSweeper(10*time.Minute, 30*time.Second)
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
func (s *Server) handleStart(w http.ResponseWriter, r *http.Request) {
	if !requirePOST(w, r) {
		return
	}
	var req struct {
		CallID  string `json:"call_id"`
		From    string `json:"from_user"`
		To      string `json:"to_user"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.CallID == "" || req.To == "" {
		httpError(w, 400, "call_id and to_user required")
		return
	}
	// Number screening happens before rating: a blacklisted (or
	// non-whitelisted) destination never reaches billing.
	if ok, why := s.blocklist.CheckScreen(req.To, "dest"); !ok {
		httpError(w, 403, "destination blocked: "+why)
		return
	}
	// Caller resolution first: the risk engine and billing both need
	// the final account ID.
	accID := req.From
	if accID == "" {
		accID = "1000" // spike default caller
	}
	// Spike compatibility: unknown callers resolve by SIP username
	// (Kamailio sends $fU which is the auth username), then fall back
	// to the demo account. Production rejects with 401/404 after auth.
	if _, err := s.store.Get(accID); err != nil {
		if id, ok := s.userIndex[accID]; ok {
			accID = id
		} else if _, err2 := s.store.Get("1000"); err2 == nil {
			accID = "1000"
		}
	}
	// Fraud engine: frequency/cost/prefix rules, block or alert.
	// Estimated cost for the cost-cap check uses the 60s lock basis.
	if h := s.risk.CheckCall(accID, req.To, 0.05); h.Blocked {
		httpError(w, 403, "risk-blocked ["+h.RuleID+"]: "+h.Reason)
		return
	} else if h.Alerted {
		log.Printf("RISK-ALERT account=%s rule=%s: %s", accID, h.RuleID, h.Reason)
	}

	now := time.Now()
	entry := s.rates.Lookup(req.To, int(now.Weekday()), now.Hour()*60+now.Minute())
	if entry == nil {
		httpError(w, 404, "no rate for destination "+req.To)
		return
	}
	if err := s.calls.Start(req.CallID, accID, req.To, entry, 60, s.recordEnabled); err != nil {
		// Insufficient balance is a 403 for the caller; account limits
		// are a 429 (retry later); unknown account a 404.
		var limit *callstate.ErrLimit
		if errors.As(err, &limit) {
			w.Header().Set("Retry-After", "1")
			httpError(w, 429, err.Error())
			return
		}
		if strings.Contains(err.Error(), "not found") {
			httpError(w, 404, err.Error())
			return
		}
		httpError(w, 403, err.Error())
		return
	}
	// Media plane: open the bidirectional stream so AI subscribers
	// can attach even before the first RTP chunk.
	s.media.Open(req.CallID)

	writeJSON(w, 200, map[string]any{
		"call_id":     req.CallID,
		"rate_prefix": entry.Prefix,
		"price":       entry.Price,
		"lock":        entry.Price,
		// Recording opt-in: Kamailio calls start_recording() when true.
		"record": s.recordEnabled,
	})
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
	// Interim starts on the first tick; the manager has no explicit
	// answer event in the spike, so acknowledge.
	writeJSON(w, 200, map[string]string{"call_id": req.CallID, "status": "live"})
}

// handleEnd finalizes billing and returns the CDR.
func (s *Server) handleEnd(w http.ResponseWriter, r *http.Request) {
	if !requirePOST(w, r) {
		return
	}
	var req struct {
		CallID  string `json:"call_id"`
		DurSecs int    `json:"duration"` // optional: defaulted from call start
		PDDMs   int    `json:"pdd_ms"`  // post-dial delay measured by Kamailio
		SIPCode int    `json:"sip_code"`
		Release string `json:"release"` // normal | balance-exhausted | ...
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.CallID == "" {
		httpError(w, 400, "call_id required")
		return
	}
	seconds := req.DurSecs
	if seconds == 0 {
		// Kamailio sends the BYE without duration: derive it from the
		// call's start time (SIPp's failed call lasted ~0s, real calls
		// bill their true length).
		if info, ok := s.calls.Info(req.CallID); ok {
			seconds = int(time.Since(info).Seconds())
		}
	}
	cdr, err := s.calls.End(req.CallID, seconds)
	if err != nil {
		httpError(w, 404, err.Error())
		return
	}
	// Subscription package: free allowance first, then discount on the
	// billable remainder. The CDR carries the effective cost; free
	// seconds are recorded in the record for the bill view.
	if pr := s.pkg.Apply(cdr.AccountID, cdr.BilledSecs, cdr.Cost); pr.BillableSecs < float64(cdr.BilledSecs) || pr.Discount != 1 {
		if float64(cdr.BilledSecs) > 0 {
			ratio := pr.BillableSecs / float64(cdr.BilledSecs)
			cdr.Cost = roundUp7f(cdr.Cost * ratio * pr.Discount)
			cdr.FreeSecs = int(pr.FreeSecs)
		}
	}

	// Fraud engine settlement: reconcile the daily cost accumulation
	// with the actual CDR cost (estimate was 0.05 basis at start).
	s.risk.SettleCost(cdr.AccountID, 0.05, cdr.Cost)
	// Media plane: close the call's stream (subscribers drop, final
	// stats reported for the RTT budget audit).
	if st, ok := s.media.Close(req.CallID); ok && st.UplinkChunks+st.DownlinkChunks > 0 {
		log.Printf("MEDIA call=%s up=%d down=%d maxRTT=%dms", req.CallID,
			st.UplinkChunks, st.DownlinkChunks, st.MaxRTTMs)
	}
	if req.PDDMs > 0 {
		cdr.PDDMs = req.PDDMs
	}
	if req.SIPCode > 0 {
		cdr.SIPCode = req.SIPCode
	}
	if req.Release != "" {
		cdr.Release = req.Release
	}
	s.cdrs = append(s.cdrs, cdr)
	if s.pg != nil {
		// Persistence failures log but never block the billing answer:
		// the CDR is already returned and held in memory for retry.
		if err := s.pg.InsertCDR(r.Context(), cdr); err != nil {
			log.Printf("CDR persist failed (call %s): %v", cdr.CallID, err)
		}
		if bal, err := account.Balance(s.store, cdr.AccountID); err == nil {
			if err := s.pg.AppendLedger(r.Context(), cdr.AccountID, -cdr.Cost, bal, "call-settle", cdr.CallID); err != nil {
				log.Printf("ledger persist failed (call %s): %v", cdr.CallID, err)
			}
		}
	}
	writeJSON(w, 200, cdr)
}

// handleStats is a monitoring hook (Prometheus comes later).
func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	resp := map[string]any{
		"live_calls": s.calls.Live(),
		"cdrs":       len(s.cdrs),
		"uptime_s":   int(time.Since(s.startAt).Seconds()),
		"mode":       "memory",
	}
	if s.pg != nil {
		resp["mode"] = "postgres"
	}
	if bal, err := account.Balance(s.store, "1000"); err == nil {
		resp["caller_bal"] = bal
	}
	writeJSON(w, 200, resp)
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
	flag.Parse()

	// Compose/deploy environments pass the DSN via env; the flag wins
	// when both are set.
	dsnVal := *dsn
	if dsnVal == "" {
		dsnVal = os.Getenv("GVOS_DB_DSN")
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
	log.Printf("gvos-core listening on %s", *listen)
	log.Fatal(http.ListenAndServe(*listen, muxFor(s)))
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
