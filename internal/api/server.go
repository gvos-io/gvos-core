// Package api is the interface layer: HTTP handlers for the admin,
// signaling-event and stats faces, plus the Server assembly that wires
// the domain packages together. cmd/gvos-core keeps only flags/env
// parsing and lifecycle (ADR-0005).
package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gvos-io/gvos-core/internal/billingdomain/account"
	"github.com/gvos-io/gvos-core/internal/billingdomain/money"
	"github.com/gvos-io/gvos-core/internal/billingdomain/pkgplan"
	"github.com/gvos-io/gvos-core/internal/billingdomain/rate"
	"github.com/gvos-io/gvos-core/internal/callcore"
	"github.com/gvos-io/gvos-core/internal/customerdomain/axb"
	"github.com/gvos-io/gvos-core/internal/customerdomain/blocklist"
	"github.com/gvos-io/gvos-core/internal/customerdomain/did"
	"github.com/gvos-io/gvos-core/internal/customerdomain/gateway"
	"github.com/gvos-io/gvos-core/internal/customerdomain/ipallow"
	"github.com/gvos-io/gvos-core/internal/customerdomain/regtable"
	"github.com/gvos-io/gvos-core/internal/customerdomain/riskctrl"
	"github.com/gvos-io/gvos-core/internal/mediamain/mediastream"
	"github.com/gvos-io/gvos-core/internal/recpipe"
	"github.com/gvos-io/gvos-core/internal/shared/acl"
	"github.com/gvos-io/gvos-core/internal/shared/events"
	"github.com/gvos-io/gvos-core/internal/shared/rds"
	"github.com/gvos-io/gvos-core/internal/shared/store"
	"github.com/gvos-io/gvos-core/internal/webhook"
)

