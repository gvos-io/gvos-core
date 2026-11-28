// Account management: create, top up, list (resource routes).
package api

import (
	"context"
	"encoding/json"
	"github.com/gvos-io/gvos-core/internal/billingdomain/money"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/gvos-io/gvos-core/internal/billingdomain/account"
	"github.com/gvos-io/gvos-core/internal/shared/events"
)

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

	// Money edge conversion (ADR-0003): the wire still speaks decimal
	// yuan; the domain runs fixed-point from here on.
	creditAmt, err := money.FromYuan(req.Credit)
	if err != nil {
		httpErrorC(w, 400, "BAD_AMOUNT", err.Error())
		return
	}
	// SIP credential: random password, HA1 = MD5(user:realm:pass).
	// Returned once in the response; never retrievable again.
	sipPass := genSIPPassword(12)
	ha1 := computeHA1(req.Username, "gvos.local", sipPass)

	id := "acc-" + req.Username + "-" + time.Now().Format("150405")
	if s.pg != nil {
		var err error
		id, err = s.pg.CreateAccountWithHA1(r.Context(), demoTenant, req.ParentID,
			req.Name, req.Username, ha1, req.RateGroup, req.Postpaid, creditAmt)
		if err != nil {
			if strings.Contains(err.Error(), "duplicate key") {
				httpErrorC(w, 409, "DUPLICATE", "username already exists")
				return
			}
			httpError(w, 500, err.Error())
			return
		}
	}
	acc := &account.Account{
		ID: id, ParentID: req.ParentID, Name: req.Name, Username: req.Username,
		RateGroup: req.RateGroup, Postpaid: req.Postpaid, Credit: creditAmt,
	}
	if err := s.store.Put(acc); err != nil {
		httpError(w, 500, err.Error())
		return
	}
	// Seed Redis authority (SETNX) so prepaid credit is callable before
	// the next process restart / Seed() pass.
	if s.bal != nil && !req.Postpaid {
		if err := s.bal.SeedOne(r.Context(), id, creditAmt); err != nil {
			log.Printf("REDIS seed new account %s: %v", id, err)
		}
	}
	// Keep the live username->ID index warm so freshly created accounts
	// are immediately callable/resolvable by SIP username.
	if s.userIndex != nil {
		s.cfgMu.Lock()
		s.userIndex[req.Username] = id
		s.cfgMu.Unlock()
	}
	s.dropTenantSet()
	s.audit(r, "create", "account", id, nil,
		map[string]any{"username": req.Username, "credit": req.Credit})
	// Peer replicas apply the account directly (HA1 never leaves PG:
	// the bus payload carries billing-relevant fields only — all of
	// them: replicas missing concurrent/CPS/max_debt used to admit
	// against defaults until the next full reload).
	s.emit(events.SubjectCfgReload, map[string]any{
		"entity": "account", "op": "create",
		"account": map[string]any{
			"id": id, "username": req.Username, "name": req.Name,
			"parent_id": req.ParentID, "rate_group": req.RateGroup,
			"postpaid": req.Postpaid, "credit": req.Credit,
			"max_debt": acc.MaxDebt, "max_concurrent": acc.Concurrent, "max_cps": acc.CPS,
			"low_balance": acc.LowBalance,
		},
	})
	writeJSON(w, 201, map[string]any{
		"id": id, "username": req.Username, "credit": req.Credit,
		"sip_password": sipPass, "sip_realm": "gvos.local",
		"note": "sip password is shown once; store it now",
	})
}

// POST /v1/admin/topup {"account_id","delta","reason"}

