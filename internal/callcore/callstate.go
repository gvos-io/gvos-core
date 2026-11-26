// Package callstate tracks in-progress calls and drives pre-answer
// locking plus interim deduction every interval, with forced teardown
// when the balance runs dry (the VOS "real-time billing, forced
// release" guarantee).
package callcore

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gvos-io/gvos-core/internal/billingdomain/account"
	"github.com/gvos-io/gvos-core/internal/billingdomain/billing"
	"github.com/gvos-io/gvos-core/internal/billingdomain/money"
	"github.com/gvos-io/gvos-core/internal/billingdomain/rate"
	"github.com/gvos-io/gvos-core/internal/shared/metrics"
)

// zombieSwept feeds the /metrics exposition (gvos_zombie_swept_total):
// a rising rate means BYE-loss at the edge, the input to the Interim
// starvation loop measured on the cloud bench.
var zombieSwept = metrics.Default.Counter("gvos_zombie_swept_total",
	"calls force-settled by the zombie sweeper (BYE never arrived)")

// noAnswerSwept counts calls that were never answered and lost their
// pre-answer lock to the ring timeout (the INVITE is long dead; the
// edge sent no end event). A rising rate means end events for failed
// INVITEs are missing at the edge.
var noAnswerSwept = metrics.Default.Counter("gvos_noanswer_swept_total",
	"never-answered calls released by the ring timeout (zero cost)")

// interimAuthorityFailOpen counts interim deductions skipped because
// the balance authority was unreachable: the call stays up and End
// settles the full cost. A rising rate is an authority outage.
var interimAuthorityFailOpen = metrics.Default.Counter("gvos_interim_authority_failopen_total",
	"interim deductions skipped because the balance authority was unavailable (call kept up)")

// balanceCommitFailed counts settlements whose refund/charge never
// reached the authority — money only a reconciliation can see.
var balanceCommitFailed = metrics.Default.Counter("gvos_balance_commit_failed_total",
	"balance commits (refund/charge) that failed on the authority")

// ErrCallIDInUse: a Start arrived for a Call-ID that already names a
// live call of another account or dialog. A retransmission of the SAME
// dialog is idempotent (Start returns nil); a different one is refused,
// because accepting it let a caller ride an existing Call-ID unbilled
// and untracked.
var ErrCallIDInUse = errors.New("call-id already in use by a live call")

// ErrUnknownCall: no live call and no settle meta under this Call-ID.
var ErrUnknownCall = errors.New("unknown call")

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
	LastActive  time.Time    // refreshed by Interim; sweeper uses this, not Start
	Lock        money.Amount // pre-answer reservation
	Accrued     money.Amount // deducted so far
	Tick        int          // interim deduction rounds completed
	Recorded    bool         // rtpengine recording engaged for this call
	// Direction of the billed leg: "out" (caller-billed relay, the
	// historical only mode) or "in" (callee-billed inbound DID). Set
	// once at Start; rides the CDR at settlement.
	Direction string
	// GatewayID names the outbound carrier leg when known (edge
	// dispatch choice, or the DID external second leg).
	GatewayID string
	// Answered: the 200 OK was seen (Answer, or adopted from the shared
	// meta). Interim bills only answered calls, from AnswerTime; a
	// ringing call accrues nothing.
	Answered   bool
	AnswerTime time.Time
	// FromTag is the SIP From-tag: with the Call-ID it names the
	// dialog, so a second dialog reusing a live Call-ID is refused.
	FromTag string

	metaCheck time.Time // unanswered: last shared-meta answer lookup
	closed    bool      // set by End under m.mu; Interim refunds if it races past End
}

// billStart is where talk time counts from: the answer when it is
// known, the INVITE otherwise (callers that never report an answer).
func (c *Call) billStart() time.Time {
	if !c.AnswerTime.IsZero() {
		return c.AnswerTime
	}
	return c.Start
}

// inflightCall is a Start in progress; the dialog identity guards its
// Call-ID the same way a registered call does.
type inflightCall struct {
	account string
	tag     string
}

// sameDialog reports whether a repeated Start names the call already
// registered under the Call-ID: same account, and the same dialog
// whenever both sides carry a From-tag (edges that predate the tag
// send none). Any other combination is a different call.
func sameDialog(callID, curAccount, curTag, accountID, fromTag string) error {
	if curAccount != accountID {
		return fmt.Errorf("%w %q: account %s vs %s", ErrCallIDInUse, callID, curAccount, accountID)
	}
	if curTag != "" && fromTag != "" && curTag != fromTag {
		return fmt.Errorf("%w %q: dialog %s vs %s", ErrCallIDInUse, callID, curTag, fromTag)
	}
	return nil
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
	GetBalance(ctx context.Context, id string) (money.Amount, error)
}