type Server struct {
	store         *account.Store
	rates         *rate.Table
	rateList      []rate.Rate  // memory-mode source of truth for the table
	cfgMu         sync.RWMutex // guards rates, rateList, userIndex
	calls         *callcore.Manager
	cdrs          []*callcore.CDR
	cdrsMu        sync.Mutex           // guards cdrs (NATS responders append concurrently with HTTP reads)
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
	axb           *axb.Store           // privacy-number bindings (AXB module)
	dids          *did.Store           // inbound-DID registry (callee-billed, migration 013)
	acl           *acl.Store           // API-key registry (Phase B; nil = keys off, legacy fallback)
	idem          *idemStore           // Idempotency-Key registry for money routes
	rds           *rds.Client          // Redis runtime-state authority (nil = off, memory authoritative)
	recpipe       *recpipe.Uploader    // recording pipeline (nil = off)
	recStore      recpipe.Storage      // archive backend (LocalFS or MinIO/S3)
	whDispatch    *webhook.Dispatcher  // outbound webhook fan-out (nil = off)
	whStore       WebhookStore         // subscription storage (nil = memory-local)
	redisLocks    bool                 // single-runner locks active (rollup/rebroadcast dedup)
	bal           *account.BalanceR    // redis balance face (nil = in-memory authority)
	events        *events.Publisher    // NATS event bus (nil = publishing off)
	webhookURL    string               // webhook dispatcher target ("" = off)
	asyncCdr      bool                 // CDRW worker owns CDR persistence
	asyncAudit    bool                 // AUDIT worker owns the audit trail
	workers       []*events.Consumer   // running async workers (stopped at exit)
	workerNames   []string             // surfaced in /v1/stats
	startAt       time.Time

	// Tenant CDR stream taps (Phase D): per-connection subscribers of
	// /v1/tenant/cdr-stream, fed from storeCdr. Non-blocking drops.
	cdrTapsMu sync.Mutex
	cdrTaps   []*cdrTap
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
	if s.whDispatch != nil && subject != events.SubjectCDRCompleted {
		// Phase D fan-out: customer endpoints subscribed to this
		// subject receive the same payload (signed; retries in the
		// dispatcher). CDR completed is owned solely by tapAndPublish
		// (contract envelope) — publishing here too double-delivers
		// and double-increments seq.
		s.whDispatch.PublishScoped(subject, s.eventOwners(subject, payload), payload)
	}
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

// callOwners lists the tenant identities whose scope holds an account:
// the account itself and every ancestor up to its root. A tenant/agent
// key is bound to one of them (see tenantAccounts), so a webhook
// subscription owned by that key sees exactly the calls under it.
func (s *Server) callOwners(accountID string) []string {
	chain, err := s.store.Chain(accountID)
	if err != nil {
		return []string{accountID}
	}
	owners := make([]string, 0, len(chain))
	for _, a := range chain {
		owners = append(owners, a.ID)
	}
	return owners
}

// eventOwners names the tenant identities an event belongs to, for
// webhook scoping. An event whose account cannot be named from its
// payload belongs to nobody: platform subscriptions alone see it.
func (s *Server) eventOwners(subject string, payload any) []string {
	m, _ := payload.(map[string]string)
	accountID := m["account"]
	if accountID == "" {
		accountID = m["account_id"]
	}
	if accountID == "" && m["call_id"] != "" {
		if acc, ok := s.calls.AccountOf(m["call_id"]); ok {
			accountID = acc
		}
	}
	if accountID == "" {
		return []string{}
	}
	return s.callOwners(accountID)
}

// partitionMonthsAhead is how many future monthly CDR partitions the
// boot / hourly ensure step keeps ready.
const partitionMonthsAhead = 3

// lcrRater adapts the per-group rate tables to the gateway.Rater
// face (LCR). Tables load lazily per CostRate group and cache.
type lcrRater struct {
	s     *Server
	cache map[string]*rate.Table
}

func (r *lcrRater) BestRate(group, dest string) *money.Amount {
	tbl, ok := r.cache[group]
	if !ok {
		if r.s.pg != nil {
			ctx, cancel := contextWithTimeoutDuration(context.Background(), 5*time.Second)
			defer cancel()
			if t, err := r.s.pg.LoadRates(ctx, group); err == nil {
				tbl = t
			}
		} else {
			// Memory mode: filter rateList by group membership (the
			// demo list is single-group; rateList entries carry no
			// group so all serve the demo group).
			r.s.cfgMu.RLock()
			list := append([]rate.Rate(nil), r.s.rateList...)
			r.s.cfgMu.RUnlock()
			tbl = rate.NewTable(list)
		}
		r.cache[group] = tbl
	}
	if tbl == nil {
		return nil
	}
	now := time.Now()
	if e := tbl.Lookup(dest, int(now.Weekday()), now.Hour()*60+now.Minute()); e != nil {
		return &e.Price
	}
	return nil
}

// rebuildRates recomputes the lookup table. PG mode reloads from the
// database (authoritative); memory mode rebuilds from rateList.
func (s *Server) rebuildRates() {
	var tbl *rate.Table
	if s.pg != nil {
		ctx, cancel := contextWithTimeoutDuration(context.Background(), 5*time.Second)
		defer cancel()
		if loaded, err := s.pg.LoadRates(ctx, "demo"); err == nil {
			tbl = loaded
		}
	}
	s.cfgMu.Lock()
	defer s.cfgMu.Unlock()
	if tbl != nil {
		s.rates = tbl
		return
	}
	// DB reload failed (or memory mode): rebuild from the in-memory list.
	s.rates = rate.NewTable(s.rateList)
}

// NewServer builds the in-memory demo server (dev/test mode).
func NewServer() *Server {
	s := &Server{store: account.NewStore(), blocklist: blocklist.New(), ipallow: ipallow.New(), risk: riskctrl.New(), media: mediastream.New(), pkg: pkgplan.New(), gws: gateway.New(), axb: axb.NewStore(), dids: did.NewStore(), regs: regtable.New(), acl: acl.NewStore(), idem: newIdemStore(), startAt: time.Now()}
	s.userIndex = map[string]string{}

	// Demo seed: one tenant chain and a small rate table. The tenant
	// root is postpaid (it invoices); agents and users are prepaid.
	s.store.Put(&account.Account{ID: "tenant", Name: "Demo Tenant", RateGroup: "demo", Postpaid: true})
	s.store.Put(&account.Account{ID: "agent1", ParentID: "tenant", Credit: money.MustFromYuan(1000), RateGroup: "demo"})
	s.store.Put(&account.Account{ID: "1000", ParentID: "agent1", Name: "Demo SIP user", Credit: money.MustFromYuan(10), RateGroup: "demo"})

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
	s := &Server{store: account.NewStore(), pg: pg, blocklist: blocklist.New(), ipallow: ipallow.New(), risk: riskctrl.New(), media: mediastream.New(), pkg: pkgplan.New(), gws: gateway.New(), axb: axb.NewStore(), dids: did.NewStore(), regs: regtable.New(), acl: acl.NewStore(), idem: newIdemStore(), startAt: time.Now()}
	// CDR partitions: the schema ships a fixed window only, and the
	// first month without one fails every CDR insert. Make sure this
	// month and the next few exist before any CDR needs one (the
	// hourly report pass repeats it).
	if err := pg.EnsureCDRPartitions(ctx, partitionMonthsAhead); err != nil {
		log.Printf("CDR partition ensure failed: %v", err)
	}
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
	// Inbound DID registry (callee-billed termination).
	var didList []did.DID
	if err := pg.GetSetting(ctx, "dids", &didList); err == nil {
		s.dids.Reload(didList)
		log.Printf("restored %d inbound DIDs from settings", s.dids.Count())
	} else if !errors.Is(err, store.ErrSettingNotFound) {
		log.Printf("dids setting load failed: %v", err)
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
		if reason == "balance-exhausted" {
			// Contract event (Phase D): the billing alarm a customer's
			// finance desk reacts to — balance drained mid-call.
			s.emit(events.SubjectAccountLowBalance, map[string]string{"call_id": callID, "reason": reason})
		}
		if s.events == nil {
			// No bus: pre-NATS behavior, executor runs inline.
			dispatchTeardown(callID, reason)
		}
	}, 10*time.Second)
	// Never-answered calls give up their pre-answer lock after the ring
	// timeout (it must exceed the edge's worst-case INVITE lifetime).
	ring := callcore.DefaultRingTimeout
	if v := os.Getenv("GVOS_RING_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			ring = d
		} else {
			log.Printf("GVOS_RING_TIMEOUT=%q invalid, using %s", v, ring)
		}
	}
	m.SetRingTimeout(ring)
	// Zombie sweeper: calls with no Interim activity (LastActive)
	// beyond the timeout — BYE never arrived — settle and teardown
	// instead of leaking pre-answer locks. Long live calls that keep
	// ticking Interim are NOT swept. Timeout is env-tunable; the
	// 10-minute default is the production stance. A bench or edge
	// with BYE-loss under load collapses live_calls fast — every
	// zombie rides the 10s Interim/Redis loop, and thousands of them
	// starve the RPC responders (measured: live_calls 21829 ->
	// SVC-CALL-START throughput to zero).
	if t := os.Getenv("GVOS_ZOMBIE_TIMEOUT"); t != "" {
		if d, err := time.ParseDuration(t); err == nil {
			m.StartSweeper(d, 30*time.Second)
			log.Printf("zombie sweeper: timeout=%s (env)", d)
		} else {
			m.StartSweeper(10*time.Minute, 30*time.Second)
		}
	} else {
		m.StartSweeper(10*time.Minute, 30*time.Second)
	}
	// Interim driver: the 10s deduction slices of the architecture doc.
	// In-process time slices until RTPengine activity events replace
	// them; teardown rides the gvos.call.teardown event queue.
	m.StartInterimDriver(0)
	return m
}

