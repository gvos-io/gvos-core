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
	"github.com/gvos-io/gvos-core/internal/billingdomain/money"
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

// Mode controls Redis-unavailable behaviour for free-minute claims.
type Mode string

const (
	// ModeOpen (default legacy): Redis claim errors fall back to the
	// local free-minute counter (may oversell free minutes under
	// multi-replica Redis outages).
	ModeOpen Mode = "open"
	// ModeListPrice: Redis claim errors charge list price (zero free
	// seconds) — prefer under-discounting over free-minute oversell.
	ModeListPrice Mode = "list_price"
)

// Engine tracks which account holds which package plus usage counters.
type Engine struct {
	mu       sync.Mutex
	accounts map[string]assigned // accountID -> plan+usage
	plans    map[string]Package
	pr       *PkgR // shared usage face (nil = local counters)
	mode     Mode
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
		mode:     ModeListPrice,
		now:      time.Now,
	}
}

// SetClock overrides the time source (tests).
func (e *Engine) SetClock(f func() time.Time) { e.now = f }

// SetMode selects open or list_price Redis-failure behaviour.
func (e *Engine) SetMode(m Mode) {
	if m == ModeOpen {
		e.mode = ModeOpen
		return
	}
	e.mode = ModeListPrice
}

// SetPkgRedis wires the shared usage face (Phase 4). Local counters
// stay the fail-open fallback unless ModeListPrice (default).
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
// through unchanged. Redis round trips run outside e.mu so callEnd
// paths do not serialize on the package lock.
func (e *Engine) Apply(accountID string, billedSecs int, ratedCost money.Amount) RateResult {
	e.mu.Lock()
	a, ok := e.accounts[accountID]
	if !ok {
		e.mu.Unlock()
		return RateResult{FreeSecs: 0, BillableSecs: float64(billedSecs), Discount: 1}
	}
	key := e.cycleKeyOf(a.pkg.Cycle)
	if key != a.cycleKey {
		a.cycleKey = key
		a.freeUsedSecs = 0
		a.spend = 0
		e.accounts[accountID] = a
	}
	maxFree := float64(a.pkg.FreeMinutes) * 60
	localUsed := a.freeUsedSecs
	discount := a.pkg.Discount
	pr := e.pr
	mode := e.mode
	e.mu.Unlock()

	want := float64(billedSecs)
	var free float64
	if pr != nil {
		claimed, err := pr.ClaimUsed(context.Background(), accountID, key, want, maxFree)
		if err != nil {
			warn("used claim", err)
			if mode == ModeListPrice {
				free = 0 // charge list price rather than oversell free minutes
			} else {
				freeLeft := maxFree - localUsed
				if freeLeft < 0 {
					freeLeft = 0
				}
				free = want
				if free > freeLeft {
					free = freeLeft
				}
			}
		} else {
			free = claimed
		}
	} else {
		freeLeft := maxFree - localUsed
		if freeLeft < 0 {
			freeLeft = 0
		}
		free = want
		if free > freeLeft {
			free = freeLeft
		}
	}
	billable := float64(billedSecs) - free

	var share int64
	if billedSecs > 0 {
		share = int64(ratedCost) * int64(billable*1e9/float64(billedSecs)) / 1e9
		if discount > 0 && discount != 1 {
			share = int64(float64(share) * discount)
		}
	}
	amt := money.Amount(share)
	if pr != nil && amt != 0 {
		if _, err := pr.AddSpend(context.Background(), accountID, key, amt); err != nil {
			warn("spend add", err)
		}
	}

	e.mu.Lock()
	a, ok = e.accounts[accountID]
	if ok {
		if a.cycleKey == key {
			a.freeUsedSecs += free
			a.spend = a.spend.Add(amt)
		}
		e.accounts[accountID] = a
	}
	e.mu.Unlock()

	return RateResult{FreeSecs: free, BillableSecs: billable, Discount: discount}
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
