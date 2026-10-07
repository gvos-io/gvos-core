// Admin API: management endpoints for accounts and rates. Every
// mutating call writes an audit row (PG mode) and updates the live
// in-memory snapshot so Kamailio-facing behavior changes immediately.
package main

import (
	"context"
	"crypto/md5"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"time"

	"github.com/gvos/gvos-core/internal/account"
	"github.com/gvos/gvos-core/internal/blocklist"
	"github.com/gvos/gvos-core/internal/rate"
	"github.com/gvos/gvos-core/internal/mediastream"
	"github.com/gvos/gvos-core/internal/riskctrl"
)

const demoTenant = "00000000-0000-0000-0000-000000000001"
const demoRateGroup = "00000000-0000-0000-0000-000000000010"

// AdminAuth guards admin routes. The spike uses a static bearer token;
// production replaces this with OIDC/session + RBAC per PRD.
const adminToken = "dev-admin-token"

func (s *Server) requireAdmin(w http.ResponseWriter, r *http.Request) bool {
	if r.Header.Get("Authorization") != "Bearer "+adminToken {
		httpError(w, 401, "admin token required")
		return false
	}
	return true
}

// POST /v1/admin/accounts {"name","username","parent_id","rate_group","postpaid","credit"}
func (s *Server) handleAccountCreate(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) || !requirePOST(w, r) {
		return
	}
	var req struct {
		Name      string  `json:"name"`
		Username  string  `json:"username"`
		ParentID  string  `json:"parent_id"`
		RateGroup string  `json:"rate_group"`
		Postpaid  bool    `json:"postpaid"`
		Credit    float64 `json:"credit"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Username == "" {
		httpError(w, 400, "username required")
		return
	}
	if req.RateGroup == "" {
		req.RateGroup = "demo"
	}
	if req.ParentID == "" {
		if s.pg != nil {
			req.ParentID = "00000000-0000-0000-0000-0000000000a2" // seeded agent1
		} else {
			req.ParentID = "agent1" // memory-mode demo chain
		}
	}

	// SIP credential: random password, HA1 = MD5(user:realm:pass).
	// Returned once in the response; never retrievable again.
	sipPass := genSIPPassword(12)
	ha1 := computeHA1(req.Username, "gvos.local", sipPass)

	id := "acc-" + req.Username + "-" + time.Now().Format("150405")
	if s.pg != nil {
		var err error
		id, err = s.pg.CreateAccountWithHA1(r.Context(), demoTenant, req.ParentID,
			req.Name, req.Username, ha1, req.RateGroup, req.Postpaid, req.Credit)
		if err != nil {
			httpError(w, 500, err.Error())
			return
		}
	}
	if err := s.store.Put(&account.Account{
		ID: id, ParentID: req.ParentID, Name: req.Name,
		RateGroup: req.RateGroup, Postpaid: req.Postpaid, Credit: req.Credit,
	}); err != nil {
		httpError(w, 500, err.Error())
		return
	}
	s.audit(r, "create", "account", id, nil,
		map[string]any{"username": req.Username, "credit": req.Credit})
	writeJSON(w, 201, map[string]any{
		"id": id, "username": req.Username, "credit": req.Credit,
		"sip_password": sipPass, "sip_realm": "gvos.local",
		"note":          "sip password is shown once; store it now",
	})
}

// POST /v1/admin/topup {"account_id","delta","reason"}
func (s *Server) handleTopUp(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) || !requirePOST(w, r) {
		return
	}
	var req struct {
		AccountID string  `json:"account_id"`
		Delta     float64 `json:"delta"`
		Reason    string  `json:"reason"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.AccountID == "" || req.Delta == 0 {
		httpError(w, 400, "account_id and non-zero delta required")
		return
	}
	if req.Reason == "" {
		req.Reason = "topup"
	}

	before, _ := account.Balance(s.store, req.AccountID)

	balance := before + req.Delta
	if s.pg != nil {
		var err error
		balance, err = s.pg.TopUp(r.Context(), req.AccountID, req.Delta, req.Reason, "admin")
		if err != nil {
			httpError(w, 500, err.Error())
			return
		}
	}
	// Update the live in-memory balance: read-modify-write via Put.
	if a, err := s.store.Get(req.AccountID); err == nil {
		a.Credit = balance
		_ = s.store.Put(a)
	}
	s.audit(r, "topup", "account", req.AccountID,
		map[string]any{"balance": before}, map[string]any{"balance": balance, "delta": req.Delta})
	writeJSON(w, 200, map[string]any{"account_id": req.AccountID, "balance": balance})
}