// Manager owns live calls and their billing lifecycle.
type Manager struct {
	mu          sync.Mutex
	calls       map[string]*Call
	inflight    map[string]inflightCall // Start in progress; blocks duplicate INVITE
	store       *account.Store
	bal         BalanceAuthority // nil = the in-memory store path
	adm         *AdmissionR      // nil = local-map admission (single replica)
	meta        *CallMetaStore   // nil = no cross-replica settle fallback
	charger     *billing.Charger
	teardown    TeardownFunc
	interval    time.Duration // interim deduction period
	ringTimeout time.Duration // unanswered calls lose their pre-answer lock after this
	now         func() time.Time
	lastRelease map[string]string // callID -> release reason set at teardown
}

// SetRingTimeout bounds how long a never-answered call keeps its
// pre-answer lock and its ledger slot. It must exceed the edge's
// worst-case INVITE lifetime (fr_inv_timer x failover hops), so the
// default is generous; end events for failed INVITEs normally release
// the lock long before it fires.
func (m *Manager) SetRingTimeout(d time.Duration) {
	if d > 0 {
		m.ringTimeout = d
	}
}

// SetBalanceAuthority swaps the money authority (Redis phase 1). The
// in-memory fallback stays for configuration reads.
func (m *Manager) SetBalanceAuthority(b BalanceAuthority) { m.bal = b }

// SetAdmissionLedger swaps the admission source (Redis phase 2):
// concurrent/CPS caps count the shared ledger instead of the local
// map, closing the ×N oversell. nil = local admission.
func (m *Manager) SetAdmissionLedger(a *AdmissionR) { m.adm = a }

// SetCallMetaStore wires the cross-replica settle fallback (Redis).
func (m *Manager) SetCallMetaStore(s *CallMetaStore) { m.meta = s }

func (m *Manager) putMeta(c *Call) {
	if m.meta == nil || c == nil {
		return
	}
	meta := CallMeta{
		AccountID: c.AccountID, Dest: c.Destination,
		StartUnix: c.Start.Unix(), StartMs: c.Start.UnixMilli(),
		Lock: c.Lock, Accrued: c.Accrued,
		Recorded:  c.Recorded,
		Direction: c.Direction, GatewayID: c.GatewayID,
	} // the answer is parked separately (Answer → TouchAnswer)
	if c.Rate != nil {
		meta.Prefix = c.Rate.Prefix
		meta.Price = c.Rate.Price
		meta.InitDur = c.Rate.InitDur
		meta.StepDur = c.Rate.StepDur
		meta.MinDur = c.Rate.MinDur
	}
	if err := m.meta.Put(context.Background(), c.ID, meta); err != nil {
		log.Printf("call meta put %s: %v", c.ID, err)
	}
}

// StartLedgerSweeper periodically drops Redis admission members older
// than ZombieTTL. Key-level EXPIRE refreshes on every new call, so
// without member pruning a busy account keeps crashed-replica
// callIDs forever. Returns a stop function.
func (m *Manager) StartLedgerSweeper(interval time.Duration) (stop func()) {
	if interval <= 0 {
		interval = time.Minute
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
				m.sweepLedger()
			}
		}
	}()
	return func() { close(done) }
}

func (m *Manager) sweepLedger() {
	if m.adm == nil {
		return
	}
	accs, err := m.store.List()
	if err != nil {
		return
	}
	ctx := context.Background()
	for _, a := range accs {
		zombies, err := m.adm.Zombies(ctx, a.ID)
		if err != nil || len(zombies) == 0 {
			continue
		}
		for _, cid := range zombies {
			if err := m.adm.Remove(ctx, a.ID, cid); err != nil {
				log.Printf("ledger zombie remove %s/%s: %v", a.ID, cid, err)
			}
		}
	}
}

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

// commit applies one settlement delta to the balance authority. A
// failure is loud (log + counter): it is money that moved in the CDR
// but not in the balance.
func (m *Manager) commit(id string, est, actual money.Amount) error {
	var err error
	if m.bal != nil {
		err = m.bal.CommitCtx(context.Background(), "", id, est, actual)
		if err != nil {
			log.Printf("balance commit failed account=%s est=%s actual=%s: %v", id, est, actual, err)
		}
	} else {
		err = m.store.Commit("", id, est, actual)
	}
	if err != nil {
		balanceCommitFailed.Inc(1)
	}
	return err
}

