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
	"sync"
	"time"
)

// Entry is one user's registration state (latest wins per user; the
// event count accumulates since process start).
type Entry struct {
	User    string    `json:"user"`
	LastReg int64     `json:"last_reg"` // unix seconds
	Expires int64     `json:"expires"`  // unix seconds (0 when expired/unset)
	SrcIP   string    `json:"src_ip"`
	Events  int       `json:"events"` // total reg events seen (reg+expire)
	Online  bool      `json:"online"` // last event was a registration, not expiry
}

// Table holds the per-user entries.
type Table struct {
	mu   sync.RWMutex
	byUser map[string]*Entry
}

// New returns an empty table.
func New() *Table { return &Table{byUser: map[string]*Entry{}} }

// Apply folds one registration event. op is "reg" (a successful
// REGISTER) or "expire" (usrloc expiry hook); unknown ops count as
// expirations offline (defensive: an unrecognized event should not
// paint a user online).
func (t *Table) Apply(user, op string, expires int64, srcIP string, at time.Time) {
	if user == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
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
