// Package axb is the privacy-number service (the VOS AXB module's
// open equivalent): A dials the virtual number X, the platform
// rewrites the destination to B — both sides stay anonymous. Bindings
// are time-boxed and per-tenant; X numbers are allocated from a
// virtual pool (9xxxxxxxxx) and recycled on expiry.
//
// Call flow (gvos): the INVITE arrives with dest = X; gv-core's call
// start resolves the binding, bills against B's rate, and returns a
// rewrite_dest instruction — the signaling edge rewrites the R-URI
// before relaying to the gateway. B's callback path (PSTN DID into
// the platform) is the same binding in reverse; it needs DID routing
// on the ingress trunk, out of this package's scope (recorded).
package axb

import (
	"fmt"
	"math/rand"
	"strings"
	"sync"
	"time"
)

// Binding is one AXB relationship.
type Binding struct {
	ID       string    `json:"id"`
	XNumber  string    `json:"x_number"`
	ANumber  string    `json:"a_number"`
	BNumber  string    `json:"b_number"`
	ExpireAt time.Time `json:"expire_at"`
	Active   bool      `json:"active"`
}

// Store is the in-memory binding registry (PG mode swaps in a
// table-backed store with the same interface; migration 010).
type Store struct {
	mu        sync.RWMutex
	byX       map[string]*Binding
	byID      map[string]*Binding
	xPoolBase int64 // next X candidate: 9 + base
	serial    int64
}

// NewStore returns an empty registry.
func NewStore() *Store {
	return &Store{byX: map[string]*Binding{}, byID: map[string]*Binding{}, xPoolBase: 9000000000}
}

// validate trims and sanity-checks the numbers.
func validate(a, b string) error {
	if len(strings.TrimSpace(a)) < 3 || len(strings.TrimSpace(b)) < 3 {
		return fmt.Errorf("a_number and b_number required (>=3 digits)")
	}
	return nil
}

// Bind creates a binding, allocating X from the pool. ttl bounds the
// lifetime; an existing ACTIVE binding for the same (a,b) pair is
// reused (the caller gets the same X back — repeat calls within the
// window stay consistent).
func (s *Store) Bind(a, b string, ttl time.Duration) (*Binding, error) {
	a, b = strings.TrimSpace(a), strings.TrimSpace(b)
	if err := validate(a, b); err != nil {
		return nil, err
	}
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	// Reuse an existing active binding for the pair.
	for _, bd := range s.byX {
		if bd.Active && bd.ANumber == a && bd.BNumber == b && bd.ExpireAt.After(time.Now()) {
			return bd, nil
		}
	}
	bd := &Binding{
		XNumber:  s.allocateX(),
		ANumber:  a,
		BNumber:  b,
		ExpireAt: time.Now().Add(ttl),
		Active:   true,
	}
	s.serial++
	bd.ID = fmt.Sprintf("axb-%d", s.serial)
	s.byX[bd.XNumber] = bd
	s.byID[bd.ID] = bd
	return bd, nil
}

// allocateX picks an unused pool number (9xxxxxxxxx).
func (s *Store) allocateX() string {
	for i := 0; i < 1000; i++ {
		cand := fmt.Sprintf("9%09d", s.xPoolBase%1000000000+int64(rand.Intn(1000)))
		s.xPoolBase++
		if _, taken := s.byX[cand]; !taken {
			return cand
		}
	}
	return fmt.Sprintf("9%09d", time.Now().UnixNano()%1000000000)
}

// Resolve maps a dialed X to the live binding (expiry-checked; an
// expired binding is deactivated lazily on read).
func (s *Store) Resolve(x string) (*Binding, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	bd, ok := s.byX[strings.TrimSpace(x)]
	if !ok || !bd.Active {
		return nil, false
	}
	if time.Now().After(bd.ExpireAt) {
		bd.Active = false // lazy expiry
		return nil, false
	}
	return bd, true
}

// RevokeByID deactivates by binding id (returns 1 when revoked).
func (s *Store) RevokeByID(id string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if bd, ok := s.byID[id]; ok && bd.Active {
		bd.Active = false
		return 1
	}
	return 0
}

// RevokeByX deactivates by virtual number.
func (s *Store) RevokeByX(x string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if bd, ok := s.byX[strings.TrimSpace(x)]; ok && bd.Active {
		bd.Active = false
		return 1
	}
	return 0
}

// List returns active bindings (snapshot).
func (s *Store) List() []Binding {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Binding, 0, len(s.byX))
	for _, bd := range s.byX {
		if bd.Active && bd.ExpireAt.After(time.Now()) {
			out = append(out, *bd)
		}
	}
	return out
}

// Count returns the active binding count (monitoring).
func (s *Store) Count() int {
	return len(s.List())
}