// Refund returns amt to the account balance (package discount settlement).
func (m *Manager) Refund(accountID string, amt money.Amount) error {
	if amt <= 0 {
		return nil
	}
	return m.commit(accountID, amt, 0)
}

// availableBalance reads the live authority (Redis when wired) for
// interim affordability checks. ok=false means the value could not be
// read: the caller skips the check rather than judging a caller
// against a stale number.
func (m *Manager) availableBalance(accountID string) (bal money.Amount, ok bool) {
	if m.bal != nil {
		b, err := m.bal.GetBalance(context.Background(), accountID)
		return b, err == nil
	}
	b, err := account.Balance(m.store, accountID)
	return b, err == nil
}

func NewManager(store *account.Store, teardown TeardownFunc, interval time.Duration) *Manager {
	if interval <= 0 {
		interval = 10 * time.Second
	}
	return &Manager{
		calls:       map[string]*Call{},
		inflight:    map[string]inflightCall{},
		store:       store,
		charger:     &billing.Charger{},
		teardown:    teardown,
		interval:    interval,
		ringTimeout: DefaultRingTimeout,
		now:         time.Now,
		lastRelease: map[string]string{},
	}
}

// DefaultRingTimeout bounds a never-answered call's pre-answer lock.
// The edge's INVITE transaction lives at most fr_inv_timer x failover
// hops (Kamailio defaults: 120s x 2), so 5 minutes can only fire on an
// INVITE that is already dead at the edge.
const DefaultRingTimeout = 5 * time.Minute

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

// interimWorkersBase is the parallel fan-out for one Interim pass.
// Under tens of thousands of live calls a single goroutine starves RPC;
// a small pool keeps Redis latency overlapping without unbounded
// goroutine storms. The pool SCALES with the snapshot size past the
// base (20k live calls × 10s cadence = ~2k Redis ops/s at steady
// state — the 20k ladder showed 8 workers falling behind at the
// tail): base + 1 per 2,500 live calls, capped to keep the goroutine
// storm bounded.
var interimWorkersBase = 8

// interimPoolSize returns the fan-out for a live-call snapshot.
func interimPoolSize(live int) int {
	n := interimWorkersBase + live/2500
	if n > 64 {
		return 64
	}
	if n < 1 {
		return 1
	}
	return n
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
	if len(ids) == 0 {
		return
	}
	n := interimPoolSize(len(ids))
	if len(ids) < n {
		n = len(ids)
	}
	ch := make(chan string, n)
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			for id := range ch {
				_, _ = m.Interim(id)
			}
		}()
	}
	for _, id := range ids {
		ch <- id
	}
	close(ch)
	wg.Wait()
}

// sweep settles one pass of timed-out calls. Timeout is measured from
// LastActive (refreshed by Interim), not Start — otherwise every call
// longer than the timeout is force-billed while SIP keeps talking.
// The release reason is "zombie-timeout" so the CDR trail shows what
// happened; teardown is dispatched so the edge can hang up.
func (m *Manager) sweep(timeout time.Duration) {
	now := m.now()
	type victim struct {
		id string
		d  int
	}
	var victims []victim
	var unanswered []string

	m.mu.Lock()
	for id, c := range m.calls {
		if !c.Answered {
			// Never answered: Interim never touched it, so the activity
			// clock says nothing — the INVITE's own age does. Past
			// ringTimeout the edge transaction is long over and its end
			// event is simply not coming (a failed INVITE had none).
			if m.ringTimeout > 0 && now.Sub(c.Start) > m.ringTimeout && m.lastRelease[id] == "" {
				unanswered = append(unanswered, id)
			}
			continue
		}
		active := c.LastActive
		if active.IsZero() {
			active = c.Start
		}
		if now.Sub(active) > timeout {
			victims = append(victims, victim{id, int(now.Sub(c.Start).Seconds())})
		}
	}
	m.mu.Unlock()

	for _, id := range unanswered {
		m.markRelease(id, ReleaseNoAnswer)
		if _, err := m.End(id, 0); err == nil {
			noAnswerSwept.Inc(1)
		}
	}

	for _, v := range victims {
		m.markRelease(v.id, "zombie-timeout")
		if m.teardown != nil {
			m.teardown(v.id, "zombie-timeout")
		}
		if _, err := m.End(v.id, v.d); err == nil {
			zombieSwept.Inc(1)
		}
	}
}

