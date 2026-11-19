// Package callstate tracks in-progress calls and drives pre-answer
// locking plus interim deduction every interval, with forced teardown
// when the balance runs dry (the VOS "real-time billing, forced
// release" guarantee).
package callcore

import (
	"context"
	"strings"
	"strconv"
	"github.com/lixuanqun/gvos-core/internal/billingdomain/money"
	"fmt"
	"sync"
	"time"

	"github.com/lixuanqun/gvos-core/internal/billingdomain/account"
	"github.com/lixuanqun/gvos-core/internal/billingdomain/billing"
	"github.com/lixuanqun/gvos-core/internal/billingdomain/rate"
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
	Lock        money.Amount // pre-answer reservation
	Accrued     money.Amount // deducted so far
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

// BalanceAuthority is the money-moving seam: the in-memory Store
// implements it directly; the Redis authority (account.BalanceR)
// implements it with fail-closed semantics. callcore stays storage-
// agnostic.
type BalanceAuthority interface {
	ReserveCtx(ctx context.Context, id string, est money.Amount) (string, error)
	CommitCtx(ctx context.Context, token, id string, est, actual money.Amount) error
}

// Manager owns live calls and their billing lifecycle.
type Manager struct {
	mu          sync.Mutex
	calls       map[string]*Call
	store       *account.Store
	bal         BalanceAuthority // nil = the in-memory store path
	adm         *AdmissionR      // nil = local-map admission (single replica)
	charger     *billing.Charger
	teardown    TeardownFunc
	interval    time.Duration // interim deduction period
	now         func() time.Time
	lastRelease map[string]string // callID -> release reason set at teardown
}

// SetBalanceAuthority swaps the money authority (Redis phase 1). The
// in-memory fallback stays for configuration reads.
func (m *Manager) SetBalanceAuthority(b BalanceAuthority) { m.bal = b }

// SetAdmissionLedger swaps the admission source (Redis phase 2):
// concurrent/CPS caps count the shared ledger instead of the local
// map, closing the ×N oversell. nil = local admission.
func (m *Manager) SetAdmissionLedger(a *AdmissionR) { m.adm = a }

// reserve/commit route through the balance authority (fail-closed on
// start: an unavailable authority is an error the caller turns into a
// 403/503; interim/settle paths log and continue per the plan).
func (m *Manager) reserve(ctx context.Context, id string, est money.Amount) error {
	if m.bal != nil {
		_, err := m.bal.ReserveCtx(ctx, id, est)
		return err
	}
	_, err := m.store.Reserve(id, est)
	return err
}

func (m *Manager) commit(id string, est, actual money.Amount) error {
	if m.bal != nil {
		return m.bal.CommitCtx(context.Background(), "", id, est, actual)
	}
	return m.store.Commit("", id, est, actual)
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

// StartInterimDriver launches the periodic deduction loop: every
// interval it reconciles every live call's accrued cost against the
// account balance and forces teardown when the next tick is no longer
// affordable — this is what makes "real-time billing, forced release"
// actually fire on long calls (until RTPengine activity events replace
// the time slices, the clock is the activity source). Calls already
// torn down (release reason recorded, BYE pending) are skipped so a
// slow executor never sees duplicate dispatches. Returns a stop func.
func (m *Manager) StartInterimDriver(interval time.Duration) (stop func()) {
	if interval <= 0 {
		interval = m.interval
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
				m.driveInterim()
			}
		}
	}()
	return func() { close(done) }
}

