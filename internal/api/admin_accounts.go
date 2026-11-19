// Account management: create, top up, list (resource routes).
package api

import (
	"context"
	"log"
	"github.com/lixuanqun/gvos-core/internal/billingdomain/money"
	"encoding/json"
	"net/http"
	"time"

	"github.com/lixuanqun/gvos-core/internal/billingdomain/account"
	"github.com/lixuanqun/gvos-core/internal/shared/events"
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
			httpError(w, 500, err.Error())
			return
		}
	}
	if err := s.store.Put(&account.Account{
		ID: id, ParentID: req.ParentID, Name: req.Name, Username: req.Username,
		RateGroup: req.RateGroup, Postpaid: req.Postpaid, Credit: creditAmt,
	}); err != nil {
		httpError(w, 500, err.Error())
		return
	}
	// Keep the live username->ID index warm so freshly created accounts
	// are immediately callable/resolvable by SIP username.
	if s.userIndex != nil {
		s.userIndex[req.Username] = id
	}
	s.audit(r, "create", "account", id, nil,
		map[string]any{"username": req.Username, "credit": req.Credit})
	// Peer replicas apply the account directly (HA1 never leaves PG:
	// the bus payload carries billing-relevant fields only).
	s.emit(events.SubjectCfgReload, map[string]any{
		"entity": "account", "op": "create",
		"account": map[string]any{
			"id": id, "username": req.Username, "name": req.Name,
			"parent_id": req.ParentID, "rate_group": req.RateGroup,
			"postpaid": req.Postpaid, "credit": req.Credit,
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
	deltaAmt, err := money.FromYuan(req.Delta)
	if err != nil {
		httpErrorC(w, 400, "BAD_AMOUNT", err.Error())
		return
	}

	before, _ := account.Balance(s.store, req.AccountID)

	balance := before.Add(deltaAmt)
	if s.pg != nil {
		balance, err = s.pg.TopUp(r.Context(), req.AccountID, deltaAmt, req.Reason, whoAmI(r).Prefix)
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
	// Redis authority gets the absolute value (adoption plan: TopUp
	// writes PG -> Redis absolute; the broadcast below keeps no-redis
	// peers' caches converging).
	if s.bal != nil {
		if err := s.bal.SetAbsolute(r.Context(), req.AccountID, balance); err != nil {
			log.Printf("REDIS topup absolute write failed (%s): %v", req.AccountID, err)
		}
	}
	s.audit(r, "topup", "account", req.AccountID,
		map[string]any{"balance": before}, map[string]any{"balance": balance, "delta": req.Delta})
	// Absolute new balance (not the delta) so replaying the broadcast
	// is idempotent on peer replicas.
	s.emit(events.SubjectCfgReload, map[string]any{
		"entity": "credit", "op": "topup",
		"account_id": req.AccountID, "credit": balance,
	})
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