// Release reasons recorded on the CDR (Release field).
const (
	ReleaseNoAnswer = "no-answer" // never answered: zero cost, lock released
)

// Start is StartDialog for callers that have no SIP From-tag to offer
// (the HTTP demo face, the load generator).
func (m *Manager) Start(callID, accountID, dest string, e *rate.Entry, expectedSecs int, recorded bool) error {
	return m.StartDialog(callID, accountID, "", dest, e, expectedSecs, recorded)
}

// CallOpts carries per-call attributes that are not billing inputs:
// direction (out/in leg marking for the CDR) and the gateway leg.
type CallOpts struct {
	Direction string // "in" = callee-billed inbound DID; ""/"out" = default
	GatewayID string // outbound carrier leg when known
}

// callDirection normalizes the wire value onto the CDR vocabulary.
func callDirection(direction string) string {
	if direction == "inbound" {
		return "in"
	}
	return "out"
}

// StartDialogOpts is StartDialog with per-call attributes. The plain
// StartDialog stays for the historical outbound call sites. The opts
// ride the Call struct (persisted to shared meta for remote settle).
func (m *Manager) StartDialogOpts(callID, accountID, fromTag, dest string, e *rate.Entry, expectedSecs int, recorded bool, opts CallOpts) error {
	err := m.StartDialog(callID, accountID, fromTag, dest, e, expectedSecs, recorded)
	if err != nil {
		return err
	}
	m.mu.Lock()
	if c, ok := m.calls[callID]; ok {
		c.Direction = callDirection(opts.Direction)
		c.GatewayID = opts.GatewayID
	}
	var metaCopy *Call
	if c, ok := m.calls[callID]; ok {
		cp := *c
		metaCopy = &cp
	}
	m.mu.Unlock()
	if metaCopy != nil {
		m.putMeta(metaCopy)
	}
	return nil
}

// StartDialog reserves the estimate and registers the call, after
// checking the account's concurrent and CPS limits. fromTag (the SIP
// From-tag, "" when the caller has none) is kept beside the Call-ID as
// the dialog identity. Errors: account.ErrLimit (HTTP 429), insufficient
// balance (403), no account (404), ErrCallIDInUse (409),
// account.ErrUnavailable / ErrAdmissionUnavailable (503).
//
// Locking: every Redis round trip (the atomic admission script, the
// balance reserve) runs OUTSIDE m.mu — the critical section is only
// the map insert. With the ledger wired the admission script does
// check+register in one atomic round trip; if the reserve then fails,
// the registration is rolled back by EndChain (a failed rollback
// falls to the 15m zombie TTL — logged, never blocking the answer).
// Chain errors (account cycles) fail closed: a chain that cannot be
// walked cannot be billed.
func (m *Manager) StartDialog(callID, accountID, fromTag, dest string, e *rate.Entry, expectedSecs int, recorded bool) error {
	acc, err := m.store.Get(accountID)
	if err != nil {
		return err
	}

	// Retransmit / in-flight guard, keyed on the DIALOG. A repeat of
	// the same call is idempotent: a second reserve with no matching
	// Commit leaks money, and a failed second reserve + EndChain would
	// drop the live first call. A different account or From-tag under a
	// live Call-ID is another call and is refused — accepting it let
	// that call proceed unlocked and untracked (audit).
	m.mu.Lock()
	if c, ok := m.calls[callID]; ok {
		m.mu.Unlock()
		return sameDialog(callID, c.AccountID, c.FromTag, accountID, fromTag)
	}
	if fl, ok := m.inflight[callID]; ok {
		m.mu.Unlock()
		return sameDialog(callID, fl.account, fl.tag, accountID, fromTag)
	}
	m.inflight[callID] = inflightCall{account: accountID, tag: fromTag}
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		delete(m.inflight, callID)
		m.mu.Unlock()
	}()

	est := m.charger.EstimatedCost(e, expectedSecs)

	if m.adm != nil {
		// Shared ledger path (multi-replica). Walk the chain once: the
		// caps and the registration keys both derive from it. chain is
		// [leaf, parent, ..., root]; every capped member bounds this
		// start — concurrency as its sub-tree's live count, CPS as its
		// sub-tree's per-second window.
		chain, err := m.store.Chain(accountID)
		if err != nil {
			return err
		}
		caps := map[string]int{}
		cpsCaps := map[string]int{}
		keys := []string{accountID} // own key first; chain adds ancestors
		for _, a := range chain {
			if a.Concurrent > 0 {
				caps[a.ID] = a.Concurrent
			}
			if a.CPS > 0 {
				cpsCaps[a.ID] = a.CPS
			}
			if a.ID != accountID {
				keys = append(keys, a.ID) // dedup: Chain includes the account itself
			}
		}
		start := m.now()
		if err := m.adm.AdmitCallChain(context.Background(), keys, caps, cpsCaps, callID, start); err != nil {
			return err
		}
		if err := m.reserve(context.Background(), accountID, est); err != nil {
			// Roll the ledger registration + CPS slots back out; a failed
			// rollback self-heals via the zombie TTL (logged, never blocks).
			if err2 := m.adm.EndChain(context.Background(), callID, keys); err2 != nil {
				log.Printf("admission rollback failed for %s (zombie TTL will reclaim): %v", callID, err2)
			}
			if err2 := m.adm.ReleaseCPS(context.Background(), keys, cpsCaps, start); err2 != nil {
				log.Printf("cps rollback failed for %s: %v", callID, err2)
			}
			return err
		}
		c := &Call{
			ID:          callID,
			AccountID:   accountID,
			Destination: dest,
			Rate:        e,
			Start:       start,
			LastActive:  start,
			Lock:        est,
			Recorded:    recorded,
			FromTag:     fromTag,
		}
		m.mu.Lock()
		m.calls[callID] = c
		m.mu.Unlock()
		m.putMeta(c)
		return nil
	}

	// Local admission (single replica): the map walks are microsecond
	// scale, so they stay under the lock that already guards the map.
	// The clock is read once, before the lock.
	now := m.now()
	m.mu.Lock()
	defer m.mu.Unlock()

	if err := m.admitConcurrent(accountID, acc); err != nil {
		return err
	}
	if err := m.admitCPS(accountID, now); err != nil {
		return err
	}
	if err := m.reserve(context.Background(), accountID, est); err != nil {
		return err
	}
	start := now
	c := &Call{
		ID:          callID,
		AccountID:   accountID,
		Destination: dest,
		Rate:        e,
		Start:       start,
		LastActive:  start,
		Lock:        est,
		Recorded:    recorded,
		FromTag:     fromTag,
	}
	m.calls[callID] = c
	// putMeta needs the unlock for Redis; defer Unlock above — call after return via...
	// Local path holds m.mu; write meta after unlock by copying.
	metaCopy := *c
	go func() { m.putMeta(&metaCopy) }()
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