// GET /v1/admin/accounts
func (s *Server) handleAccountList(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	accs, err := s.store.List()
	if err != nil {
		httpError(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"accounts": accs, "count": len(accs)})
}

// POST /v1/admin/rates {"prefix","price","init_dur","step_dur","min_dur"}
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
	if req.InitDur == 0 {
		req.InitDur = 60
	}
	if req.StepDur == 0 {
		req.StepDur = 6
	}

	if s.pg != nil {
		if err := s.pg.UpsertRate(r.Context(), demoRateGroup, req.Prefix, req.Price, req.InitDur, req.StepDur, req.MinDur); err != nil {
			httpError(w, 500, err.Error())
			return
		}
	} else {
		// Memory mode: upsert into rateList so rebuildRates sees it.
		replaced := false
		for i, rr := range s.rateList {
			if rr.Prefix == req.Prefix {
				s.rateList[i] = rate.Rate{
					Prefix: req.Prefix, Price: req.Price,
					InitDur: req.InitDur, StepDur: req.StepDur, MinDur: req.MinDur,
				}
				replaced = true
				break
			}
		}
		if !replaced {
			s.rateList = append(s.rateList, rate.Rate{
				Prefix: req.Prefix, Price: req.Price,
				InitDur: req.InitDur, StepDur: req.StepDur, MinDur: req.MinDur,
			})
		}
	}
	// Live rate table rebuild: the next call uses the new price.
	s.rebuildRates()

	s.audit(r, "upsert", "rate", req.Prefix, nil, req)
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

// audit is a no-op in memory mode; PG mode appends the audit row.
func (s *Server) audit(r *http.Request, action, entity, entityID string, before, after any) {
	if s.pg == nil {
		return
	}
	ctx, cancel := contextWithTimeout(r.Context())
	defer cancel()
	if err := s.pg.AppendAudit(ctx, demoTenant, "admin", action, entity, entityID, before, after); err != nil {
		// Audit failure must never block the business answer, but it
		// must be loud: append-only audit is a compliance feature.
		logPrintf("AUDIT persist failed (%s %s %s): %v", action, entity, entityID, err)
	}
}

func contextWithTimeout(parent context.Context) (context.Context, context.CancelFunc) {
	return contextWithTimeoutDuration(parent, 5*time.Second)
}
// POST /v1/admin/record {"enabled":bool} - global recording toggle.
// Kamailio reads the start-event answer's record flag and engages
// rtpengine recording per call.
func (s *Server) handleRecordToggle(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) || !requirePOST(w, r) {
		return
	}
	var req struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpError(w, 400, "body required")
		return
	}
	before := s.recordEnabled
	s.recordEnabled = req.Enabled
	s.audit(r, "toggle", "recording", "", map[string]any{"enabled": before}, map[string]any{"enabled": req.Enabled})
	writeJSON(w, 200, map[string]any{"enabled": s.recordEnabled})
}

