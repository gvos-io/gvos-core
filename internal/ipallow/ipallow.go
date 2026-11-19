// Package ipallow is the source-IP allowlist: ordered CIDR rules with
// explicit allow/deny actions and a default-deny fallback (an empty
// list rejects everything — it is an *allow* list, not a "log-only"
// list). The gv-core replicas hold it in memory (NATS broadcasts keep
// them in sync); the Kamailio signaling edge consumes the same rule
// set via the snapshot endpoint into an htable, so admission control
// on the hot path never touches a database.
//
// Decision order: deny rules win over allow rules on overlap; a
// source matching nothing is rejected (default-deny). The matched
// rule ID travels back to the caller for the 403 audit trail.
package ipallow

import (
	"fmt"
	"net"
	"sync"
)

// Action decides what happens when a source IP matches a rule.
type Action string

const (
	Allow Action = "allow"
	Deny  Action = "deny"
)

// Rule is one allowlist entry. CIDR is the source network in CIDR
// notation (IPv4 or IPv6); ID is the operator-facing handle used for
// removal and audit.
type Rule struct {
	ID     string `json:"id"`
	CIDR   string `json:"cidr"`
	Action Action `json:"action"`
	Note   string `json:"note"`
}

// Validate checks one rule: non-empty ID, parseable CIDR, valid action.
func Validate(r Rule) error {
	if r.ID == "" {
		return fmt.Errorf("rule id required")
	}
	if _, _, err := net.ParseCIDR(r.CIDR); err != nil {
		return fmt.Errorf("rule %q: bad CIDR %q: %v", r.ID, r.CIDR, err)
	}
	if r.Action != Allow && r.Action != Deny {
		return fmt.Errorf("rule %q: action must be allow|deny, got %q", r.ID, r.Action)
	}
	return nil
}

// compiled pairs a rule with its parsed network.
type compiled struct {
	rule Rule
	net  *net.IPNet
}

// List is the in-memory rule set. Reads are lock-free after the
// snapshot copy; writes replace the whole set atomically (the rule
// count is small — operators manage dozens, not millions).
type List struct {
	mu       sync.RWMutex
	rules    []Rule
	nets     []compiled
	denyNets []compiled
}

// New returns an empty (default-deny) list.
func New() *List { return &List{} }

// Reload atomically replaces the rule set. Invalid rules are skipped
// (counted in the return) — validation belongs to the API layer,
// which rejects bad input before it ever reaches here; this is the
// belt to that suspenders for broadcast-restored snapshots.
func (l *List) Reload(rules []Rule) (int, error) {
	valid := make([]Rule, 0, len(rules))
	var nets, denyNets []compiled
	for _, r := range rules {
		if err := Validate(r); err != nil {
			continue
		}
		_, ipnet, err := net.ParseCIDR(r.CIDR)
		if err != nil {
			continue
		}
		valid = append(valid, r)
		c := compiled{rule: r, net: ipnet}
		if r.Action == Deny {
			denyNets = append(denyNets, c)
		} else {
			nets = append(nets, c)
		}
	}
	l.mu.Lock()
	l.rules = valid
	l.nets = nets
	l.denyNets = denyNets
	l.mu.Unlock()
	return len(rules) - len(valid), nil
}

// Rules returns a copy of the current rule set (GET API / snapshot
// endpoint / persistence all need the full list).
func (l *List) Rules() []Rule {
	l.mu.RLock()
	defer l.mu.RUnlock()
	out := make([]Rule, len(l.rules))
	copy(out, l.rules)
	return out
}

// Count returns the number of active rules.
func (l *List) Count() int {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return len(l.rules)
}

// Check decides whether ip may connect. matched is the ID of the rule
// that decided ("" only when nothing matched, which is also a reject
// — default-deny).
func (l *List) Check(ip net.IP) (allowed bool, matched string) {
	if ip == nil {
		return false, ""
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	for _, c := range l.denyNets {
		if c.net.Contains(ip) {
			return false, c.rule.ID
		}
	}
	for _, c := range l.nets {
		if c.net.Contains(ip) {
			return true, c.rule.ID
		}
	}
	return false, "" // default-deny
}