func demoRates() []rate.Rate {
	return []rate.Rate{
		{Prefix: "86", Price: money.MustFromYuan(0.06), InitDur: 60, StepDur: 6},
		{Prefix: "8613", Price: money.MustFromYuan(0.05), InitDur: 60, StepDur: 6},
		{Prefix: "1", Price: money.MustFromYuan(0.03), InitDur: 60, StepDur: 6},
		{Prefix: "91", Price: money.MustFromYuan(0.9), InitDur: 60, StepDur: 6},
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
	if os.Getenv("GVOS_INTERNAL_TOKEN") == "" {
		httpErrorC(w, 503, "INTERNAL_AUTH_UNSET", "signaling face disabled (GVOS_INTERNAL_TOKEN unset)")
		return false
	}
	if internalTokenOK(r) {
		return true
	}
	// Never log presented tokens — they are credentials and may land
	// in access/log aggregators.
	log.Printf("AUTH-DEBUG %s %s: bad or missing internal token", r.Method, r.URL.Path)
	httpErrorC(w, 401, "UNAUTHORIZED", "bad internal token")
	return false
}

// internalTokenOK reports whether the request carries the configured
// internal token. Accepted forms: the X-GVOS-Internal header
// (standard) or ?token= (the Kamailio http_client module cannot
// attach headers to POST bodies; the internal face is
// network-isolated, a known tradeoff). False when no token is
// configured. The comparison is constant-time: this token guards the
// signaling billing face, the media stream and the edge pollers.
func internalTokenOK(r *http.Request) bool {
	want := os.Getenv("GVOS_INTERNAL_TOKEN")
	if want == "" {
		return false
	}
	viaHeader := subtle.ConstantTimeCompare([]byte(r.Header.Get("X-GVOS-Internal")), []byte(want)) == 1
	viaQuery := subtle.ConstantTimeCompare([]byte(r.URL.Query().Get("token")), []byte(want)) == 1
	if viaHeader || viaQuery {
		// Dev-credential guard: the compose default must be visible in
		// monitoring, not just in the docs (it authenticates the
		// billing signaling face when someone forgets the swap).
		if want == "dev-internal-token" {
			mDevCreds.Inc(1)
		}
		return true
	}
	return false
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
	s.cdrsMu.Lock()
	cdrCount := len(s.cdrs)
	s.cdrsMu.Unlock()
	resp := map[string]any{
		"live_calls": s.calls.Live(),
		"cdrs":       cdrCount,
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
	writeJSON(w, code, map[string]errorEnvelope{"error": {Code: errCodeFor(code), Message: scrubInternal(msg)}})
}

// scrubInternal strips backend error internals (SQLSTATE, constraint
// names, file paths) from operator-facing messages — pentest finding:
// raw DB errors leaked dialect and schema details. The full error
// still goes to the server log.
func scrubInternal(msg string) string {
	if i := strings.Index(msg, "SQLSTATE"); i > 0 {
		// keep the action prefix (e.g. "create account:"), drop the rest
		if j := strings.LastIndex(msg[:i], ":"); j > 0 {
			return msg[:j] + ": database constraint violated"
		}
		return "database error"
	}
	if strings.Contains(msg, "duplicate key") {
		if j := strings.Index(msg, ":"); j > 0 {
			return msg[:j] + ": duplicate entry"
		}
	}
	return msg
}

// httpErrorC raises an explicit error code that differs from the
// status-derived default (403 insufficient-balance vs blocklist vs
// risk-blocked).
func httpErrorC(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]errorEnvelope{"error": {Code: code, Message: msg}})
}

