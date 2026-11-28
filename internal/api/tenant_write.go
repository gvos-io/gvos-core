// Tenant-plane write operations (P1 M10, authorization ruling:
// role=tenant funds and provisions its own subtree; role=agent stays
// read-only). The money semantics are TRANSFER, not injection: a
// tenant reallocates its own credit downward, both sides are
// journaled (transfer-out / transfer-in) — a reseller cannot conjure
// credit, and the ledger keeps the conservation invariant auditable.
package api

import (
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/gvos-io/gvos-core/internal/billingdomain/account"
	"github.com/gvos-io/gvos-core/internal/billingdomain/money"
	"github.com/gvos-io/gvos-core/internal/shared/events"
)

// tenantScopedAccount resolves {id} to an account inside the caller's
// subtree, excluding the tenant root itself (a tenant's own balance is
// funded from above; it reallocates downward only).
func (s *Server) tenantScopedAccount(w http.ResponseWriter, tenantID, id string) (*account.Account, bool) {
	if mapped, ok := s.userIndex[id]; ok {
		id = mapped
	}
	acc, err := s.store.Get(id)
	if err != nil || id == tenantID {
		httpErrorC(w, 404, "NOT_FOUND", "no such account in your scope")
		return nil, false
	}
	if !s.tenantAccountsCached(tenantID)[id] {
		// Escape attempt: do not reveal whether the account exists.
		httpErrorC(w, 404, "NOT_FOUND", "no such account in your scope")
		return nil, false
	}
	return acc, true
}

