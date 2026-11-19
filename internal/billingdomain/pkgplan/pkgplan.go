// Package pkgplan implements subscription packages (套餐): periodic
// allowances + minimum-spend semantics applied on the billing path.
//
// A package has:
//   - Cycle: daily | monthly (allowance resets each cycle)
//   - FreeMinutes: included minutes at zero cost (used before real
//     billing; per VOS semantics, 60s minimums still apply)
//   - Discount: multiplier on rated cost after the allowance
//   - MinSpend: minimum spend per cycle - at cycle close the account
//     is topped up to the minimum (settlement job; here we expose the
//     shortfall calculation)
//
// The engine tracks per-account cycle usage in memory (production
// moves this to Redis with the same shape).
package pkgplan

import (
	"context"
	"github.com/fastvoip/gvos-core/internal/billingdomain/money"
	"fmt"
	"sync"
	"time"
)

// Cycle is the allowance reset period.
type Cycle int

const (
	Daily Cycle = iota
	Monthly
)

// Package is one plan definition.
type Package struct {
	ID           string
	Name         string
	Cycle        Cycle
	FreeMinutes  int     // included zero-cost minutes per cycle
	Discount     float64 // 1.0 = list price; 0.8 = 8折
	MinSpend     money.Amount // minimum spend per cycle
	GraceMinutes float64 // minutes allowed beyond balance (debt floor)
}

// Engine tracks which account holds which package plus usage counters.
type Engine struct {
	mu       sync.Mutex
	accounts map[string]assigned // accountID -> plan+usage
	plans    map[string]Package
	pr       *PkgR              // shared usage face (nil = local counters)
	now      func() time.Time
	cur      *time.Time // tests: shared clock handle
}

type assigned struct {
	pkg          Package
	freeUsedSecs float64
	spend        money.Amount
	cycleKey     string // "2026-10-07" or "2026-10" - reset on change
}

func New() *Engine {
	return &Engine{
		accounts: map[string]assigned{},
		plans:    map[string]Package{},
		now:      time.Now,
	}
}

// SetClock overrides the time source (tests).
func (e *Engine) SetClock(f func() time.Time) { e.now = f }

// SetPkgRedis wires the shared usage face (Phase 4). Local counters
// stay the fail-open fallback.
func (e *Engine) SetPkgRedis(p *PkgR) { e.pr = p }

// Define registers/updates a package definition.
func (e *Engine) Define(p Package) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if p.Discount <= 0 {
		p.Discount = 1
	}
	e.plans[p.ID] = p
}

// Assign binds an account to a package.
func (e *Engine) Assign(accountID, pkgID string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	p, ok := e.plans[pkgID]
	if !ok {
		return fmt.Errorf("package %q not defined", pkgID)
	}
	e.accounts[accountID] = assigned{pkg: p, cycleKey: e.cycleKeyOf(p.Cycle)}
	return nil
}

// Unassign removes the binding.
func (e *Engine) Unassign(accountID string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.accounts, accountID)
}

// Snapshot exports definitions and bindings — the configuration part
// of the engine (usage counters stay per-replica: runtime state, not
// config). Used for settings persistence and cfg-reload rebuilds.
type Snapshot struct {
	Packages    []Package         `json:"packages"`
	Assignments map[string]string `json:"assignments"` // accountID -> pkgID
}

func (e *Engine) Snapshot() Snapshot {
	e.mu.Lock()
	defer e.mu.Unlock()
	s := Snapshot{Assignments: map[string]string{}}
	for _, p := range e.plans {
		s.Packages = append(s.Packages, p)
	}
	for acc, a := range e.accounts {
		s.Assignments[acc] = a.pkg.ID
	}
	return s
}

// Restore rebuilds definitions and bindings from a snapshot. Usage
// counters start fresh — correct: usage is runtime state.
func (e *Engine) Restore(s Snapshot) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, p := range s.Packages {
		if p.Discount <= 0 {
			p.Discount = 1
		}
		e.plans[p.ID] = p
	}
	for acc, id := range s.Assignments {
		if p, ok := e.plans[id]; ok {
			e.accounts[acc] = assigned{pkg: p, cycleKey: e.cycleKeyOf(p.Cycle)}
		}
	}
}

