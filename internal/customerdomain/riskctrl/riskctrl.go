// Package riskctrl implements the real-time fraud engine: rule-based
// screening at call start (frequency caps, cost caps, destination
// admission) with hit actions (block | alert). Rules evaluate against
// live counters that the call path feeds; the cost cap settles from
// CDR costs at call end.
//
// The engine is deliberately synchronous and allocation-free on the
// hot path (lock-guarded counters) - it rides the billing critical
// path, so it must cost microseconds, not milliseconds.
package riskctrl

import (
	"context"
	"github.com/fastvoip/gvos-core/internal/billingdomain/money"
	"fmt"
	"strings"
	"sync"
	"time"
)

// Rule is one fraud-control rule.
type Rule struct {
	ID     string `json:"id"`
	Kind   string `json:"kind"` // "freq" calls/minute | "cost_day" daily spend | "dest_prefix" | "reg_freq" registrations/minute
	Action string `json:"action"` // "block" | "alert"
	// freq: max calls per window
	Limit int `json:"limit"`
	// cost_day: max accumulated cost
	CostLimit float64 `json:"cost_limit"`
	// dest_prefix: allowed (whitelist semantics) or blocked prefixes
	Prefixes []string `json:"prefixes"`
	// AppliesTo: "" = all accounts, else account id
	Account string `json:"account"`
	// Enabled default true
	Disabled bool `json:"disabled"`
}

// Engine holds rules and live counters.
type Engine struct {
	mu sync.Mutex
	// rules in evaluation order
	rules []Rule
	// per-account rolling call timestamps (freq windows)
	calls map[string][]int64 // account -> unix seconds
	// per-account daily cost accumulation (day -> account -> cost)
	dayCost map[int64]map[string]money.Amount
	// per-user rolling registration timestamps (reg_freq windows),
	// fed by the gvos.reg.event consumer on the bus
	regs map[string][]int64 // user -> unix seconds
	rr   *RiskR             // redis aggregate face (nil = local windows)
	now  func() time.Time
	cur  *time.Time // tests: shared clock handle
}

func New() *Engine {
	return &Engine{
		calls:   map[string][]int64{},
		dayCost: map[int64]map[string]money.Amount{},
		regs:    map[string][]int64{},
		now:     time.Now,
	}
}

// SetClock overrides the time source (tests).
func (e *Engine) SetClock(f func() time.Time) { e.now = f }

// SetRiskRedis wires the shared aggregate face (Phase 3). Local
// windows stay as the fail-open fallback and single-replica truth.
func (e *Engine) SetRiskRedis(r *RiskR) { e.rr = r }

// SetRules atomically replaces the rule set (admin plane).
func (e *Engine) SetRules(rules []Rule) {
	e.mu.Lock()
	e.rules = rules
	e.mu.Unlock()
}

// Rules returns a copy (admin listing).
func (e *Engine) Rules() []Rule {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]Rule, len(e.rules))
	copy(out, e.rules)
	return out
}

// Hit is the evaluation result: when Blocked, Reason carries the
// matched rule for the 403 body; alerts are surfaced to the log
// stream (production: webhook/event bus).
type Hit struct {
	Blocked bool
	Alerted bool
	RuleID  string
	Reason  string
}

