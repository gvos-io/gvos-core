// Command gvos-core is the billing control plane: it exposes the
// event endpoints Kamailio calls (call start/answer/end) plus health.
// Without -dsn it runs fully in memory with demo seed data (dev/test);
// with -dsn it loads accounts/rates from PostgreSQL and persists CDRs,
// ledger and audit rows.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/gvos/gvos-core/internal/account"
	"github.com/gvos/gvos-core/internal/callstate"
	"github.com/gvos/gvos-core/internal/rate"
	"github.com/gvos/gvos-core/internal/store"
)

// Server bundles the billing control plane.
type Server struct {
	store    *account.Store
	rates    *rate.Table
	rateList []rate.Rate // memory-mode source of truth for the table
	calls    *callstate.Manager
	cdrs     []*callstate.CDR
	pg       *store.PG // nil in memory mode
	startAt  time.Time
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
	s := &Server{store: account.NewStore(), startAt: time.Now()}

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
	s := &Server{store: account.NewStore(), pg: pg, startAt: time.Now()}
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
	log.Printf("loaded %d accounts from PostgreSQL", n)
	s.calls = s.newCallManager()
	return s, nil
}
func (s *Server) newCallManager() *callstate.Manager {
	// Teardown in production sends CANCEL/BYE via Kamailio.
	return callstate.NewManager(s.store, func(callID, reason string) {
		log.Printf("TEARDOWN %s reason=%s", callID, reason)
	}, 10*time.Second)
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
	now := time.Now()
	entry := s.rates.Lookup(req.To, int(now.Weekday()), now.Hour()*60+now.Minute())
	if entry == nil {
		httpError(w, 404, "no rate for destination "+req.To)
		return
	}
	accID := req.From
	if accID == "" {
		accID = "1000" // spike default caller
	}
	if err := s.calls.Start(req.CallID, accID, req.To, entry, 60); err != nil {
		// Insufficient balance is a 403 for the caller.
		httpError(w, 403, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{
		"call_id":     req.CallID,
		"rate_prefix": entry.Prefix,
		"price":       entry.Price,
		"lock":        entry.Price,
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
		DurSecs int    `json:"duration"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.CallID == "" {
		httpError(w, 400, "call_id required")
		return
	}
	cdr, err := s.calls.End(req.CallID, req.DurSecs)
	if err != nil {
		httpError(w, 404, err.Error())
		return
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

	var s *Server
	if *dsn != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		var err error
		s, err = NewPGServer(ctx, *dsn)
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
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		fmt.Fprintln(w, "ok")
	})
	return mux
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