// maxChainDepth bounds a parent walk (the store already refuses cycles
// on a well-formed tree; this is the belt for a broken one).
const maxChainDepth = 64

// inSubtreeLocked reports whether accountID is rootID or descends
// from it. Caller holds m.mu.
func (m *Manager) inSubtreeLocked(rootID, accountID string) bool {
	cur := accountID
	for i := 0; i < maxChainDepth && cur != ""; i++ {
		if cur == rootID {
			return true
		}
		a, err := m.store.Get(cur)
		if err != nil {
			return false
		}
		cur = a.ParentID
	}
	return false
}

// admitCPS enforces the calls-per-second cap of the calling account
// (its own starts) and of every capped ancestor (the starts of its
// whole sub-tree). Ancestor CPS used to be silently ignored: only the
// leaf's own cap was checked. now is the start instant (the caller
// reads the clock once, outside m.mu). Caller holds m.mu.
func (m *Manager) admitCPS(accountID string, now time.Time) error {
	chain, err := m.store.Chain(accountID)
	if err != nil {
		return err
	}
	sec := now.Unix()
	for i, a := range chain {
		if a.CPS <= 0 {
			continue
		}
		started := 0
		for _, c := range m.calls {
			if c.Start.Unix() != sec {
				continue
			}
			if i == 0 {
				if c.AccountID == accountID {
					started++
				}
			} else if m.inSubtreeLocked(a.ID, c.AccountID) {
				started++
			}
		}
		if started >= a.CPS {
			detail := fmt.Sprintf("%d starts this second >= cap %d", started, a.CPS)
			if i > 0 {
				detail = "sub-tree " + detail
			}
			return &ErrLimit{AccountID: a.ID, Kind: "cps", Detail: detail}
		}
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
		return false, fmt.Errorf("%w %q", ErrUnknownCall, callID)
	}
	if !c.Answered {
		m.mu.Unlock()
		return m.interimUnanswered(c)
	}
	// Talk time counts from the answer, not the INVITE: ringing is not
	// billable, and the edge's end event measures the same way.
	elapsed := int(m.now().Sub(c.billStart()).Seconds())
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
	if c.closed {
		m.mu.Unlock()
		return false, nil
	}
	delta := cost.Sub(c.Accrued)
	m.mu.Unlock()
	if delta > 0 {
		if err := m.reserve(context.Background(), c.AccountID, delta); err != nil {
			if errors.Is(err, account.ErrUnavailable) || errors.Is(err, account.ErrNotSeeded) {
				// The authority is down; the caller is not broke. Keep
				// the call up — an outage used to tear down every live
				// call — and let End settle the full cost.
				interimAuthorityFailOpen.Inc(1)
				log.Printf("INTERIM reserve unavailable call=%s (call kept up): %v", callID, err)
				return true, nil
			}
			// Cannot afford the next interval: forced teardown.
			m.markRelease(callID, "balance-exhausted")
			if m.teardown != nil {
				m.teardown(callID, "balance-exhausted")
			}
			return false, nil
		}
		// Reserve already deducted; fold into Accrued. If End settled
		// while we held the reservation, refund immediately.
		m.mu.Lock()
		if c.closed {
			m.mu.Unlock()
			_ = m.commit(c.AccountID, delta, 0)
			return false, nil
		}
		c.Accrued = c.Accrued.Add(delta)
		c.LastActive = m.now()
		accrued := c.Accrued
		m.mu.Unlock()
		if m.meta != nil {
			_ = m.meta.TouchAccrued(context.Background(), callID, accrued)
		}
	} else if delta < 0 {
		m.mu.Lock()
		if c.closed {
			m.mu.Unlock()
			return false, nil
		}
		accountID := c.AccountID
		c.Accrued = c.Accrued.Add(delta)
		c.LastActive = m.now()
		accrued := c.Accrued
		m.mu.Unlock()
		_ = m.commit(accountID, -delta, 0) // refund excess (outside lock)
		if m.meta != nil {
			_ = m.meta.TouchAccrued(context.Background(), callID, accrued)
		}
	} else {
		m.mu.Lock()
		if !c.closed {
			c.LastActive = m.now()
		}
		m.mu.Unlock()
	}

	// Pre-check: can the account still afford one more tick? The shared
	// authority answers when wired. A postpaid account is never cut off
	// here (it invoices), and a prepaid one spends its credit line
	// (MaxDebt) before it is — the check used to ignore both, hanging
	// up postpaid callers holding zero balance after exactly one minute.
	acc, accErr := m.store.Get(c.AccountID)
	if accErr != nil || acc.Postpaid {
		return true, nil
	}
	m.mu.Lock()
	if c.closed {
		m.mu.Unlock()
		return false, nil
	}
	accrued := c.Accrued
	accountID := c.AccountID
	nextSecs := elapsed + int(m.interval.Seconds())
	_, nextCost, err := m.charger.ChargeFor(entry, nextSecs)
	m.mu.Unlock()
	if err != nil {
		return true, nil
	}
	need := nextCost.Sub(accrued)
	if need <= 0 {
		return true, nil
	}
	bal, ok := m.availableBalance(accountID)
	if !ok {
		return true, nil // authority unreadable: the next reserve decides
	}
	if bal.Add(acc.MaxDebt) < need {
		m.markRelease(callID, "balance-exhausted")
		if m.teardown != nil {
			m.teardown(callID, "balance-exhausted")
		}
		return false, nil
	}
	return true, nil
}

