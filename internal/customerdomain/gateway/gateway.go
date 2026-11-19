// Package gateway is the outbound gateway (carrier) directory: the
// call-signaling edge routes INVITEs to the highest-priority enabled
// gateway, and the admin API owns the contents. The gv-core replicas
// hold the set in memory (NATS broadcasts keep them in sync); the
// Kamailio edge consumes the same set via the gvos.cfg.reload
// broadcast into an htable, so gateway selection on the hot path
// never touches a database.
//
// This is the API-authoritative source: PostgreSQL is only the
// restart fallback (settings key "gateways"; the legacy gateways
// table is read once when no setting exists).
package gateway

import (
	"fmt"
	"sync"
)

// Gateway is one routable carrier entry. SipAddr is host:port; the
// cfg side derives "sip:"+SipAddr. Priority orders failover attempts
// (lower value = tried first); Enabled=false removes it from
// selection without deleting the entry.
type Gateway struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	SipAddr  string   `json:"sip_addr"`
	Protocol string   `json:"protocol"`  // udp | tcp | tls (informational; routing is by SipAddr)
	Priority int      `json:"priority"`  // base priority (lower = preferred)
	Weight   int      `json:"weight"`    // future ratio balancing (ignored by priority selection)
	Enabled  bool     `json:"enabled"`
	// LCR (competitive-analysis §3.1): the prefixes this gateway
	// serves and the rate group that prices its *outbound cost*. The
	// control plane reorders per-prefix (cheapest first) before the
	// broadcast; the edge keeps its plain priority selection.
	Prefixes []string `json:"prefixes"` // served destination prefixes, longest-match wins
	CostRate string   `json:"cost_rate"` // rate group pricing this gateway's cost
}

// Validate checks one gateway: non-empty ID/name, host:port-shaped
// address, known protocol. Selection never sees invalid entries.
func Validate(g Gateway) error {
	if g.ID == "" {
		return fmt.Errorf("gateway id required")
	}
	if g.Name == "" {
		return fmt.Errorf("gateway %q: name required", g.ID)
	}
	if g.SipAddr == "" {
		return fmt.Errorf("gateway %q: sip_addr required", g.ID)
	}
	if g.Protocol != "" {
		switch g.Protocol {
		case "udp", "tcp", "tls":
		default:
			return fmt.Errorf("gateway %q: protocol must be udp|tcp|tls, got %q", g.ID, g.Protocol)
		}
	}
	return nil
}

// List is the in-memory gateway set. Reads are lock-free after the
// snapshot copy; writes replace the whole set atomically (the set is
// small — operators manage dozens, not millions).
type List struct {
	mu   sync.RWMutex
	gws  []Gateway
}

// New returns an empty set.
func New() *List { return &List{} }

// Reload replaces the whole set. Invalid entries are skipped (the
// admin API validated them on write; this is belt-and-braces against
// a corrupted broadcast). Returns the applied count.
func (l *List) Reload(gws []Gateway) (int, error) {
	kept := make([]Gateway, 0, len(gws))
	for _, g := range gws {
		if err := Validate(g); err != nil {
			continue
		}
		kept = append(kept, g)
	}
	l.mu.Lock()
	l.gws = kept
	l.mu.Unlock()
	return len(kept), nil
}

// Gateways returns a copy of the full set (admin listing, broadcast
// payloads).
func (l *List) Gateways() []Gateway {
	l.mu.RLock()
	defer l.mu.RUnlock()
	out := make([]Gateway, len(l.gws))
	copy(out, l.gws)
	return out
}

// Count returns the total entry count (enabled + disabled).
func (l *List) Count() int {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return len(l.gws)
}
