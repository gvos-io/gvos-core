// Origination calls resource (M11 F1, docs/click-to-call-design.md):
// the customer-facing face for platform-dialed calls. POST /v1/calls
// admits (blacklist/risk/rate/balance prechecks — the full billing
// lock still rides the normal /v1/call/start path), persists a
// pending row, and publishes gvos.call.originate for the outboard UAC
// dialer (which issues the INVITE with call_id = calls.id). The
// existing signaling event flow (call start/answer/end) drives
// dialing → answered → ended; every transition emits the call.state
// webhook contract event.
package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gvos-io/gvos-core/internal/billingdomain/account"
	"github.com/gvos-io/gvos-core/internal/billingdomain/billing"
	"github.com/gvos-io/gvos-core/internal/shared/events"
	"github.com/gvos-io/gvos-core/internal/shared/store"
	"github.com/gvos-io/gvos-core/internal/webhook"
)

// origState values (mirror the calls.state CHECK).
const (
	origPending   = "pending"
	origDialing   = "dialing"
	origAnswered  = "answered"
	origEnded     = "ended"
	origCancelled = "cancelled"
	origFailed    = "failed"
)

// origDedupWindow: the (caller,callee) anti-storm window — a repeat
// POST for the same pair inside it is rejected (idempotency keys are
// the sanctioned retry path).
const origDedupWindow = 5 * time.Second

// trackOrigination indexes live originated calls by their event
// call_id (= the resource id) so the signaling hooks can find them
// without touching PG on the hot path.
func (s *Server) trackOrigination(callID string) {
	s.origMu.Lock()
	s.origIndex[callID] = true
	s.origMu.Unlock()
}

func (s *Server) untrackOrigination(callID string) {
	s.origMu.Lock()
	delete(s.origIndex, callID)
	s.origMu.Unlock()
}

func (s *Server) isTracked(callID string) bool {
	s.origMu.RLock()
	defer s.origMu.RUnlock()
	return s.origIndex[callID]
}

// callIdentity authenticates the origination face: any API-key role
// (admin/operator see everything; tenant/agent are scoped to their
// subtree). The internal token is deliberately NOT accepted — this is
// the customer plane, not the signaling face.
func (s *Server) callIdentity(w http.ResponseWriter, r *http.Request) (identity, bool) {
	h := r.Header.Get("Authorization")
	const bearer = "Bearer "
	if !strings.HasPrefix(h, bearer) || s.acl == nil {
		httpErrorC(w, 401, "UNAUTHORIZED", "origination requires an API key")
		return identity{}, false
	}
	k, ok := s.acl.Lookup(strings.TrimPrefix(h, bearer))
	if !ok {
		httpErrorC(w, 401, "UNAUTHORIZED", "unknown key")
		return identity{}, false
	}
	return identity{TenantID: k.TenantID, Role: k.Role, Prefix: k.Prefix}, true
}

// scopedAccountFor resolves account_id (id or username) for the
// caller: admin/operator → any account; tenant/agent → subtree only.
func (s *Server) scopedAccountFor(w http.ResponseWriter, id identity, accountRef string) (*account.Account, bool) {
	if mapped, ok := s.userIndex[accountRef]; ok {
		accountRef = mapped
	}
	acc, err := s.store.Get(accountRef)
	if err != nil {
		httpErrorC(w, 404, "NOT_FOUND", "no account "+accountRef)
		return nil, false
	}
	switch id.Role {
	case "tenant", "agent":
		if accountRef == id.TenantID || !s.tenantAccountsCached(id.TenantID)[accountRef] {
			httpErrorC(w, 404, "NOT_FOUND", "no such account in your scope")
			return nil, false
		}
	}
	return acc, true
}

