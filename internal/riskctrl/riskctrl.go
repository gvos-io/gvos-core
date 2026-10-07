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
	"fmt"
	"strings"
	"sync"
	"time"
)

// Rule is one fraud-control rule.
type Rule struct {
	ID     string
	Kind   string // "freq" calls/minute | "cost_day" daily spend | "dest_prefix"
	Action string // "block" | "alert"
	// freq: max calls per window
	Limit int
	// cost_day: max accumulated cost
	CostLimit float64
	// dest_prefix: allowed (whitelist semantics) or blocked prefixes
	Prefixes []string
	// AppliesTo: "" = all accounts, else account id
	Account string
	// Enabled default true
	Disabled bool
}

// Engine holds rules and live counters.
type Engine struct {
	mu sync.Mutex
	// rules in evaluation order
	rules []Rule
	// per-account rolling call timestamps (freq windows)
	calls map[string][]int64 // account -> unix seconds
	// per-account daily cost accumulation (day -> account -> cost)
	dayCost map[int64]map[string]float64
	now     func() time.Time
	cur     *time.Time // tests: shared clock handle
}

func New() *Engine {
	return &Engine{
		calls:   map[string][]int64{},
		dayCost: map[int64]map[string]float64{},
		now:     time.Now,
	}
}

// SetClock overrides the time source (tests).
func (e *Engine) SetClock(f func() time.Time) { e.now = f }

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
func (e *Engine) CheckCall(account, dest string, estCost float64) Hit {
	e.mu.Lock()
	defer e.mu.Unlock()

	now := e.now()
	var hit Hit

	for _, r := range e.rules {
		if r.Disabled {
			continue
		}
		if r.Account != "" && r.Account != account {
			continue
		}
		switch r.Kind {
		case "freq":
			// calls in the trailing minute (inclusive of this attempt)
			cutoff := now.Unix() - 60
			ts := e.calls[account]
			recent := ts[:0]
			for _, t := range ts {
				if t >= cutoff {
					recent = append(recent, t)
				}
			}
			e.calls[account] = recent
			if len(recent)+1 > r.Limit {
				hit = decide(hit, r, fmt.Sprintf("%d calls/min exceeds %d", len(recent)+1, r.Limit))
				continue // rule matched; other rules may still add alerts
			}
		case "cost_day":
			day := dayKey(now)
			spent := e.dayCost[day][account]
			if spent+estCost > r.CostLimit {
				hit = decide(hit, r, fmt.Sprintf("daily cost %.7f+%.7f exceeds %.7f", spent, estCost, r.CostLimit))
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
		// Admitted: advance counters.
		e.calls[account] = append(e.calls[account], now.Unix())
		day := dayKey(now)
		if e.dayCost[day] == nil {
			e.dayCost[day] = map[string]float64{}
		}
		e.dayCost[day][account] += estCost
	}
	return hit
}

// SettleCost adjusts the daily accumulation when the actual cost
// differs from the estimate (call end reconciliation).
func (e *Engine) SettleCost(account string, estCost, actualCost float64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	day := dayKey(e.now())
	if e.dayCost[day] == nil {
		return
	}
	e.dayCost[day][account] += actualCost - estCost
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
