// Webhook contract-event emissions (P1 M8): the three planned events
// roadmap Phase D never shipped — account.low_balance, risk.blocked,
// package.exhausted. All ride publishContract → PublishEnvelopeScoped
// (typed contract projections, tenant-scoped via callOwners,
// non-blocking: the billing path never waits on observability).
package api

import (
	"sync"
	"time"

	"github.com/gvos-io/gvos-core/internal/billingdomain/account"
	"github.com/gvos-io/gvos-core/internal/webhook"
)

// emitContractEvent fans one contract event to the webhook
// dispatcher (typed projection, tenant-scoped, non-blocking).
func (s *Server) emitContractEvent(event string, owners []string, data any) {
	if s.whDispatch == nil {
		return
	}
	s.whDispatch.PublishEnvelopeScoped(event, demoTenant, owners, data)
}

// publishRiskBlocked is called from the call-start admission path when
// the fraud engine blocks (same seam as the existing NATS
// gvos.risk.alert emission).
func (s *Server) publishRiskBlocked(accID, username, dest, ruleID, reason string) {
	s.emitContractEvent(webhook.EventRiskBlocked, s.callOwners(accID),
		webhook.RiskBlockedData{
			AccountID: accID, Username: username, Dest: dest,
			RuleID: ruleID, Reason: reason,
		})
}

// publishPackageExhausted fires when a settlement crosses the cycle
// allowance (RateResult.ExhaustedNow).
func (s *Server) publishPackageExhausted(accID string, pkgID string, freeMinutes int) {
	s.emitContractEvent(webhook.EventPackageExhausted, s.callOwners(accID),
		webhook.PackageExhaustedData{
			AccountID: accID, PackageID: pkgID, FreeMinutes: freeMinutes,
		})
}

// lowBalanceNotifyMu guards the dedupe window: at most one
// account.low_balance per account per hour (a settling call below
// threshold fires once, not on every subsequent call).
var lowBalanceNotifyMu sync.Mutex
var lowBalanceNotified = map[string]time.Time{}

const lowBalanceNotifyWindow = time.Hour

// publishLowBalance checks the account's threshold against its live
// balance and emits through the contract envelope when the balance is
// at/below the threshold and the notify window has elapsed. Balance
// reads the in-memory mirror (Commit keeps it fresh in memory mode;
// Redis-authority mode mirrors settle asynchronously — best-effort by
// design, the webhook is a nudge, not a statement).
func (s *Server) publishLowBalance(accID string) {
	acc, err := s.store.Get(accID)
	if err != nil || acc.LowBalance == 0 {
		return
	}
	lowBalanceNotifyMu.Lock()
	if at, ok := lowBalanceNotified[accID]; ok && time.Since(at) < lowBalanceNotifyWindow {
		lowBalanceNotifyMu.Unlock()
		return
	}
	bal, balErr := account.Balance(s.store, accID)
	lowBalanceNotifyMu.Unlock()
	if balErr != nil {
		return
	}
	// Above threshold (or the mirror is stale-high): no signal, but a
	// top-up clears the window so the next dip re-notifies promptly.
	if bal > acc.LowBalance {
		return
	}
	lowBalanceNotifyMu.Lock()
	lowBalanceNotified[accID] = time.Now()
	lowBalanceNotifyMu.Unlock()
	s.emitContractEvent(webhook.EventAccountLowBalance, s.callOwners(accID),
		webhook.AccountLowBalanceData{
			AccountID: accID,
			Balance:   bal.String(),
			Threshold: acc.LowBalance.String(),
			Currency:  acc.Currency,
		})
}

// rearmLowBalance clears the notify window on a top-up so a renewed
// dip is signalled immediately instead of waiting out the hour.
func rearmLowBalance(accID string) {
	lowBalanceNotifyMu.Lock()
	delete(lowBalanceNotified, accID)
	lowBalanceNotifyMu.Unlock()
}