func MuxFor(s *Server) http.Handler {
	mux := http.NewServeMux()
	// Per-request logging: on only when GVOS_LOG_HTTP=1 (300 CPS x 2
	// requests is 600 log lines/s otherwise — a hot-path IO stall).
	logWrap := func(next http.Handler) http.Handler {
		if os.Getenv("GVOS_LOG_HTTP") != "1" {
			return next
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			log.Printf("HTTP-IN %s %s len=%d", r.Method, r.URL.RequestURI(), r.ContentLength)
			next.ServeHTTP(w, r)
		})
	}

	// Operator-facing planes (admin/tenant) share a per-caller token
	// bucket (ratelimit.go); the signaling/internal planes below stay
	// unlimited — their natural rate IS the CPS admission gate, and a
	// second limiter would shadow the billing 429s.
	limited := s.withRateLimit(mux)

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
	// Dynamic blocklist feeds (source statistics).
	mux.HandleFunc("GET /v1/admin/blocklist-sources", s.handleBlocklistSources)

	// AXB privacy numbers (VOS module equivalent).
	mux.HandleFunc("POST /v1/admin/axb/bindings", s.handleAXBBind)
	mux.HandleFunc("GET /v1/admin/axb/bindings", s.handleAXBList)
	mux.HandleFunc("DELETE /v1/admin/axb/bindings/{x}", s.handleAXBDel)
	mux.HandleFunc("GET /v1/admin/axb/stats", s.handleAXBStats)

	// Inbound DID registry (callee-billed termination, migration 013).
	mux.HandleFunc("POST /v1/admin/dids", s.handleDIDPut)
	mux.HandleFunc("GET /v1/admin/dids", s.handleDIDList)
	mux.HandleFunc("DELETE /v1/admin/dids/{number}", s.handleDIDDelete)

	// Outbound webhooks (Phase D).
	mux.HandleFunc("POST /v1/admin/webhooks", s.handleWebhookCreate)
	mux.HandleFunc("GET /v1/admin/webhooks", s.handleWebhookList)
	mux.HandleFunc("DELETE /v1/admin/webhooks/{id}", s.handleWebhookDelete)

	// Tenant self-service plane (Phase C): read-only, scoped to the
	// API key's tenant. Roles tenant|agent only.
	mux.HandleFunc("GET /v1/tenant/me", s.handleTenantMe)
	mux.HandleFunc("GET /v1/tenant/accounts", s.handleTenantAccounts)
	mux.HandleFunc("GET /v1/tenant/cdrs", s.handleTenantCDRs)
	mux.HandleFunc("GET /v1/tenant/stats", s.handleTenantStats)
	mux.HandleFunc("GET /v1/tenant/packages/usage", s.handleTenantPackageUsage)
	mux.HandleFunc("POST /v1/tenant/webhooks", s.handleTenantWebhookCreate)
	mux.HandleFunc("GET /v1/tenant/webhooks", s.handleTenantWebhookList)
	mux.HandleFunc("GET /v1/tenant/cdr-stream", s.handleTenantCDRStream)

	// Recording pipeline (R2/R3).
	// Recording pipeline (R2/R3).
	mux.HandleFunc("GET /v1/recording/{call_id}", s.handleRecordingList)
	mux.HandleFunc("GET /v1/recording/{call_id}/{track}", s.handleRecordingPlay)

	// OpenAPI 3.1 spec (public contract descriptor).
	mux.HandleFunc("GET /openapi.json", s.handleOpenAPI)

	// API-key administration (Phase B).
	mux.HandleFunc("POST /v1/admin/keys", s.handleKeyCreate)
	mux.HandleFunc("GET /v1/admin/keys", s.handleKeyList)
	mux.HandleFunc("DELETE /v1/admin/keys/{prefix}", s.handleKeyRevoke)

	// Test hook; x-internal, stays out of the public OpenAPI.
	mux.HandleFunc("POST /v1/admin/stream-push", s.handleStreamPush)

	// Internal control plane (SIP edge pollers + ADR-0002 fallback).
	mux.HandleFunc("GET /v1/internal/ipallow-snapshot", s.handleIPAllowSnapshot)
	mux.HandleFunc("GET /v1/internal/gateway-snapshot", s.handleGatewaySnapshot)
	mux.HandleFunc("GET /v1/internal/did-snapshot", s.handleDIDSnapshot)
	mux.HandleFunc("GET /v1/internal/reg-lookup", s.handleRegLookup)

	// Media stream (WebSocket).
	mux.HandleFunc("/v1/stream/", s.handleMediaStream)

	// Static admin console (token entered in the UI).
	mux.HandleFunc("/ui", s.handleUI)
	mux.HandleFunc("/ui/", s.handleUI)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		fmt.Fprintln(w, "ok")
	})

	// Prometheus scrape (unauthenticated: counters only, no secrets —
	// gate at the ingress if 8080 is ever exposed publicly).
	mux.HandleFunc("/metrics", s.handleMetrics)
	return logWrap(limited)
}

