// Command gvos-core is the billing control plane: it exposes the
// event endpoints Kamailio calls (call start/answer/end) plus health.
// This spike wires the in-memory account store, rate table and call
// state manager; the production build persists via PostgreSQL and
// serves the full REST API.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/gvos/gvos-core/internal/account"
	"github.com/gvos/gvos-core/internal/callstate"
	"github.com/gvos/gvos-core/internal/rate"
)

// Server bundles the spike control plane.
type Server struct {
	store   *account.Store
	rates   *rate.Table
	calls   *callstate.Manager
	cdrs    []*callstate.CDR
	startAt time.Time
}

func NewServer() *Server {
	s := &Server{store: account.NewStore(), startAt: time.Now()}

	// Spike seed: one tenant chain and a small rate table. The tenant
	// root is postpaid (it invoices); agents and users are prepaid.
	s.store.Put(&account.Account{ID: "tenant", Name: "Demo Tenant", RateGroup: "demo", Postpaid: true})
	s.store.Put(&account.Account{ID: "agent1", ParentID: "tenant", Credit: 1000, RateGroup: "demo"})
	s.store.Put(&account.Account{ID: "1000", ParentID: "agent1", Name: "Demo SIP user", Credit: 10, RateGroup: "demo"})

	s.rates = rate.NewTable([]rate.Rate{
		{Prefix: "86", Price: 0.06, InitDur: 60, StepDur: 6},
		{Prefix: "8613", Price: 0.05, InitDur: 60, StepDur: 6},
		{Prefix: "1", Price: 0.03, InitDur: 60, StepDur: 6},
		{Prefix: "91", Price: 0.9, InitDur: 60, StepDur: 6},
	})

	// Teardown in production sends CANCEL/BYE via Kamailio.
	s.calls = callstate.NewManager(s.store, func(callID, reason string) {
		log.Printf("TEARDOWN %s reason=%s", callID, reason)
	}, 10*time.Second)
	return s
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
	writeJSON(w, 200, cdr)
}

// handleStats is a monitoring hook (Prometheus comes later).
func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	bal, _ := account.Balance(s.store, "1000")
	writeJSON(w, 200, map[string]any{
		"live_calls": s.calls.Live(),
		"cdrs":       len(s.cdrs),
		"caller_bal": bal,
		"uptime_s":   int(time.Since(s.startAt).Seconds()),
	})
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
	flag.Parse()

	s := NewServer()
	log.Printf("gvos-core listening on %s", *listen)
	log.Fatal(http.ListenAndServe(*listen, muxFor(s)))
}

// muxFor builds the HTTP routing table. Path-only patterns (Go 1.21
// ServeMux has no method-aware patterns); handlers enforce verbs.
func muxFor(s *Server) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/call/start", s.handleStart)
	mux.HandleFunc("/v1/call/answer", s.handleAnswer)
	mux.HandleFunc("/v1/call/end", s.handleEnd)
	mux.HandleFunc("/v1/stats", s.handleStats)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		fmt.Fprintln(w, "ok")
	})
	return mux
}

// requirePOST rejects non-POST calls (helper for the spike routes).
func requirePOST(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != http.MethodPost {
		httpError(w, http.StatusMethodNotAllowed, "POST required")
		return false
	}
	return true
}