// RateResult is the outcome of one call's package application.
type RateResult struct {
	// FreeSecs: seconds covered by the allowance in this call.
	FreeSecs float64
	// BillableSecs: seconds charged after the allowance.
	BillableSecs float64
	// Charge: amount to actually bill (billed secs at discounted price
	// - the caller already has the list-price math; we return the
	// multiplier to apply).
	Discount float64
}

// Apply runs package semantics for one call of billedSecs at
// ratedCost (list). It advances the cycle counters and returns the
// effective billing split. Calls on accounts without a package pass
// through unchanged.
func (e *Engine) Apply(accountID string, billedSecs int, ratedCost money.Amount) RateResult {
	e.mu.Lock()
	defer e.mu.Unlock()

	a, ok := e.accounts[accountID]
	if !ok {
		return RateResult{FreeSecs: 0, BillableSecs: float64(billedSecs), Discount: 1}
	}
	// Cycle rollover resets counters.
	key := e.cycleKeyOf(a.pkg.Cycle)
	if key != a.cycleKey {
		a.cycleKey = key
		a.freeUsedSecs = 0
		a.spend = 0
	}

	// Free-allowance headroom: the shared counter is authoritative when
	// wired (Phase 4) — the free minutes then aggregate every
	// replica's consumption. Fail-open to the local view on error.
	freeLeft := float64(a.pkg.FreeMinutes)*60 - a.freeUsedSecs
	if e.pr != nil {
		if n, err := e.pr.GetUsed(context.Background(), accountID, key); err == nil {
			freeLeft = float64(a.pkg.FreeMinutes)*60 - n
		} else {
			warn("used read", err)
		}
	}
	if freeLeft < 0 {
		freeLeft = 0
	}
	free := float64(billedSecs)
	if free > freeLeft {
		free = freeLeft
	}
	billable := float64(billedSecs) - free

	a.freeUsedSecs += free
	if e.pr != nil {
		if _, err := e.pr.AddUsed(context.Background(), accountID, key, free); err != nil {
			warn("used add", err)
		}
	}
	// Spend accrues the discounted share of the rated cost. The
	// ratio stays float (it is a proportion, not money); the product
	// returns to fixed point via the scaled numerator.
	share := int64(ratedCost) * int64(billable*1e9/float64(billedSecs)) / 1e9
	a.spend = a.spend.Add(money.Amount(share))
	if e.pr != nil {
		if _, err := e.pr.AddSpend(context.Background(), accountID, key, money.Amount(share)); err != nil {
			warn("spend add", err)
		}
	}
	e.accounts[accountID] = a

	return RateResult{FreeSecs: free, BillableSecs: billable, Discount: a.pkg.Discount}
}

// MinSpendShortfall returns how much the account is below its
// cycle minimum right now (0 when met; positive = top-up amount at
// cycle close).
func (e *Engine) MinSpendShortfall(accountID string) money.Amount {
	e.mu.Lock()
	defer e.mu.Unlock()
	a, ok := e.accounts[accountID]
	if !ok {
		return 0
	}
	key := e.cycleKeyOf(a.pkg.Cycle)
	if key != a.cycleKey {
		return a.pkg.MinSpend // fresh cycle, nothing spent yet
	}
	if d := a.pkg.MinSpend.Sub(a.spend); d > 0 {
		return d
	}
	return 0
}

// Usage returns the cycle usage snapshot (monitoring).
func (e *Engine) Usage(accountID string) (freeSecs float64, spend money.Amount, ok bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	a, ok := e.accounts[accountID]
	return a.freeUsedSecs, a.spend, ok
}

func (e *Engine) cycleKeyOf(c Cycle) string {
	t := e.now()
	if c == Monthly {
		return t.Format("2006-01")
	}
	return t.Format("2006-01-02")
}

func roundUp7(p float64) float64 {
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