// handleCallCreate serves POST /v1/calls {caller, callee|to_agent,
// account_id, max_duration_s?, custom?}: 202 with the call_id — the
// lifecycle is asynchronous by design (state via GET + call.state
// webhook).
func (s *Server) handleCallCreate(w http.ResponseWriter, r *http.Request) {
	id, ok := s.callIdentity(w, r)
	if !ok || !requirePOST(w, r) {
		return
	}
	var req struct {
		Caller       string          `json:"caller"`
		Callee       string          `json:"callee"`
		ToAgent      string          `json:"to_agent"`
		AccountID    string          `json:"account_id"`
		MaxDurationS int             `json:"max_duration_s"`
		Custom       json.RawMessage `json:"custom"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpErrorC(w, 400, "BAD_REQUEST", "body required")
		return
	}
	if req.Caller == "" || (req.Callee == "" && req.ToAgent == "") || (req.Callee != "" && req.ToAgent != "") {
		httpErrorC(w, 400, "BAD_REQUEST", "caller plus exactly one of callee|to_agent required")
		return
	}
	if req.AccountID == "" {
		httpErrorC(w, 400, "BAD_REQUEST", "account_id required")
		return
	}
	acc, ok := s.scopedAccountFor(w, id, req.AccountID)
	if !ok {
		return
	}
	if !acc.Enabled {
		httpErrorC(w, 403, "DISABLED", "account "+acc.ID+" is disabled")
		return
	}
	maxDur := req.MaxDurationS
	if maxDur == 0 {
		maxDur = 1800
	}
	if maxDur < 30 || maxDur > 7200 {
		httpErrorC(w, 400, "BAD_REQUEST", "max_duration_s must be 30..7200")
		return
	}

	// Anti-storm: same (caller,callee) pair inside the window → reject.
	dedupKey := req.Caller + "|" + req.Callee + req.ToAgent
	s.origMu.Lock()
	if at, seen := s.origDedup[dedupKey]; seen && time.Since(at) < origDedupWindow {
		s.origMu.Unlock()
		httpErrorC(w, 409, "DUP_ORIGINATION", "same caller/callee pair dialed within "+origDedupWindow.String()+" — retry after it passes or use Idempotency-Key")
		return
	}
	s.origDedup[dedupKey] = time.Now()
	s.origMu.Unlock()

	// Admission (the real billing lock rides /v1/call/start when the
	// UAC's INVITE lands): blacklist both dialed parties, rate
	// presence, fraud engine, and a balance precheck on the 60s lock
	// basis — never a hardcoded stub amount.
	dialed := []string{req.Caller}
	if req.Callee != "" {
		dialed = append(dialed, req.Callee)
	}
	for _, d := range dialed {
		if ok, why := s.blocklist.CheckScreen(d, "dest"); !ok {
			httpErrorC(w, 403, "FORBIDDEN", "number blocked: "+why)
			return
		}
	}
	now := time.Now()
	s.cfgMu.RLock()
	rates := s.rates
	if tbl := s.rateTables[acc.RateGroup]; tbl != nil {
		rates = tbl
	}
	s.cfgMu.RUnlock()
	entry := rates.Lookup(req.Caller, int(now.Weekday()), now.Hour()*60+now.Minute())
	if entry == nil {
		httpErrorC(w, 404, "NOT_FOUND", "no rate for destination "+req.Caller)
		return
	}
	estCost := (&billing.Charger{}).EstimatedCost(entry, 60)
	if h := s.risk.CheckCall(acc.ID, req.Caller, estCost); h.Blocked {
		s.emit(events.SubjectRiskBlocked, map[string]string{
			"account_id": acc.ID, "dest": req.Caller, "rule_id": h.RuleID, "reason": h.Reason,
		})
		s.publishRiskBlocked(acc.ID, acc.Username, req.Caller, h.RuleID, h.Reason)
		httpErrorC(w, 403, "FORBIDDEN", "risk-blocked ["+h.RuleID+"]: "+h.Reason)
		return
	}
	bal, balErr := account.Balance(s.store, acc.ID)
	if balErr != nil {
		httpError(w, 500, balErr.Error())
		return
	}
	if !acc.Postpaid && bal < estCost {
		httpErrorC(w, 403, "INSUFFICIENT_CREDIT", "balance "+bal.String()+" below the 60s estimate "+estCost.String())
		return
	}

	row := store.CallRow{
		TenantID: id.TenantID, AccountID: acc.ID,
		Caller: req.Caller, Callee: req.Callee, ToAgent: req.ToAgent,
		State: origPending, MaxDurationS: maxDur, Custom: req.Custom,
	}
	if s.pg != nil {
		cid, err := s.pg.CreateCall(r.Context(), row)
		if err != nil {
			httpError(w, 500, err.Error())
			return
		}
		row.ID = cid
	} else {
		row.ID = "call-" + strconv.FormatInt(time.Now().UnixNano(), 10)
		s.origMu.Lock()
		s.origCalls[row.ID] = &row
		s.origMu.Unlock()
	}
	s.trackOrigination(row.ID)

	s.emit(events.SubjectCallOriginate, map[string]any{
		"op": "dial", "call_id": row.ID,
		"caller": req.Caller, "callee": req.Callee, "to_agent": req.ToAgent,
		"account_id": acc.ID, "max_duration_s": maxDur,
	})
	s.emitCallState(&row, "")
	s.audit(r, "create", "call", row.ID, nil, map[string]any{
		"caller": req.Caller, "callee": req.Callee, "to_agent": req.ToAgent, "account": acc.ID,
	})
	writeJSON(w, http.StatusAccepted, map[string]any{"call_id": row.ID, "state": row.State})
}

// handleCallGet serves GET /v1/calls/{id}.
func (s *Server) handleCallGet(w http.ResponseWriter, r *http.Request) {
	id, ok := s.callIdentity(w, r)
	if !ok {
		return
	}
	row, ok := s.originationRow(w, r, id, r.PathValue("id"))
	if !ok {
		return
	}
	writeJSON(w, 200, map[string]any{"call": row, "item": row})
}

// handleCallList serves GET /v1/calls?state=&account_id=&limit=&cursor=.
func (s *Server) handleCallList(w http.ResponseWriter, r *http.Request) {
	id, ok := s.callIdentity(w, r)
	if !ok {
		return
	}
	state := r.URL.Query().Get("state")
	accounts := []string{}
	if ref := r.URL.Query().Get("account_id"); ref != "" {
		acc, ok := s.scopedAccountFor(w, id, ref)
		if !ok {
			return
		}
		accounts = append(accounts, acc.ID)
	} else if id.Role == "tenant" || id.Role == "agent" {
		accounts = setKeys(s.tenantAccountsCached(id.TenantID))
	}
	limit := 50
	if l := r.URL.Query().Get("limit"); l != "" {
		if n, err := strconv.Atoi(l); err == nil && n > 0 && n <= 500 {
			limit = n
		}
	}
	if s.pg != nil {
		rows, next, err := s.pg.ListCalls(r.Context(), state, accounts, limit, r.URL.Query().Get("cursor"))
		if err != nil {
			if strings.HasPrefix(err.Error(), "bad cursor") {
				httpErrorC(w, 400, "BAD_CURSOR", err.Error())
				return
			}
			httpError(w, 500, err.Error())
			return
		}
		writeJSON(w, 200, map[string]any{"calls": rows, "items": rows, "count": len(rows), "next_cursor": next})
		return
	}
	s.origMu.RLock()
	all := make([]store.CallRow, 0, len(s.origCalls))
	for _, row := range s.origCalls {
		if state != "" && row.State != state {
			continue
		}
		if len(accounts) > 0 && !containsStr(accounts, row.AccountID) {
			continue
		}
		all = append(all, *row)
	}
	s.origMu.RUnlock()
	sortRowsByCreated(all)
	limit, offset := listPage(r, len(all))
	page := all[offset : offset+limit]
	writeJSON(w, 200, map[string]any{"calls": page, "items": page, "count": len(page), "total": len(all)})
}

// handleCallCancel serves DELETE /v1/calls/{id}: pending/dialing →
// cancelled; answered → teardown dispatched, then cancelled; terminal
// states 409.
func (s *Server) handleCallCancel(w http.ResponseWriter, r *http.Request) {
	id, ok := s.callIdentity(w, r)
	if !ok {
		return
	}
	row, ok := s.originationRow(w, r, id, r.PathValue("id"))
	if !ok {
		return
	}
	switch row.State {
	case origEnded, origCancelled, origFailed:
		httpErrorC(w, 409, "INVALID_STATE", "call already "+row.State)
		return
	}
	if row.State == origAnswered {
		s.emit(events.SubjectCallTeardown, map[string]string{"call_id": row.ID, "reason": "admin"})
	}
	s.advanceOrigination(row.ID, origCancelled, "cancelled by request")
	writeJSON(w, 200, map[string]any{"cancelled": row.ID})
}

// originationRow fetches the row and enforces the caller's scope
// (tenant/agent: own subtree; a foreign call is a bare 404).
func (s *Server) originationRow(w http.ResponseWriter, r *http.Request, id identity, callID string) (*store.CallRow, bool) {
	if s.pg != nil {
		row, err := s.pg.GetCall(r.Context(), callID)
		if err != nil {
			httpErrorC(w, 404, "NOT_FOUND", "no such call")
			return nil, false
		}
		if id.Role == "tenant" || id.Role == "agent" {
			if row.AccountID == id.TenantID || !s.tenantAccountsCached(id.TenantID)[row.AccountID] {
				httpErrorC(w, 404, "NOT_FOUND", "no such call")
				return nil, false
			}
		}
		return row, true
	}
	s.origMu.RLock()
	row, ok := s.origCalls[callID]
	s.origMu.RUnlock()
	if !ok {
		httpErrorC(w, 404, "NOT_FOUND", "no such call")
		return nil, false
	}
	if id.Role == "tenant" || id.Role == "agent" {
		if row.AccountID == id.TenantID || !s.tenantAccountsCached(id.TenantID)[row.AccountID] {
			httpErrorC(w, 404, "NOT_FOUND", "no such call")
			return nil, false
		}
	}
	return row, true
}

// advanceOrigination drives the lifecycle from the signaling hooks
// (call start → dialing, answer → answered, end → ended). Only
// originated calls are affected; the terminal guard makes repeated
// events idempotent.
func (s *Server) advanceOrigination(callID, state, reason string) {
	if !s.isTracked(callID) {
		return
	}
	if s.pg != nil {
		if n, err := s.pg.UpdateCallState(context.Background(), callID, state, reason); err != nil || n == 0 {
			return // unknown/not-live: nothing to announce
		}
	} else {
		s.origMu.Lock()
		row, ok := s.origCalls[callID]
		if !ok || row.State == origEnded || row.State == origCancelled || row.State == origFailed {
			s.origMu.Unlock()
			return
		}
		row.State = state
		row.FailReason = reason
		s.origMu.Unlock()
	}
	if state == origEnded || state == origCancelled || state == origFailed {
		s.untrackOrigination(callID)
	}
	if row := s.callRowForEvent(callID); row != nil {
		s.emitCallState(row, reason)
	}
}

// callRowForEvent reads the row for the state announcement.
func (s *Server) callRowForEvent(callID string) *store.CallRow {
	if s.pg != nil {
		row, err := s.pg.GetCall(context.Background(), callID)
		if err != nil {
			return nil
		}
		return row
	}
	s.origMu.RLock()
	defer s.origMu.RUnlock()
	if row, ok := s.origCalls[callID]; ok {
		cp := *row
		return &cp
	}
	return nil
}

// emitCallState fans one lifecycle transition to the webhook plane.
func (s *Server) emitCallState(row *store.CallRow, reason string) {
	s.emitContractEvent(webhook.EventCallState, s.callOwners(row.AccountID), webhook.CallStateData{
		CallID: row.ID, State: row.State, Reason: reason,
		Caller: row.Caller, Callee: row.Callee, ToAgent: row.ToAgent,
	})
}

// StartOriginateTracker keeps every replica's live-origination index
// in sync via the bus: the replica that answered POST /v1/calls adds
// the id inline; peers learn from this subject (and remove on cancel).
func StartOriginateTracker(s *Server) {
	if s.events == nil {
		return
	}
	_ = s.events.Subscribe(events.SubjectCallOriginate, func(data []byte) {
		var op struct {
			Op     string `json:"op"`
			CallID string `json:"call_id"`
		}
		if json.Unmarshal(data, &op) != nil || op.CallID == "" {
			return
		}
		if op.Op == "cancel" {
			s.untrackOrigination(op.CallID)
			return
		}
		s.trackOrigination(op.CallID)
	})
}

// containsStr is a tiny helper (setKeys + slices would be heavier).
func containsStr(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}

// sortRowsByCreated orders memory-mode rows newest-first.
func sortRowsByCreated(rows []store.CallRow) {
	for i := 1; i < len(rows); i++ {
		for j := i; j > 0 && rows[j].CreatedAt.After(rows[j-1].CreatedAt); j-- {
			rows[j], rows[j-1] = rows[j-1], rows[j]
		}
	}
}