// driveInterim runs one interim pass over all live calls that have not
// been torn down yet. Errors from raced-out calls (BYE settled between
// snapshot and Interim) are ignored: the call is simply gone.
func (m *Manager) driveInterim() {
	m.mu.Lock()
	ids := make([]string, 0, len(m.calls))
	for id := range m.calls {
		if m.lastRelease[id] == "" {
			ids = append(ids, id)
		}
	}
	m.mu.Unlock()
	for _, id := range ids {
		_, _ = m.Interim(id)
	}
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

	// Admission: with the Redis ledger wired (multi-replica), caps
	// count the shared truth; locally (single replica) the map walk
	// below. Ledger errors fail closed — admission is money-adjacent.
	if m.adm != nil {
		chain, _ := m.store.Chain(accountID)
		caps := map[string]int{}
		ids := []string{accountID} // own key first; chain adds ancestors
		for _, a := range append(chain, acc) {
			if a.Concurrent > 0 {
				caps[a.ID] = a.Concurrent
			}
			if a.ID != accountID {
				ids = append(ids, a.ID) // dedup: Chain includes the account itself
			}
		}
		if err := m.adm.AdmitConcurrent(context.Background(), ids, accountID, caps); err != nil {
			m.mu.Unlock()
			return err
		}
		if err := m.adm.AdmitCPS(context.Background(), accountID, m.now().Unix(), acc.CPS); err != nil {
			m.mu.Unlock()
			return err
		}
	} else {
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
	}

	if err := m.reserve(context.Background(), accountID, est); err != nil {
		m.mu.Unlock()
		return err
	}
	c := &Call{
		ID:          callID,
		AccountID:   accountID,
		Destination: dest,
		Rate:        e,
		Start:       m.now(),
		Lock:        est,
		Recorded:    recorded,
	}
	m.calls[callID] = c
	if m.adm != nil {
		chain, _ := m.store.Chain(accountID)
		keys := []string{accountID}
		for _, a := range chain {
			if a.ID != accountID {
				keys = append(keys, a.ID)
			}
		}
		if err := m.adm.StartChain(context.Background(), callID, keys, c.Start); err != nil {
			// Ledger registration failed after local admit: log and
			// continue — the local map still owns runtime detail, and
			// the sweeper's zombie scan tolerates stragglers.
			_ = err
		}
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

	// Cost so far vs already deducted: reconcile the difference. The
	// accrued fold-in happens under the lock (End races this loop).
	m.mu.Lock()
	delta := cost.Sub(c.Accrued)
	m.mu.Unlock()
	if delta > 0 {
		if err := m.reserve(context.Background(), c.AccountID, delta); err != nil {
			// Cannot afford the next interval: forced teardown.
			m.markRelease(callID, "balance-exhausted")
			if m.teardown != nil {
				m.teardown(callID, "balance-exhausted")
			}
			return false, nil
		}
		// Reserve succeeded: fold the delta into accrued (release trick:
		// commit the reservation immediately at actual cost delta).
		_ = m.commit(c.AccountID, delta, delta)
		m.mu.Lock()
		c.Accrued = c.Accrued.Add(delta)
		m.mu.Unlock()
	} else if delta < 0 {
		_ = m.commit(c.AccountID, -delta, 0) // refund excess
		m.mu.Lock()
		c.Accrued = c.Accrued.Add(delta)
		m.mu.Unlock()
	}

	// Pre-check: can the account still afford one more tick? The
	// accrued read and the balance check both go under the lock (End
	// races this loop on Accrued).
	m.mu.Lock()
	nextSecs := elapsed + int(m.interval.Seconds())
	_, nextCost, err := m.charger.ChargeFor(entry, nextSecs)
	if err != nil {
		m.mu.Unlock()
		return true, nil
	}
	bal, _ := account.Balance(m.store, c.AccountID)
	exhausted := bal < nextCost.Sub(c.Accrued)
	m.mu.Unlock()
	if exhausted {
		m.markRelease(callID, "balance-exhausted")
		if m.teardown != nil {
			m.teardown(callID, "balance-exhausted")
		}
		return false, nil
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
	if m.adm != nil {
		chain, _ := m.store.Chain(c.AccountID)
		keys := []string{c.AccountID}
		for _, a := range chain {
			if a.ID != c.AccountID {
				keys = append(keys, a.ID)
			}
		}
		_ = m.adm.EndChain(context.Background(), callID, keys)
	}
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
	var price money.Amount
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
	taken := lock.Add(accrued)
	if taken > cost {
		_ = m.commit(c.AccountID, taken.Sub(cost), 0)
	} else if taken < cost {
		// Under-collection (call ran longer than locked): charge the
		// gap. Exhausted prepaid accounts still get the CDR at full
		// cost — the debt shows in the next call's 403.
		if err := m.reserve(context.Background(), c.AccountID, cost.Sub(taken)); err == nil {
			_ = m.commit(c.AccountID, cost.Sub(taken), cost.Sub(taken))
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
	Cost       money.Amount
	RatePrefix string
	Price      money.Amount // per-minute price applied
	Release    string  // normal | balance-exhausted | unratable
	PDDMs      int     // post-dial delay (INVITE -> first 200), Kamailio-measured
	SIPCode    int     // final SIP response on the release path
	Recorded   bool    // rtpengine recording was engaged (file named by call-id)
	FreeSecs   int     // package allowance seconds applied at settlement
	// QoS from rtpengine's $rtpstat (U6: parsed at the CDRR consumer,
	// columns already exist in the cdr table).
	Mos        float64
	JitterMs   float64
	PacketLoss float64
}

// ParseRTPStat extracts MOS/jitter/packet-loss from rtpengine's
// $rtpstat text (comma/space-separated key=value pairs, e.g.
// "packet loss=1.2%, jitter=3 ms, round trip time=15 ms, MOS=4.2").
// Unknown/absent keys parse to 0.
func ParseRTPStat(text string) (mos, jitterMs, packetLoss float64) {
	for _, kv := range strings.FieldsFunc(text, func(r rune) bool { return r == ',' || r == ';' || r == '|' }) {
		kv = strings.TrimSpace(kv)
		eq := strings.Index(kv, "=")
		if eq < 0 {
			continue
		}
		key := strings.ToLower(strings.TrimSpace(kv[:eq]))
		val := strings.TrimSpace(kv[eq+1:])
		numEnd := 0
		for numEnd < len(val) && (val[numEnd] >= '0' && val[numEnd] <= '9' || val[numEnd] == '.') {
			numEnd++
		}
		f, err := strconv.ParseFloat(val[:numEnd], 64)
		if err != nil {
			continue
		}
		switch {
		case strings.Contains(key, "mos"):
			mos = f
		case strings.Contains(key, "jitter"):
			jitterMs = f
		case strings.Contains(key, "loss"):
			packetLoss = f
		}
	}
	return
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