// MuxForTest exposes MuxFor to tests in the same package (the real
// builder is unexported to keep external wiring going through
// cmd/gvos-core's Attach* path).
func (s *Server) MuxForTest() http.Handler { return MuxFor(s) }

// GET /v1/admin/cdrs?limit=50 - latest CDRs (in-memory ring; PG mode
// reads would come from the DB, same shape).
// handleCDRList serves GET /v1/admin/cdrs with cursor pagination
// (api-conventions §4): newest first, cursor = index into the
// in-memory ring (PG mode later switches to keyset on
// start_time,call_id). Default limit 50, max 500.
//
// The ring is capped (see maxInMemoryCDRs); when the head is trimmed a
// cursor issued before the trim slides forward one row per dropped
// entry — pagination through a live, bounded ring is best-effort by
// design. PG is the history authority; this ring is the recent view.
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
	s.cdrsMu.Lock()
	n := len(s.cdrs)
	// cursor = ring index one past the last returned row (0 = start
	// from newest). Page i returns [len-cursor-limit, len-cursor).
	cursor := 0
	if c := r.URL.Query().Get("cursor"); c != "" {
		if v, err := json.Number(c).Int64(); err == nil && v >= 0 && int(v) <= n {
			cursor = int(v)
		} else {
			s.cdrsMu.Unlock()
			httpErrorC(w, 400, "BAD_CURSOR", "cursor out of range")
			return
		}
	}
	out := make([]*callcore.CDR, 0, limit)
	for i := n - 1 - cursor; i >= 0 && len(out) < limit; i-- {
		out = append(out, s.cdrs[i])
	}
	next := ""
	if cursor+len(out) < n {
		next = json.Number(fmt.Sprintf("%d", cursor+len(out))).String()
	}
	s.cdrsMu.Unlock()
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