func (s *Server) handleTopUp(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) || !requirePOST(w, r) {
		return
	}
	var req struct {
		Delta  float64 `json:"delta"`
		Reason string  `json:"reason"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Delta == 0 {
		httpError(w, 400, "non-zero delta required")
		return
	}
	reqAccountID := r.PathValue("id")
	if _, err := account.ParseID(reqAccountID); err != nil {
		httpErrorC(w, 404, "NOT_FOUND", "no account "+reqAccountID)
		return
	}
	if req.Reason == "" {
		req.Reason = "topup"
	}
	deltaAmt, err := money.FromYuan(req.Delta)
	if err != nil {
		httpErrorC(w, 400, "BAD_AMOUNT", err.Error())
		return
	}

	before, _ := account.Balance(s.store, reqAccountID)
	if s.bal != nil {
		if b, err := s.bal.Get(r.Context(), reqAccountID); err == nil {
			before = b
		}
	}

	var balance money.Amount
	if s.bal != nil {
		// Redis is the live authority: INCRBY the delta so concurrent
		// call deductions are not wiped by a stale PG absolute SET.
		balance, err = s.bal.IncrBy(r.Context(), reqAccountID, deltaAmt)
		if err != nil {
			httpError(w, 500, err.Error())
			return
		}
		if s.pg != nil {
			if err := s.pg.WriteBalance(r.Context(), reqAccountID, balance); err != nil {
				log.Printf("PG topup write-back failed (%s): %v", reqAccountID, err)
			}
			if err := s.pg.AppendLedger(r.Context(), reqAccountID, deltaAmt, balance, req.Reason, ""); err != nil {
				log.Printf("PG topup ledger failed (%s): %v", reqAccountID, err)
			}
		}
	} else {
		// No Redis: the in-memory balance is the authority. Move it by
		// the delta — not to PG's number (that read-modify-write raced a
		// concurrent call settle) — and mirror the same delta in PG.
		// Every call settle now deducts from accounts.credit too, so
		// the mirror and the authority stay in step across restarts.
		balance, err = s.store.AddCredit(reqAccountID, deltaAmt)
		if err != nil {
			httpError(w, 404, err.Error())
			return
		}
		if s.pg != nil {
			if _, err := s.pg.TopUp(r.Context(), reqAccountID, deltaAmt, req.Reason, whoAmI(r).Prefix); err != nil {
				httpError(w, 500, err.Error())
				return
			}
		}
	}
	s.audit(r, "topup", "account", reqAccountID,
		map[string]any{"balance": before}, map[string]any{"balance": balance, "delta": req.Delta})
	// Absolute new balance (not the delta) so replaying the broadcast
	// is idempotent on peer replicas.
	s.emit(events.SubjectCfgReload, map[string]any{
		"entity": "credit", "op": "topup",
		"account_id": reqAccountID, "credit": balance,
	})
	// A top-up re-arms the low-balance notify window: a renewed dip
	// signals immediately instead of waiting out the dedupe hour.
	rearmLowBalance(reqAccountID)
	s.publishLowBalance(reqAccountID)
	writeJSON(w, 200, map[string]any{"account_id": reqAccountID, "balance": balance})
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
	q := r.URL.Query()
	parentFilter := q.Get("parent_id")
	rootOnly := q.Get("roots") == "1" || q.Get("roots") == "true"
	needle := strings.ToLower(strings.TrimSpace(q.Get("q")))
	if parentFilter != "" || rootOnly || needle != "" {
		filtered := accs[:0:0]
		for _, a := range accs {
			if a == nil {
				continue
			}
			if parentFilter != "" && a.ParentID != parentFilter {
				continue
			}
			if rootOnly && a.ParentID != "" {
				continue
			}
			if needle != "" {
				hay := strings.ToLower(a.ID + " " + a.Name + " " + a.Username)
				if !strings.Contains(hay, needle) {
					continue
				}
			}
			filtered = append(filtered, a)
		}
		accs = filtered
	}
	// Dual keys: historical "accounts" + console-friendly "items".
	writeJSON(w, 200, map[string]any{"accounts": accs, "items": accs, "count": len(accs)})
}

// POST /v1/admin/rates {"prefix","price","init_dur","step_dur","min_dur"}

// snapshotRedisBalances writes the authoritative Redis balances back
// into PG accounts.credit — the adoption plan's disaster-recovery
// point: a lost Redis is re-seeded from the last snapshot, bounded by
// one rollup interval of movements.
func (s *Server) snapshotRedisBalances(ctx context.Context) {
	if s.bal == nil {
		return
	}
	accs, err := s.store.List()
	if err != nil {
		log.Printf("REDIS balance snapshot: list failed: %v", err)
		return
	}
	n := 0
	for _, a := range accs {
		bal, err := s.bal.Get(ctx, a.ID)
		if err != nil {
			log.Printf("REDIS balance snapshot %s: %v", a.ID, err)
			continue
		}
		if bal == a.Credit {
			continue // unchanged, skip the write
		}
		if err := s.pg.WriteBalance(ctx, a.ID, bal); err != nil {
			log.Printf("REDIS balance snapshot %s: write: %v", a.ID, err)
			continue
		}
		a.Credit = bal
		_ = s.store.Put(a)
		n++
	}
	if n > 0 {
		log.Printf("REDIS balance snapshot: %d accounts written back to PG", n)
	}
}

// handleAccountGet serves GET /v1/admin/accounts/{id}: one account by
// id (or username, for operator convenience). The console detail page
// used to pull the full list and match client-side — O(N) and broken
// under pagination.
func (s *Server) handleAccountGet(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	id := r.PathValue("id")
	if mapped, ok := s.userIndex[id]; ok {
		id = mapped // username → account id
	}
	acc, err := s.store.Get(id)
	if err != nil {
		httpErrorC(w, 404, "NOT_FOUND", "no account "+id)
		return
	}
	writeJSON(w, 200, map[string]any{"account": acc, "item": acc})
}

// PATCH /v1/admin/accounts/{id}: optional-field update of the
// management knobs (name, rate_group, postpaid, max_debt,
// max_concurrent, max_cps). Credit is deliberately not patchable —
// balance moves only through topup/settlement so balance_ledger stays
// the single money story. Disabling an account is NOT here: the
// schema and admission path have no enabled flag yet (gap analysis
// M6 remainder).
func (s *Server) handleAccountPatch(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) || !requirePATCH(w, r) {
		return
	}
	id := r.PathValue("id")
	if mapped, ok := s.userIndex[id]; ok {
		id = mapped
	}
	acc, err := s.store.Get(id)
	if err != nil {
		httpErrorC(w, 404, "NOT_FOUND", "no account "+id)
		return
	}
	before := *acc
	var req struct {
		Name          *string  `json:"name"`
		RateGroup     *string  `json:"rate_group"`
		Postpaid      *bool    `json:"postpaid"`
		MaxDebt       *float64 `json:"max_debt"`
		MaxConcurrent *int     `json:"max_concurrent"`
		MaxCPS        *int     `json:"max_cps"`
		LowBalance    *float64 `json:"low_balance"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpErrorC(w, 400, "BAD_REQUEST", "body required")
		return
	}
	var maxDebt *money.Amount
	if req.MaxDebt != nil {
		if *req.MaxDebt < 0 {
			httpErrorC(w, 400, "BAD_REQUEST", "max_debt must be >= 0")
			return
		}
		amt, err := money.FromYuan(*req.MaxDebt)
		if err != nil {
			httpErrorC(w, 400, "BAD_AMOUNT", err.Error())
			return
		}
		maxDebt = &amt
		acc.MaxDebt = amt
	}
	if req.Name != nil {
		acc.Name = *req.Name
	}
	if req.RateGroup != nil {
		if *req.RateGroup == "" {
			httpErrorC(w, 400, "BAD_REQUEST", "rate_group must be non-empty")
			return
		}
		acc.RateGroup = *req.RateGroup
	}
	if req.Postpaid != nil {
		acc.Postpaid = *req.Postpaid
	}
	if req.MaxConcurrent != nil {
		if *req.MaxConcurrent < 0 {
			httpErrorC(w, 400, "BAD_REQUEST", "max_concurrent must be >= 0")
			return
		}
		acc.Concurrent = *req.MaxConcurrent
	}
	if req.MaxCPS != nil {
		if *req.MaxCPS < 0 {
			httpErrorC(w, 400, "BAD_REQUEST", "max_cps must be >= 0")
			return
		}
		acc.CPS = *req.MaxCPS
	}
	var lowBal *money.Amount
	if req.LowBalance != nil {
		if *req.LowBalance < 0 {
			httpErrorC(w, 400, "BAD_REQUEST", "low_balance must be >= 0")
			return
		}
		amt, err := money.FromYuan(*req.LowBalance)
		if err != nil {
			httpErrorC(w, 400, "BAD_AMOUNT", err.Error())
			return
		}
		lowBal = &amt
		acc.LowBalance = amt
		rearmLowBalance(id)
	}
	// Durable first, then the in-memory mirror (same discipline as
	// gateway PUT).
	if s.pg != nil {
		if err := s.pg.UpdateAccountPatch(r.Context(), id, req.Name, req.RateGroup, req.Postpaid, maxDebt, req.MaxConcurrent, req.MaxCPS, lowBal); err != nil {
			log.Printf("PATCH account %s failed: %v", id, err)
			httpError(w, 500, err.Error())
			return
		}
	}
	if err := s.store.Put(acc); err != nil {
		httpError(w, 500, err.Error())
		return
	}
	s.audit(r, "update", "account", id, before, acc)
	// Peer replicas apply the same snapshot (create/update share the
	// apply path; op distinguishes audit semantics only).
	s.emit(events.SubjectCfgReload, map[string]any{
		"entity": "account", "op": "update",
		"account": map[string]any{
			"id": acc.ID, "username": acc.Username, "name": acc.Name,
			"parent_id": acc.ParentID, "rate_group": acc.RateGroup,
			"postpaid": acc.Postpaid, "credit": acc.Credit.String(),
			"max_debt": acc.MaxDebt.String(), "max_concurrent": acc.Concurrent, "max_cps": acc.CPS,
			"low_balance": acc.LowBalance.String(),
		},
	})
	writeJSON(w, 200, map[string]any{"account": acc})
}