// handleTenantTopUp serves POST /v1/tenant/accounts/{id}/topup
// {"delta","reason"}: move delta from the tenant root's balance down
// to a child. Positive delta only — clawback is an admin action.
func (s *Server) handleTenantTopUp(w http.ResponseWriter, r *http.Request) {
	id, ok := s.requireTenant(w, r)
	if !ok {
		return
	}
	if id.Role != "tenant" {
		httpErrorC(w, 403, "FORBIDDEN", "role agent is read-only on the tenant plane")
		return
	}
	child, ok := s.tenantScopedAccount(w, id.TenantID, r.PathValue("id"))
	if !ok {
		return
	}
	var req struct {
		Delta  float64 `json:"delta"`
		Reason string  `json:"reason"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Delta <= 0 {
		httpErrorC(w, 400, "BAD_REQUEST", "delta must be > 0 (allocation downward)")
		return
	}
	deltaAmt, err := money.FromYuan(req.Delta)
	if err != nil {
		httpErrorC(w, 400, "BAD_AMOUNT", err.Error())
		return
	}

	// Affordability check against the live authority (PG path is
	// transactional and re-checks under row lock).
	tenantBal, _ := account.Balance(s.store, id.TenantID)
	if s.bal != nil {
		if b, berr := s.bal.Get(r.Context(), id.TenantID); berr == nil {
			tenantBal = b
		}
	}
	if tenantBal < deltaAmt {
		httpErrorC(w, 403, "INSUFFICIENT_CREDIT", "tenant balance "+tenantBal.String()+" below delta "+deltaAmt.String())
		return
	}

	var fromBal, toBal money.Amount
	if s.pg != nil {
		fromBal, toBal, err = s.pg.TransferCredit(r.Context(), id.TenantID, child.ID, deltaAmt, id.Prefix)
		if err != nil {
			log.Printf("tenant transfer %s -> %s failed: %v", id.TenantID, child.ID, err)
			httpErrorC(w, 403, "INSUFFICIENT_CREDIT", "transfer rejected")
			return
		}
	} else {
		// Memory mode: two-sided mutation, mirror only.
		if fromBal, err = s.store.AddCredit(id.TenantID, -deltaAmt); err != nil {
			httpError(w, 500, err.Error())
			return
		}
		if toBal, err = s.store.AddCredit(child.ID, deltaAmt); err != nil {
			// Roll the tenant side back — never leave the pair split.
			if _, rerr := s.store.AddCredit(id.TenantID, deltaAmt); rerr != nil {
				log.Printf("tenant transfer rollback failed (%s): %v", id.TenantID, rerr)
			}
			httpError(w, 500, err.Error())
			return
		}
	}
	if s.bal != nil {
		// Redis is the live authority: mirror both sides.
		if fromBal, err = s.bal.IncrBy(r.Context(), id.TenantID, -deltaAmt); err != nil {
			httpError(w, 500, err.Error())
			return
		}
		if toBal, err = s.bal.IncrBy(r.Context(), child.ID, deltaAmt); err != nil {
			httpError(w, 500, err.Error())
			return
		}
		if s.pg != nil {
			if werr := s.pg.WriteBalance(r.Context(), id.TenantID, fromBal); werr != nil {
				log.Printf("tenant transfer write-back from: %v", werr)
			}
			if werr := s.pg.WriteBalance(r.Context(), child.ID, toBal); werr != nil {
				log.Printf("tenant transfer write-back to: %v", werr)
			}
		}
	}
	reason := req.Reason
	if reason == "" {
		reason = "allocation"
	}
	s.audit(r, "transfer", "account", child.ID,
		map[string]any{"from_balance": tenantBal.String()}, map[string]any{"delta": deltaAmt.String(), "to_balance": toBal.String(), "reason": reason})
	s.emit(events.SubjectCfgReload, map[string]any{
		"entity": "credit", "op": "topup", "account_id": id.TenantID, "credit": fromBal,
	})
	s.emit(events.SubjectCfgReload, map[string]any{
		"entity": "credit", "op": "topup", "account_id": child.ID, "credit": toBal,
	})
	rearmLowBalance(child.ID)
	writeJSON(w, 200, map[string]any{
		"from_account_id": id.TenantID, "to_account_id": child.ID,
		"delta": deltaAmt.String(), "from_balance": fromBal, "to_balance": toBal,
	})
}

// handleTenantAccountCreate serves POST /v1/tenant/accounts
// {"username","name","rate_group"?,"initial_credit"?}: the tenant
// provisions a child under itself. initial_credit (optional) is
// allocated from the tenant's own balance via the transfer primitive —
// not created from nothing. rate_group defaults to the tenant's own.
func (s *Server) handleTenantAccountCreate(w http.ResponseWriter, r *http.Request) {
	id, ok := s.requireTenant(w, r)
	if !ok {
		return
	}
	if id.Role != "tenant" {
		httpErrorC(w, 403, "FORBIDDEN", "role agent is read-only on the tenant plane")
		return
	}
	tenantAcc, terr := s.store.Get(id.TenantID)
	if terr != nil {
		httpError(w, 500, "tenant root account missing: "+terr.Error())
		return
	}
	var req struct {
		Username      string  `json:"username"`
		Name          string  `json:"name"`
		RateGroup     string  `json:"rate_group"`
		InitialCredit float64 `json:"initial_credit"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Username == "" {
		httpErrorC(w, 400, "BAD_REQUEST", "username required")
		return
	}
	rateGroup := req.RateGroup
	if rateGroup == "" {
		rateGroup = tenantAcc.RateGroup // inherit the reseller's own book
	}
	creditAmt, err := money.FromYuan(req.InitialCredit)
	if err != nil {
		httpErrorC(w, 400, "BAD_AMOUNT", err.Error())
		return
	}
	if creditAmt < 0 {
		httpErrorC(w, 400, "BAD_REQUEST", "initial_credit must be >= 0")
		return
	}

	sipPass := genSIPPassword(12)
	ha1 := computeHA1(req.Username, "gvos.local", sipPass)
	var childID string
	if s.pg != nil {
		childID, err = s.pg.CreateAccountWithHA1(r.Context(), demoTenant, id.TenantID,
			req.Name, req.Username, ha1, rateGroup, false, 0)
		if err != nil {
			if strings.Contains(err.Error(), "duplicate key") {
				httpErrorC(w, 409, "DUPLICATE", "username already exists")
				return
			}
			log.Printf("tenant account create failed: %v", err)
			httpError(w, 500, err.Error())
			return
		}
	} else {
		childID = "acc-" + req.Username + "-" + time.Now().Format("150405")
	}
	child := &account.Account{
		ID: childID, ParentID: id.TenantID, Name: req.Name, Username: req.Username,
		RateGroup: rateGroup, Credit: 0, Enabled: true,
	}
	if err := s.store.Put(child); err != nil {
		httpError(w, 500, err.Error())
		return
	}
	s.cfgMu.Lock()
	if s.userIndex != nil {
		s.userIndex[req.Username] = childID
	}
	s.cfgMu.Unlock()
	s.dropTenantSet() // subtree membership changed

	// Initial allocation rides the same transfer primitive (checked,
	// journaled) — the child never receives credit from nothing.
	var fromBal, toBal money.Amount
	if creditAmt > 0 {
		if s.pg != nil {
			fromBal, toBal, err = s.pg.TransferCredit(r.Context(), id.TenantID, childID, creditAmt, id.Prefix)
			if err != nil {
				log.Printf("tenant initial allocation %s -> %s failed: %v", id.TenantID, childID, err)
				httpErrorC(w, 403, "INSUFFICIENT_CREDIT", "account created, but initial allocation rejected (tenant balance insufficient)")
				return
			}
		} else {
			if fromBal, err = s.store.AddCredit(id.TenantID, -creditAmt); err == nil {
				if toBal, err = s.store.AddCredit(childID, creditAmt); err != nil {
					if _, rerr := s.store.AddCredit(id.TenantID, creditAmt); rerr != nil {
						log.Printf("tenant initial allocation rollback failed: %v", rerr)
					}
				}
			}
			if err != nil {
				httpError(w, 500, err.Error())
				return
			}
		}
		if s.bal != nil {
			if _, ierr := s.bal.IncrBy(r.Context(), id.TenantID, -creditAmt); ierr == nil {
				if _, ierr2 := s.bal.IncrBy(r.Context(), childID, creditAmt); ierr2 == nil {
					if s.pg != nil {
						_ = s.pg.WriteBalance(r.Context(), childID, toBal)
					}
				}
			}
		}
	}
	s.audit(r, "create", "account", childID, nil,
		map[string]any{"username": req.Username, "parent": id.TenantID, "initial_credit": req.InitialCredit})
	s.emit(events.SubjectCfgReload, map[string]any{
		"entity": "account", "op": "create",
		"account": map[string]any{
			"id": childID, "username": req.Username, "name": req.Name,
			"parent_id": id.TenantID, "rate_group": rateGroup,
			"postpaid": false, "credit": creditAmt.String(),
		},
	})
	writeJSON(w, 201, map[string]any{
		"id": childID, "username": req.Username, "credit": creditAmt.String(),
		"from_balance": fromBal, "to_balance": toBal,
		"sip_password": sipPass, "sip_realm": "gvos.local",
		"note": "sip password is shown once; store it now",
	})
}