// AttachRecording wires the recording pipeline: output dir scanner +
// archive backend + PG index. Empty output = pipeline off.
// Backend selection: when GVOS_RECORDING_S3_ENDPOINT (+ AK/SK/BUCKET)
// is set, use MinIO/S3 (OSS-compatible) with presigned play URLs;
// otherwise LocalFS under archiveDir (GVOS_RECORDING_ARCHIVE).
func AttachRecording(s *Server, outputDir, archiveDir string) *recpipe.Uploader {
	if outputDir == "" {
		return nil
	}
	var idx recpipe.PGERecordIndex
	if s.pg != nil {
		idx = s.pg
	}
	store, err := recordingStorageFromEnv(archiveDir)
	if err != nil {
		log.Printf("recording storage: %v (pipeline off)", err)
		return nil
	}
	s.recStore = store
	u := recpipe.NewUploader(outputDir, store, idx, func(subject string, payload any) {
		s.emit(subject, payload)
	})
	s.recpipe = u
	return u
}

func recordingStorageFromEnv(archiveDir string) (recpipe.Storage, error) {
	ep := os.Getenv("GVOS_RECORDING_S3_ENDPOINT")
	if ep == "" {
		if archiveDir == "" {
			archiveDir = "/var/lib/gvos/recordings"
		}
		_ = os.MkdirAll(archiveDir, 0o755)
		return &recpipe.LocalFS{Root: archiveDir}, nil
	}
	ak := os.Getenv("GVOS_RECORDING_S3_AK")
	sk := os.Getenv("GVOS_RECORDING_S3_SK")
	bucket := os.Getenv("GVOS_RECORDING_S3_BUCKET")
	if bucket == "" {
		bucket = "gvos-recordings"
	}
	secure := os.Getenv("GVOS_RECORDING_S3_SECURE") != "0" && os.Getenv("GVOS_RECORDING_S3_SECURE") != "false"
	return recpipe.NewMinIO(ep, ak, sk, bucket, secure)
}

// StartRecordingPipeline runs the uploader scan loop (call after the
// other async workers; ctx governs lifetime).
func StartRecordingPipeline(s *Server, ctx context.Context) (stop func()) {
	if s.recpipe == nil {
		return func() {}
	}
	return s.recpipe.Run(ctx, 5*time.Second)
}

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

// lcrGateways returns the broadcast gateway array with the LCR order
// encoded into Priority (competitive-analysis §3.1: cheapest first per
// destination prefix; the edge's lowest-priority-wins selection then
// reproduces the order with zero edge changes).
func (s *Server) lcrGateways() []gateway.Gateway {
	gws := s.gws.Gateways()
	r := &lcrRater{s: s, cache: map[string]*rate.Table{}}
	// Average rank across served prefixes — taking the MIN index
	// collapsed distinct cost orders (a gateway #0 on 8613 and #5 on
	// 86 both became Priority 0). Mean rank keeps relative cost order
	// in the single global priority the edge htable can carry.
	sum := map[string]int{}
	cnt := map[string]int{}
	for _, p := range distinctPrefixes(gws) {
		for i, g := range gateway.LCR(gws, p, r) {
			sum[g.ID] += i
			cnt[g.ID]++
		}
	}
	type ranked struct {
		gw  gateway.Gateway
		avg float64
	}
	rows := make([]ranked, 0, len(gws))
	for _, g := range gws {
		avg := float64(len(gws))
		if c := cnt[g.ID]; c > 0 {
			avg = float64(sum[g.ID]) / float64(c)
		}
		rows = append(rows, ranked{g, avg})
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].avg != rows[j].avg {
			return rows[i].avg < rows[j].avg
		}
		if rows[i].gw.Priority != rows[j].gw.Priority {
			return rows[i].gw.Priority < rows[j].gw.Priority
		}
		return rows[i].gw.ID < rows[j].gw.ID
	})
	out := make([]gateway.Gateway, 0, len(rows))
	for i, row := range rows {
		g := row.gw
		g.Priority = i
		out = append(out, g)
	}
	return out
}

