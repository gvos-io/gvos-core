// Package regtable is the network-wide SIP registration mirror: the
// Kamailio signaling nodes publish gvos.reg.event on every REGISTER
// (and usrloc expiry hook), and this table folds those events into a
// per-user view — last registration time, expiry, source IP, and the
// rolling event count. The admin API exposes it for observability;
// the risk engine feeds reg_freq rules from the same stream.
//
// usrloc runs memory-only (db_mode=0) on the signaling nodes, so this
// mirror IS the control plane's only registration visibility.
package regtable

import (
	"context"
	"sync"
	"time"
)

// Entry is one user's registration state (latest wins per user; the
// event count accumulates since process start).
type Entry struct {
	User    string `json:"user"`
	LastReg int64  `json:"last_reg"` // unix seconds
	Expires int64  `json:"expires"`  // unix seconds (0 when expired/unset)
	SrcIP   string `json:"src_ip"`
	Events  int    `json:"events"` // total reg events seen (reg+expire)
	Online  bool   `json:"online"` // last event was a registration, not expiry
}

// Table holds the per-user entries.
type Table struct {
	mu     sync.RWMutex
	byUser map[string]*Entry
	rr     *RegR // redis mirror (nil = memory only)
}

// New returns an empty table.
func New() *Table { return &Table{byUser: map[string]*Entry{}} }

// SetRegRedis wires the persistent mirror (Phase 5): restarts keep
// the table, and the fallback lookup (ADR-0002) reads it.
func (t *Table) SetRegRedis(r *RegR) { t.rr = r }

// Apply folds one registration event. op is "reg" (a successful
// REGISTER) or "expire" (usrloc expiry hook); unknown ops count as
// expirations offline (defensive: an unrecognized event should not
// paint a user online).
func (t *Table) Apply(user, op string, expires int64, srcIP string, at time.Time) {
	if user == "" {
		return
	}
	t.mu.Lock()
	e := t.byUser[user]
	if e == nil {
		e = &Entry{User: user}
		t.byUser[user] = e
	}
	e.Events++
	e.LastReg = at.Unix()
	if op == "reg" {
		e.Online = true
		e.Expires = expires
		e.SrcIP = srcIP
	} else {
		e.Online = false
	}
	rr := t.rr
	t.mu.Unlock()
	// Redis round trip outside the table lock so REGISTER storms do
	// not serialize NATS callbacks behind one mutex.
	if rr != nil {
		rr.Apply(context.Background(), user, op, expires, srcIP, at.Unix())
	}
}

// LookupUser is the ADR-0002 fallback face: local mirror first, the
// redis persistence second (a restarted replica's local map is empty
// until events flow again — the redis hash bridges that window).
// Local entries past Expires are treated as offline and Redis is
// consulted so a missed expire event cannot pin a user Online forever.
func (t *Table) LookupUser(ctx context.Context, user string) (Entry, bool) {
	now := time.Now().Unix()
	t.mu.RLock()
	e, ok := t.byUser[user]
	var local Entry
	if ok {
		local = *e
	}
	t.mu.RUnlock()
	if ok {
		if local.Online && local.Expires > 0 && local.Expires < now {
			local.Online = false
			// Prefer Redis if it has a fresher registration.
			if t.rr != nil {
				if remote, rok := t.rr.Lookup(ctx, user); rok {
					return remote, true
				}
			}
			return local, true
		}
		return local, true
	}
	if t.rr != nil {
		return t.rr.Lookup(ctx, user)
	}
	return Entry{}, false
}

// Entries returns a snapshot sorted is NOT guaranteed (callers sort
// if they need a stable listing).
func (t *Table) Entries() []Entry {
	t.mu.RLock()
	defer t.mu.RUnlock()
	out := make([]Entry, 0, len(t.byUser))
	for _, e := range t.byUser {
		out = append(out, *e)
	}
	return out
}

// Online returns the online-entry count (monitoring hook).
func (t *Table) Online() int {
	t.mu.RLock()
	defer t.mu.RUnlock()
	n := 0
	for _, e := range t.byUser {
		if e.Online {
			n++
		}
	}
	return n
}

// Count returns the total tracked-user count.
func (t *Table) Count() int {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return len(t.byUser)
}