// DELETE /v1/admin/accounts/{id}: soft delete (deleted_at; LoadAccounts
// already filters it). Refused while live children exist — deleting a
// parent would orphan the charging chain.
func (s *Server) handleAccountDelete(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	id := r.PathValue("id")
	if mapped, ok := s.userIndex[id]; ok {
		id = mapped
	}
	acc, err := s.store.Get(id)
	if err != nil {
		httpErrorC(w, 404, "NOT_FOUND", "no account "+id)
		return
	}
	children := 0
	if s.pg != nil {
		if children, err = s.pg.CountAccountChildren(r.Context(), id); err != nil {
			httpError(w, 500, err.Error())
			return
		}
	} else {
		if all, lerr := s.store.List(); lerr == nil {
			for _, a := range all {
				if a.ParentID == id {
					children++
				}
			}
		}
	}
	if children > 0 {
		httpErrorC(w, 409, "HAS_CHILDREN", "account has live child accounts; delete or move them first")
		return
	}
	if s.pg != nil {
		if n, derr := s.pg.SoftDeleteAccount(r.Context(), id); derr != nil {
			httpError(w, 500, derr.Error())
			return
		} else if n == 0 {
			httpErrorC(w, 404, "NOT_FOUND", "no account "+id)
			return
		}
	}
	s.store.Delete(id)
	s.dropTenantSet()
	// Drop stale username mappings pointing at the deleted account.
	s.cfgMu.Lock()
	for u, mapped := range s.userIndex {
		if mapped == id {
			delete(s.userIndex, u)
		}
	}
	s.cfgMu.Unlock()
	// The Redis balance key is left in place: Seed() filters deleted
	// accounts on next boot, and re-creating the same username mints a
	// new id, so the stale key is unreachable.
	s.audit(r, "delete", "account", id, acc, nil)
	s.emit(events.SubjectCfgReload, map[string]any{
		"entity": "account", "op": "delete",
		"account": map[string]any{"id": id, "username": acc.Username},
	})
	writeJSON(w, 200, map[string]any{"deleted": id})
}