// distinctPrefixes lists every prefix any gateway serves.
func distinctPrefixes(gws []gateway.Gateway) []string {
	seen := map[string]bool{}
	var out []string
	for _, g := range gws {
		for _, p := range g.Prefixes {
			if !seen[p] {
				seen[p] = true
				out = append(out, p)
			}
		}
	}
	if len(out) == 0 {
		out = []string{""} // no prefixes declared: base order only
	}
	return out
}

// StartWebhookFanout wires the multi-subscription fan-out (Phase D:
// subscriptions in PG 011, signed delivery, retry schedule) and
// loads the active set; returns false when nothing is subscribed.
// The legacy single-URL dispatcher (GVOS_WEBHOOK_URL) stays for
// backward compatibility.
func StartWebhookFanout(s *Server) bool {
	if s.pg == nil {
		return false
	}
	s.whStore = s.pg
	dsp := webhook.NewDispatcher(context.Background(), webhook.NewDeliverer())
	s.whDispatch = dsp
	s.refreshWebhookDispatcher()
	return len(dsp.SubscriptionCount()) > 0
}

// AttachBlocklistFeeds starts the dynamic feed loop for the
// blocklist (GVOS_BLOCKLIST_SOURCES form tag@url,...). Empty spec =
// feeds off. The static API-owned set survives every refresh.
func AttachBlocklistFeeds(s *Server, spec string, ctx context.Context) (stop func()) {
	feeds := blocklist.ParseFeedSpec(spec)
	if len(feeds) == 0 {
		return func() {}
	}
	blocklist.RunFeedLoop(ctx, s.blocklist, feeds, s.blocklist.Snapshot(), 5*time.Minute, 15*time.Minute)
	log.Printf("blocklist feeds on: %d sources", len(feeds))
	return func() {}
}

// AttachRedis wires the Redis runtime-state authority: the balance
// authority swaps to the Redis face (Phase 1), balances are seeded
// SETNX-style, and the call manager routes money through it
// (fail-closed at call start; interim/settle fail-open per the
// adoption plan).
func AttachRedis(s *Server, c *rds.Client) {
	s.rds = c
	s.bal = account.NewBalanceR(s.store, c)
	if err := s.bal.Seed(context.Background()); err != nil {
		log.Printf("redis balance seed partial: %v (SETNX keeps live values; retry at next boot)", err)
	}
	s.calls.SetBalanceAuthority(s.bal)
	// Phase 2: the shared admission ledger (chain-mirrored ZSETs +
	// per-second CPS counters) closes the ×N concurrent/CPS oversell.
	s.calls.SetAdmissionLedger(callcore.NewAdmissionR(c))
	s.calls.StartLedgerSweeper(time.Minute)
	// Cross-replica BYE fallback: Call-ID sticky at the LB is still
	// required for Interim/teardown locality; meta lets a peer settle
	// money + free the admission slot when stickiness fails.
	s.calls.SetCallMetaStore(callcore.NewCallMetaStore(c))
	// Phase 3: shared risk windows. GVOS_RISK_MODE=strict refuses
	// calls when Redis fraud state is unavailable (default: open).
	s.risk.SetRiskRedis(riskctrl.NewRiskR(c))
	switch strings.ToLower(os.Getenv("GVOS_RISK_MODE")) {
	case "strict":
		s.risk.SetMode(riskctrl.ModeStrict)
		log.Printf("risk mode: strict (Redis errors refuse calls)")
	default:
		s.risk.SetMode(riskctrl.ModeOpen)
	}
	// Phase 4: package usage. GVOS_PKG_MODE=open falls back to local
	// free minutes; default list_price charges list on Redis errors.
	s.pkg.SetPkgRedis(pkgplan.NewPkgR(c))
	switch strings.ToLower(os.Getenv("GVOS_PKG_MODE")) {
	case "open":
		s.pkg.SetMode(pkgplan.ModeOpen)
		log.Printf("pkg mode: open (Redis errors use local free minutes)")
	default:
		s.pkg.SetMode(pkgplan.ModeListPrice)
	}
	// Phase 5: persistent registration mirror + the single-runner locks
	// on the periodic tasks (rollup, config rebroadcast) so N replicas
	// run each exactly once.
	s.regs.SetRegRedis(regtable.NewRegR(c))
	s.redisLocks = true
}
