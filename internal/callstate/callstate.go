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
	Recorded    bool    // rtpengine recording engaged for this call
}

// ErrLimit encapsulates a rejection due to account limits so the HTTP
// layer can map it to 429 (vs 403 for balance).
type ErrLimit struct {
	AccountID string
	Kind      string // "concurrent" | "cps"
	Detail    string
}

func (e *ErrLimit) Error() string {
	return fmt.Sprintf("account %q exceeded %s limit: %s", e.AccountID, e.Kind, e.Detail)
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

// StartSweeper launches the zombie-call sweeper: calls that never saw
// a BYE (caller vanished, gateway dropped) are force-settled after
// timeout, billing the elapsed duration so the pre-answer lock stops
// leaking. Returns a stop function.
func (m *Manager) StartSweeper(timeout time.Duration, interval time.Duration) (stop func()) {
	if interval <= 0 {
		interval = 30 * time.Second
	}
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}
	done := make(chan struct{})
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				m.sweep(timeout)
			}
		}
	}()
	return func() { close(done) }
}

// sweep settles one pass of timed-out calls. The release reason is
// "zombie-timeout" so the CDR trail shows what happened.
func (m *Manager) sweep(timeout time.Duration) {
	now := m.now()
	type victim struct {
		id string
		d  int
	}
	var victims []victim

	m.mu.Lock()
	for id, c := range m.calls {
		if now.Sub(c.Start) > timeout {
			victims = append(victims, victim{id, int(now.Sub(c.Start).Seconds())})
		}
	}
	m.mu.Unlock()

	for _, v := range victims {
		m.markRelease(v.id, "zombie-timeout")
		if _, err := m.End(v.id, v.d); err == nil {
			// settled fine
		}
	}
}

// Start reserves the estimate and registers the call, after checking
// the account's concurrent and CPS limits. Errors: account.ErrLimit
// (HTTP 429), insufficient balance (403), no account (404).
func (m *Manager) Start(callID, accountID, dest string, e *rate.Entry, expectedSecs int, recorded bool) error {
	acc, err := m.store.Get(accountID)
	if err != nil {
		return err
	}

	est := m.charger.EstimatedCost(e, expectedSecs)

	m.mu.Lock()

	// Concurrent limit: live calls of this account (chain ancestors
	// each carry their own limit — the whole chain must admit it).
	if err := m.admitConcurrent(accountID, acc); err != nil {
		m.mu.Unlock()
		return err
	}
	// CPS limit: start events in the current second for this account.
	if err := m.admitCPS(accountID, acc); err != nil {
		m.mu.Unlock()
		return err
	}

	if _, err := m.store.Reserve(accountID, est); err != nil {
		m.mu.Unlock()
		return err
	}
	m.calls[callID] = &Call{
		ID:          callID,
		AccountID:   accountID,
		Destination: dest,
		Rate:        e,
		Start:       m.now(),
		Lock:        est,
		Recorded:    recorded,
	}
	m.mu.Unlock()
	return nil
}

// admitConcurrent enforces the account's own concurrent limit and, via
// the chain walk, the limits of every ancestor (agents cap their
// sub-tree). Caller holds m.mu.
func (m *Manager) admitConcurrent(accountID string, acc *account.Account) error {
	live := m.liveCountLocked(accountID)
	if acc.Concurrent > 0 && live >= acc.Concurrent {
		return &ErrLimit{AccountID: accountID, Kind: "concurrent",
			Detail: fmt.Sprintf("%d live >= cap %d", live, acc.Concurrent)}
	}
	// Ancestors: count the whole sub-tree under each limit.
	chain, err := m.store.Chain(accountID)
	if err != nil {
		return err
	}
	for _, a := range chain {
		if a.Concurrent <= 0 || a.ID == accountID {
			continue
		}
		live := m.subtreeLiveLocked(a.ID)
		if live >= a.Concurrent {
			return &ErrLimit{AccountID: a.ID, Kind: "concurrent",
				Detail: fmt.Sprintf("sub-tree live %d >= cap %d", live, a.Concurrent)}
		}
	}
	return nil
}

// admitCPS enforces the account's calls-per-second cap (own calls only;
// ancestor CPS follows from their direct sub-agents' own caps).
// Caller holds m.mu.
func (m *Manager) admitCPS(accountID string, acc *account.Account) error {
	if acc.CPS <= 0 {
		return nil
	}
	now := m.now()
	startedInSecond := 0
	for _, c := range m.calls {
		if c.AccountID == accountID && c.Start.Unix() == now.Unix() {
			startedInSecond++
		}
	}
	if startedInSecond >= acc.CPS {
		return &ErrLimit{AccountID: accountID, Kind: "cps",
			Detail: fmt.Sprintf("%d starts this second >= cap %d", startedInSecond, acc.CPS)}
	}
	return nil
}

// liveCountLocked counts live calls of one exact account.
func (m *Manager) liveCountLocked(accountID string) int {
	n := 0
	for _, c := range m.calls {
		if c.AccountID == accountID {
			n++
		}
	}
	return n
}

// subtreeLiveLocked counts live calls of the account and every
// descendant. The account tree is small (chain walk per start is
// acceptable at spike scale; production moves this to a counter).
func (m *Manager) subtreeLiveLocked(rootID string) int {
	desc := map[string]bool{rootID: true}
	// Walk all accounts once, marking those whose chain passes rootID.
	all, _ := m.store.List()
	byID := map[string]*account.Account{}
	for _, a := range all {
		byID[a.ID] = a
	}
	for _, a := range all {
		cur := a.ID
		for cur != "" {
			if cur == rootID {
				desc[a.ID] = true
				break
			}
			p := byID[cur]
			if p == nil {
				break
			}
			cur = p.ParentID
		}
	}
	n := 0
	for _, c := range m.calls {
		if desc[c.AccountID] {
			n++
		}
	}
	return n
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
	recorded := c.Recorded
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
		Recorded:   recorded,
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
	PDDMs      int     // post-dial delay (INVITE -> first 200), Kamailio-measured
	SIPCode    int     // final SIP response on the release path
	Recorded   bool    // rtpengine recording was engaged (file named by call-id)
}

// Live returns the count of live calls (monitoring hook).
func (m *Manager) Live() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.calls)
}

// Info returns the start time of a live call (for duration derivation
// when the BYE event carries no duration).
func (m *Manager) Info(callID string) (time.Time, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.calls[callID]
	if !ok {
		return time.Time{}, false
	}
	return c.Start, true
}
