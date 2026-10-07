// Package callstate tracks in-progress calls and drives pre-answer
// locking plus interim deduction every interval, with forced teardown
// when the balance runs dry (the VOS "real-time billing, forced
// release" guarantee).
package callstate

import (
	"fmt"
	"sync"
	"time"

	"github.com/gvos/gvos-core/internal/account"
	"github.com/gvos/gvos-core/internal/billing"
	"github.com/gvos/gvos-core/internal/rate"
)

// TeardownFunc is invoked when a live call must be torn down (balance
// exhausted). In production this sends CANCEL/BYE via Kamailio.
type TeardownFunc func(callID string, reason string)

// Call is one live or just-finished call.
type Call struct {
	ID          string
	AccountID   string
	Destination string
	Rate        *rate.Entry
	Start       time.Time
	Lock        float64 // pre-answer reservation
	Accrued     float64 // deducted so far
	Tick        int     // interim deduction rounds completed
}

// Manager owns live calls and their billing lifecycle.
type Manager struct {
	mu          sync.Mutex
	calls       map[string]*Call
	store       *account.Store
	charger     *billing.Charger
	teardown    TeardownFunc
	interval    time.Duration // interim deduction period
	now         func() time.Time
	lastRelease map[string]string // callID -> release reason set at teardown
}

func NewManager(store *account.Store, teardown TeardownFunc, interval time.Duration) *Manager {
	if interval <= 0 {
		interval = 10 * time.Second
	}
	return &Manager{
		calls:       map[string]*Call{},
		store:       store,
		charger:     &billing.Charger{},
		teardown:    teardown,
		interval:    interval,
		now:         time.Now,
		lastRelease: map[string]string{},
	}
}

// SetClock overrides the time source (tests).
func (m *Manager) SetClock(f func() time.Time) { m.now = f }

// Start reserves the estimate and registers the call. It returns an
// error when the account cannot afford the estimate (caller answers
// with 403).
func (m *Manager) Start(callID, accountID, dest string, e *rate.Entry, expectedSecs int) error {
	est := m.charger.EstimatedCost(e, expectedSecs)
	if _, err := m.store.Reserve(accountID, est); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls[callID] = &Call{
		ID:          callID,
		AccountID:   accountID,
		Destination: dest,
		Rate:        e,
		Start:       m.now(),
		Lock:        est,
	}
	return nil
}

// Interim runs the periodic deduction for one call: it charges the
// elapsed cost so far, refunds the difference against the accrued
// amount, and triggers teardown when the remaining balance can no
// longer cover one more tick. Returns true if the call is still alive.
func (m *Manager) Interim(callID string) (bool, error) {
	m.mu.Lock()
	c, ok := m.calls[callID]
	if !ok {
		m.mu.Unlock()
		return false, fmt.Errorf("unknown call %q", callID)
	}
	elapsed := int(m.now().Sub(c.Start).Seconds())
	c.Tick++
	entry := c.Rate
	m.mu.Unlock()

	_, cost, err := m.charger.ChargeFor(entry, elapsed)
	if err != nil {
		return true, err
	}

	// Cost so far vs already deducted: reconcile the difference.
	delta := cost - c.Accrued
	if delta > 0 {
		if _, err := m.store.Reserve(c.AccountID, delta); err != nil {
			// Cannot afford the next interval: forced teardown.
			m.markRelease(callID, "balance-exhausted")
			if m.teardown != nil {
				m.teardown(callID, "balance-exhausted")
			}
			return false, nil
		}
		// Reserve succeeded: fold the delta into accrued (release trick:
		// commit the reservation immediately at actual cost delta).
		_ = m.store.Commit("", c.AccountID, delta, delta)
		c.Accrued += delta
	} else if delta < 0 {
		_ = m.store.Commit("", c.AccountID, -delta, 0) // refund excess
		c.Accrued += delta
	}

	// Pre-check: can the account still afford one more tick?
	nextSecs := elapsed + int(m.interval.Seconds())
	_, nextCost, err := m.charger.ChargeFor(entry, nextSecs)
	if err == nil {
		bal, _ := account.Balance(m.store, c.AccountID)
		if bal < nextCost-c.Accrued {
			m.markRelease(callID, "balance-exhausted")
			if m.teardown != nil {
				m.teardown(callID, "balance-exhausted")
			}
			return false, nil
		}
	}
	return true, nil
}

// markRelease remembers why a call is being torn down so the CDR can
// carry it (End reads and clears it).
func (m *Manager) markRelease(callID, reason string) {
	m.mu.Lock()
	m.lastRelease[callID] = reason
	m.mu.Unlock()
}

// End finalizes the call: charges the real duration, releases the
// remainder of the pre-answer lock, and returns the CDR record.
func (m *Manager) End(callID string, seconds int) (*CDR, error) {
	m.mu.Lock()
	c, ok := m.calls[callID]
	if !ok {
		m.mu.Unlock()
		return nil, fmt.Errorf("unknown call %q", callID)
	}
	delete(m.calls, callID)
	entry := c.Rate
	lock := c.Lock
	accrued := c.Accrued
	start := c.Start
	m.mu.Unlock()

	billedSecs, cost, err := m.charger.ChargeFor(entry, seconds)
	release := "normal"
	if err != nil {
		// Unratable connected call: treat as zero cost but keep CDR.
		billedSecs, cost, release = seconds, 0, "unratable"
		err = nil
	}
	price := 0.0
	if entry != nil {
		price = entry.Price
	}
	// If the manager tore the call down for balance exhaustion, mark it.
	if m.lastRelease[callID] != "" {
		release = m.lastRelease[callID]
		delete(m.lastRelease, callID)
	}
	// Final settle against what was already taken (pre-answer lock plus
	// interim accruals): top up when the call cost more, refund when it
	// cost less. Delta is signed; Commit applies +delta as a refund and
	// a negative est means extra charge via Reserve.
	taken := lock + accrued
	if taken > cost {
		_ = m.store.Commit("", c.AccountID, taken-cost, 0)
	} else if taken < cost {
		// Under-collection (call ran longer than locked): charge the
		// gap. Exhausted prepaid accounts still get the CDR at full
		// cost — the debt shows in the next call's 403.
		if _, err := m.store.Reserve(c.AccountID, cost-taken); err == nil {
			_ = m.store.Commit("", c.AccountID, cost-taken, cost-taken)
		}
	}

	return &CDR{
		CallID:     callID,
		AccountID:  c.AccountID,
		Dest:       c.Destination,
		StartTime:  start,
		Duration:   seconds,
		BilledSecs: billedSecs,
		Cost:       cost,
		RatePrefix: entry.Prefix,
		Price:      price,
		Release:    release,
	}, nil
}

// CDR is the billing record emitted at call end.
type CDR struct {
	CallID     string
	AccountID  string
	Dest       string
	StartTime  time.Time
	Duration   int // connected seconds
	BilledSecs int // billed seconds
	Cost       float64
	RatePrefix string
	Price      float64 // per-minute price applied
	Release    string  // normal | balance-exhausted | unratable
}

// Live returns the count of live calls (monitoring hook).
func (m *Manager) Live() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.calls)
}