// POST /v1/admin/blocklist {"kind","scope","pattern","is_regex"} -
// add a screening rule; DELETE semantics via {"remove":true}.
func (s *Server) handleBlocklist(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	switch r.Method {
	case http.MethodPost:
		var req struct {
			Kind    string `json:"kind"`   // black | white
			Scope   string `json:"scope"`  // src | dest
			Pattern string `json:"pattern"`
			IsRegex bool   `json:"is_regex"`
			Remove  bool   `json:"remove"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil ||
			(req.Kind != "black" && req.Kind != "white") ||
			(req.Scope != "src" && req.Scope != "dest") || req.Pattern == "" {
			httpError(w, 400, "kind(black|white), scope(src|dest), pattern required")
			return
		}
		if req.Remove {
			n := s.blocklist.Remove("", req.Kind, req.Scope, req.Pattern)
			s.audit(r, "remove", "blocklist", req.Pattern, map[string]any{"removed": n}, nil)
			writeJSON(w, 200, map[string]any{"removed": n})
			return
		}
		s.blocklist.Add(blocklist.Entry{
			Kind: req.Kind, Scope: req.Scope, Pattern: req.Pattern, IsRegex: req.IsRegex,
		})
		s.audit(r, "create", "blocklist", req.Pattern, nil, req)
		writeJSON(w, 201, map[string]any{"kind": req.Kind, "scope": req.Scope, "pattern": req.Pattern})
	case http.MethodGet:
		writeJSON(w, 200, map[string]any{"rules": s.blocklist.Count()})
	default:
		httpError(w, 405, "POST or GET")
	}
}

// POST /v1/admin/risk - set/replace the fraud rule set (audited).
// Body: {"rules":[{"id","kind","action","limit","cost_limit","prefixes","account"}]}
func (s *Server) handleRiskRules(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) || !requirePOST(w, r) {
		return
	}
	var req struct {
		Rules []riskctrl.Rule `json:"rules"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpError(w, 400, "body required")
		return
	}
	// Validate kinds before applying.
	for _, ru := range req.Rules {
		switch ru.Kind {
		case "freq", "cost_day", "dest_prefix":
		default:
			httpError(w, 400, "rule "+ru.ID+": kind must be freq|cost_day|dest_prefix")
			return
		}
		switch ru.Action {
		case "block", "alert":
		default:
			httpError(w, 400, "rule "+ru.ID+": action must be block|alert")
			return
		}
	}
	before := s.risk.Rules()
	s.risk.SetRules(req.Rules)
	s.audit(r, "replace", "risk_rules", "",
		map[string]any{"count": len(before)}, map[string]any{"count": len(req.Rules)})
	writeJSON(w, 200, map[string]any{"rules": len(req.Rules)})
}

// POST /v1/admin/stream-push {"call_id","direction":"uplink|downlink","audio":<base64 or raw>}
// Test hook: pushes telephony-side audio into a live call's media
// stream (drives ASR bridges without a real caller). Audio is base64
// to stay JSON-safe.
func (s *Server) handleStreamPush(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) || !requirePOST(w, r) {
		return
	}
	var req struct {
		CallID    string `json:"call_id"`
		Direction string `json:"direction"`
		AudioB64  string `json:"audio"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.CallID == "" || req.AudioB64 == "" {
		httpError(w, 400, "call_id and audio (base64) required")
		return
	}
	audio, err := base64.StdEncoding.DecodeString(req.AudioB64)
	if err != nil {
		httpError(w, 400, "audio base64 invalid")
		return
	}
	stream := s.media.Get(req.CallID)
	if stream == nil {
		httpError(w, 404, "no live stream for call")
		return
	}
	dir := mediastream.Uplink
	if req.Direction == "downlink" {
		dir = mediastream.Downlink
	}
	c := stream.Push(dir, audio)
	writeJSON(w, 200, map[string]any{"call_id": req.CallID, "seq": c.Seq, "bytes": len(audio)})
}


// genSIPPassword produces a pronounceable-ish random SIP password.
func genSIPPassword(n int) string {
	const alphabet = "abcdefghjkmnpqrstuvwxyz23456789ABCDEFGHJKMNPQRSTUVWXYZ"
	b := make([]byte, n)
	rand.Read(b)
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)]
	}
	return string(b)
}

// computeHA1: MD5(username:realm:password) for Digest auth.
func computeHA1(user, realm, pass string) string {
	sum := md5.Sum([]byte(user + ":" + realm + ":" + pass))
	return hex.EncodeToString(sum[:])
}

// startReportWorker runs the T+1 rollup: every hour, aggregates
// yesterday (and today-so-far at each pass, so intra-day numbers
// stay fresh). Manual trigger: POST /v1/admin/reports {"day":"..."}.
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