// Answer records the 200 OK: Interim bills the call from this moment.
// Idempotent. The answer is also parked on the shared meta so a peer
// that ends up holding the call can time it. A call this replica does
// not own is not an error to the edge — only the parked answer is
// written — but it is reported as ErrUnknownCall to the caller.
func (m *Manager) Answer(callID string) error {
	now := m.now()
	m.mu.Lock()
	c, ok := m.calls[callID]
	if ok && !c.Answered {
		c.Answered, c.AnswerTime = true, now
	}
	m.mu.Unlock()
	if m.meta != nil {
		if err := m.meta.TouchAnswer(context.Background(), callID, now.UnixMilli()); err != nil {
			log.Printf("call answer meta put %s: %v", callID, err)
		}
	}
	if !ok {
		return fmt.Errorf("%w %q", ErrUnknownCall, callID)
	}
	return nil
}

// AnswerTime reports the locally recorded answer instant.
func (m *Manager) AnswerTime(callID string) (time.Time, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.calls[callID]
	if !ok || !c.Answered {
		return time.Time{}, false
	}
	return c.AnswerTime, true
}

// AccountOf names the account of a live call (event scoping).
func (m *Manager) AccountOf(callID string) (string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.calls[callID]
	if !ok {
		return "", false
	}
	return c.AccountID, true
}