// CheckCall runs the rules for a prospective call. It also advances
// the freq counter and day-cost reservation only when the call is
// admitted (blocked calls do not count toward frequency).
//
// The engine mutex guards the LOCAL windows only — the redis face
// runs outside it, so concurrent call admissions overlap their redis
// round trips instead of serializing behind one lock (the redis face
// is the shared truth; the local windows are the fail-open fallback
// and their exact interleaving is not observable).
func (e *Engine) CheckCall(account, dest string, estCost money.Amount) Hit {
	now := e.now()
	var hit Hit

	e.mu.Lock()
	rules := e.rules // header copy: SetRules swaps the slice, elements are never mutated
	rr := e.rr
	e.mu.Unlock()

	for _, r := range rules {
		if r.Disabled {
			continue
		}
		if r.Account != "" && r.Account != account {
			continue
		}
		switch r.Kind {
		case "freq":
			// Redis face: the shared sliding window counts every
			// replica's calls (Phase 3). Fail-open to local on error.
			if rr != nil {
				n, err := rr.SlidingCount(context.Background(), rfreqKey(account), now)
				if err == nil {
					if n > r.Limit {
						hit = decide(hit, r, fmt.Sprintf("%d calls/min exceeds %d (shared)", n, r.Limit))
						continue
					}
					continue
				}
				warn("freq window", err)
			}
			// calls in the trailing minute (inclusive of this attempt);
			// the prune-and-count needs the local window under the lock
			cutoff := now.Unix() - 60
			e.mu.Lock()
			ts := e.calls[account]
			recent := ts[:0]
			for _, t := range ts {
				if t >= cutoff {
					recent = append(recent, t)
				}
			}
			e.calls[account] = recent
			e.mu.Unlock()
			if len(recent)+1 > r.Limit {
				hit = decide(hit, r, fmt.Sprintf("%d calls/min exceeds %d", len(recent)+1, r.Limit))
				continue // rule matched; other rules may still add alerts
			}
		case "cost_day":
			day := dayKey(now)
			if rr != nil {
				spent, err := rr.CostDayGet(ctx0(), day, account)
				if err == nil {
					if spent.Add(estCost) > money.MustFromYuan(r.CostLimit) {
						hit = decide(hit, r, fmt.Sprintf("daily cost %s+%s exceeds %.2f (shared)", spent, estCost, r.CostLimit))
					}
					continue
				}
				warn("cost_day get", err)
			}
			e.mu.Lock()
			spent := e.dayCost[day][account]
			e.mu.Unlock()
			if spent.Add(estCost) > money.MustFromYuan(r.CostLimit) {
				hit = decide(hit, r, fmt.Sprintf("daily cost %s+%s exceeds %.2f", spent, estCost, r.CostLimit))
			}
		case "dest_prefix":
			// Admission list: the call destination must match one of
			// the rule prefixes (empty match = no rule effect).
			matched := false
			for _, p := range r.Prefixes {
				if strings.HasPrefix(dest, p) {
					matched = true
					break
				}
			}
			if !matched {
				hit = decide(hit, r, "destination not in admitted prefixes")
			}
		}
	}

	if !hit.Blocked {
		// Admitted: advance the local counters (fail-open fallback and
		// single-replica truth) under a short lock.
		e.mu.Lock()
		e.calls[account] = append(e.calls[account], now.Unix())
		day := dayKey(now)
		if e.dayCost[day] == nil {
			e.dayCost[day] = map[string]money.Amount{}
		}
		e.dayCost[day][account] += estCost
		e.mu.Unlock()
		if rr != nil {
			// freq window already advanced inside SlidingCount; the day
			// accumulator is advanced here (evaluate read, admit writes).
			if _, err := rr.CostDayAdd(ctx0(), day, account, estCost); err != nil {
				warn("cost_day add", err)
			}
		}
	}
	return hit
}

// SettleCost adjusts the daily accumulation when the actual cost
// differs from the estimate (call end reconciliation).
func (e *Engine) SettleCost(account string, estCost, actualCost money.Amount) {
	if e.rr != nil {
		day := dayKey(e.now())
		if err := e.rr.CostDaySub(ctx0(), day, account, estCost.Sub(actualCost)); err != nil {
			warn("cost_day settle", err)
		}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	day := dayKey(e.now())
	if e.dayCost[day] == nil {
		return
	}
	e.dayCost[day][account] += actualCost - estCost
}

// CheckReg runs the reg_freq rules for one registration event (fed by
// the gvos.reg.event consumer). Mirrors the freq path: the trailing
// minute window prunes first, the current attempt counts inclusive,
// and every registration (matching or not) is recorded so the window
// is authoritative across events. expirations do not count.
func (e *Engine) CheckReg(user string) Hit {
	e.mu.Lock()
	defer e.mu.Unlock()

	now := e.now()
	var hit Hit

	// Redis face: shared registration-storm window (Phase 3).
	if e.rr != nil {
		n, rerr := e.rr.SlidingCount(ctx0(), gfreqKey(user), now)
		if rerr == nil {
			for _, r := range e.rules {
				if r.Disabled || r.Kind != "reg_freq" {
					continue
				}
				if r.Account != "" && r.Account != user {
					continue
				}
				if n > r.Limit {
					hit = decide(hit, r, fmt.Sprintf("%d registrations/min exceeds %d (shared)", n, r.Limit))
				}
			}
			return hit // window already advanced inside SlidingCount
		}
		warn("reg window", rerr)
	}
	ts := e.regs[user]
	cutoff := now.Unix() - 60
	recent := ts[:0]
	for _, t := range ts {
		if t >= cutoff {
			recent = append(recent, t)
		}
	}
	e.regs[user] = recent

	for _, r := range e.rules {
		if r.Disabled || r.Kind != "reg_freq" {
			continue
		}
		if r.Account != "" && r.Account != user {
			continue
		}
		if len(recent)+1 > r.Limit {
			hit = decide(hit, r, fmt.Sprintf("%d registrations/min exceeds %d", len(recent)+1, r.Limit))
		}
	}
	// Registration storms count regardless of the hit outcome (the
	// next registration sees the accumulated window).
	e.regs[user] = append(e.regs[user], now.Unix())
	return hit
}

// decide folds a rule hit into the accumulating result: block rules
// mark Blocked; alert rules only set Alerted.
func decide(hit Hit, r Rule, reason string) Hit {
	if r.Action == "block" {
		hit.Blocked = true
		hit.RuleID = r.ID
		hit.Reason = reason
	} else {
		hit.Alerted = true
		if hit.RuleID == "" {
			hit.RuleID = r.ID
			hit.Reason = reason + " (alert)"
		}
	}
	return hit
}

func dayKey(t time.Time) int64 {
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, t.Location()).Unix()
}

// Counters returns the live counter state (monitoring).
func (e *Engine) Counters() (accounts int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.calls)
}