// A ringing call older than the delay asks the shared meta whether a
// peer recorded its answer (the HTTP tier behind an LB without
// Call-ID stickiness delivers answers to any replica), at most once
// per window. Only unanswered calls pay the round trip.
const (
	unansweredMetaDelay = 30 * time.Second
	unansweredMetaEvery = 30 * time.Second
)

// interimUnanswered is the Interim pass for a call with no answer on
// record. It charges nothing: the pre-answer lock waits for the end
// event or the ring-timeout sweep. Interim used to bill every call
// from its INVITE time and refresh its activity clock, so a failed
// INVITE — which the edge never ends — accrued until the balance ran
// out and the zombie sweeper could never reclaim it.
func (m *Manager) interimUnanswered(c *Call) (bool, error) {
	if m.meta == nil {
		return true, nil
	}
	now := m.now()
	if now.Sub(c.Start) < unansweredMetaDelay {
		return true, nil
	}
	m.mu.Lock()
	due := now.Sub(c.metaCheck) >= unansweredMetaEvery
	if due {
		c.metaCheck = now
	}
	m.mu.Unlock()
	if !due {
		return true, nil
	}
	ms, answered, err := m.meta.Answered(context.Background(), c.ID)
	if err != nil || !answered {
		return true, nil
	}
	at := time.UnixMilli(ms)
	if at.Before(c.Start) {
		at = c.Start
	}
	m.mu.Lock()
	if !c.closed && !c.Answered {
		c.Answered, c.AnswerTime = true, at
	}
	m.mu.Unlock()
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
// remainder of the pre-answer lock, and returns the CDR record. The
// ledger removal (a Redis round trip) runs outside m.mu: a BYE storm
// must not serialize on the map lock.
func (m *Manager) End(callID string, seconds int) (*CDR, error) {
	m.mu.Lock()
	c, ok := m.calls[callID]
	if !ok {
		m.mu.Unlock()
		// Cross-replica / owner-crash fallback: settle from Redis meta.
		if m.meta != nil {
			return m.endFromMeta(callID, seconds)
		}
		return nil, fmt.Errorf("%w %q", ErrUnknownCall, callID)
	}
	// Mark closed before deleting so a raced Interim refunds any
	// reservation it already took against this Call pointer.
	c.closed = true
	delete(m.calls, callID)
	answered := c.Answered
	accountID := c.AccountID
	entry := c.Rate
	lock := c.Lock
	accrued := c.Accrued
	start := c.Start
	recorded := c.Recorded
	dest := c.Destination
	direction := c.Direction
	gwID := c.GatewayID
	// Read-and-clear the release reason under the same lock: the old
	// post-unlock read raced driveInterim's snapshot pass (both touch
	// m.lastRelease unlocked) and crashed the whole process under
	// load ("fatal error: concurrent map read and map write",
	// observed at 200CPS with concurrent BYE storm + interim tick).
	release := m.lastRelease[callID]
	delete(m.lastRelease, callID)
	m.mu.Unlock()

	if seconds < 0 {
		seconds = 0
	}
	if m.meta != nil {
		claimed, err := m.meta.ClaimSettle(context.Background(), callID, start.UnixMilli())
		_ = m.meta.Delete(context.Background(), callID)
		if err == nil && !claimed {
			// A peer already remote-settled THIS call instance (the lock
			// is keyed on Call-ID + start, so a reused Call-ID on a new
			// call never trips it); the local map is stale.
			return nil, fmt.Errorf("%w %q", ErrUnknownCall, callID)
		}
	}
	if release == "" {
		release = "normal"
		if !answered && seconds <= 0 {
			release = ReleaseNoAnswer
		}
	}
	return m.finalizeEnd(callID, accountID, dest, direction, gwID, entry, start, lock, accrued, recorded, release, seconds)
}

// endFromMeta settles a call that is not in the local map using the
// Redis CallMeta snapshot (BYE landed on a different replica, or the
// owner died). Take() ensures only one replica wins the settle race.
func (m *Manager) endFromMeta(callID string, seconds int) (*CDR, error) {
	// Take the meta first (read + delete: one peer wins it), then
	// claim the settle lock for the call instance it names.
	meta, ok, err := m.meta.Take(context.Background(), callID)
	if err != nil {
		return nil, fmt.Errorf("call meta take %s: %w", callID, err)
	}
	if !ok {
		return nil, fmt.Errorf("%w %q", ErrUnknownCall, callID)
	}
	claimed, err := m.meta.ClaimSettle(context.Background(), callID, meta.StartMs)
	if err != nil {
		return nil, fmt.Errorf("call settle claim %s: %w", callID, err)
	}
	if !claimed {
		// The owner settled this instance locally; this replica's view
		// is the stale one.
		return nil, fmt.Errorf("%w %q", ErrUnknownCall, callID)
	}
	if seconds < 0 {
		seconds = 0
	}
	start := time.Unix(meta.StartUnix, 0)
	if seconds == 0 {
		// No usable duration arrived. Only an ANSWERED call may be
		// timed off the wall clock (since its answer): deriving since
		// the INVITE charged ring time on calls that never connected.
		seconds = 0
		if meta.AnswerMs > 0 {
			if d := int(m.now().Sub(time.UnixMilli(meta.AnswerMs)).Seconds()); d > 0 {
				seconds = d
			}
		}
	}
	log.Printf("REMOTE-SETTLE call=%s account=%s (cross-replica or owner miss)", callID, meta.AccountID)
	return m.finalizeEnd(callID, meta.AccountID, meta.Dest, meta.Direction, meta.GatewayID, meta.Entry(), start,
		meta.Lock, meta.Accrued, meta.Recorded, "remote-settle", seconds)
}

func (m *Manager) finalizeEnd(callID, accountID, dest, direction, gwID string, entry *rate.Entry, start time.Time,
	lock, accrued money.Amount, recorded bool, release string, seconds int) (*CDR, error) {

	if m.adm != nil {
		chain, _ := m.store.Chain(accountID)
		keys := []string{accountID}
		for _, a := range chain {
			if a.ID != accountID {
				keys = append(keys, a.ID)
			}
		}
		_ = m.adm.EndChain(context.Background(), callID, keys)
	}

	billedSecs, cost, err := m.charger.ChargeFor(entry, seconds)
	if release == "" {
		release = "normal"
	}
	if err != nil {
		billedSecs, cost, release = seconds, 0, "unratable"
	}
	var price money.Amount
	prefix := ""
	if entry != nil {
		price = entry.Price
		prefix = entry.Prefix
	}
	taken := lock.Add(accrued)
	if taken > cost {
		_ = m.commit(accountID, taken.Sub(cost), 0)
	} else if taken < cost {
		gap := cost.Sub(taken)
		if err := m.reserve(context.Background(), accountID, gap); err == nil {
			_ = m.commit(accountID, gap, gap)
		} else {
			cost = taken
			if release == "normal" || release == "remote-settle" {
				release = "undercollected"
			}
		}
	}

	return &CDR{
		CallID:     callID,
		AccountID:  accountID,
		Dest:       dest,
		Direction:  direction,
		GatewayID:  gwID,
		StartTime:  start,
		Duration:   seconds,
		BilledSecs: billedSecs,
		Cost:       cost,
		EstCost:    lock,
		RatePrefix: prefix,
		Price:      price,
		Release:    release,
		Recorded:   recorded,
	}, nil
}

// CDR is the billing record emitted at call end.
type CDR struct {
	CallID    string
	AccountID string
	Dest      string
	// Direction marks the billing side: "out" = caller-billed relay
	// (the only mode before DIDs); "in" = callee-billed inbound (the
	// DID-owning account locked the call, migration 013).
	Direction string
	// GatewayID names the outbound carrier leg ("out" calls: the edge's
	// gateway choice; "in" calls with destination_type=external: the
	// second-leg gateway). Empty for registered-user termination.
	GatewayID  string
	StartTime  time.Time
	AnswerTime time.Time // 200-answer moment (zero = unanswered); edge-measured epoch-ms
	Duration   int       // connected seconds
	BilledSecs int       // billed seconds
	Cost       money.Amount
	EstCost    money.Amount // pre-answer lock / risk day-cost estimate
	RatePrefix string
	Price      money.Amount // per-minute price applied
	Release    string       // normal | balance-exhausted | unratable
	PDDMs      int          // post-dial delay (INVITE -> first 200), Kamailio-measured
	SIPCode    int          // final SIP response on the release path
	Recorded   bool         // rtpengine recording was engaged (file named by call-id)
	FreeSecs   int          // package allowance seconds applied at settlement
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

// LiveByAccounts counts live calls whose account is in the given set
// (tenant-plane slice of the live count; the manager owns the map).
func (m *Manager) LiveByAccounts(set map[string]bool) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, c := range m.calls {
		if set[c.AccountID] {
			n++
		}
	}
	return n
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
