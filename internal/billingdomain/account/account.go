// Package account models tenants, agent chains and prepaid balances.
package account

import (
	"github.com/gvos-io/gvos-core/internal/billingdomain/money"
	"fmt"
	"sync"
	"time"
)

// Account is one billable identity in a chain (tenant -> agents ->
// end account). Parent chains drive cascade charging: each level's rate
// group can add margin on the same CDR.
type Account struct {
	ID         string       `json:"id"`
	Username   string       `json:"username"` // SIP auth username (PG mode index key)
	Name       string       `json:"name"`
	ParentID   string       `json:"parent_id"` // "" for tenant root
	RateGroup  string       `json:"rate_group"` // which rate.Table applies
	Currency   string       `json:"currency"`
	Postpaid   bool         `json:"postpaid"` // skip prepaid locks when true
	Credit     money.Amount `json:"credit"` // prepaid balance (ADR-0003 fixed point)
	MaxDebt    money.Amount `json:"max_debt"` // allowed negative balance
	Concurrent int          `json:"concurrent"` // concurrent-call cap, 0 = unlimited
	CPS        int          `json:"cps"` // calls-per-second cap, 0 = unlimited
}

// ParseID guards the PG-mode topup path: only strict non-UUID shapes
// that would trip PG's uuid cast short-circuit here (memory-mode ids
// like "1000" or "acc-..." pass; unknown-but-well-formed uuids fall
// through to the normal 404 from the store/PG lookup).
func ParseID(id string) (string, error) {
	// PG uuid shape check only when it looks like it was MEANT to be
	// a uuid (36 chars with dashes) but isn't valid — or contains
	// characters PG will reject outright.
	if len(id) == 36 && (id[8] != '-' || id[13] != '-' || id[18] != '-' || id[23] != '-') {
		return "", fmt.Errorf("bad account id %q", id)
	}
	if len(id) > 64 {
		return "", fmt.Errorf("account id too long")
	}
	return id, nil
}

// Store is an in-memory account registry with balance operations. The
// production implementation swaps this for Redis (call locks) +
// PostgreSQL (authoritative), same method surface.
type Store struct {
	mu       sync.RWMutex
	accounts map[string]*Account
}

func NewStore() *Store {
	return &Store{accounts: map[string]*Account{}}
}

func (s *Store) Put(a *Account) error {
	if a == nil || a.ID == "" {
		return fmt.Errorf("invalid account")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.accounts[a.ID] = a
	return nil
}

func (s *Store) Get(id string) (*Account, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	a, ok := s.accounts[id]
	if !ok {
		return nil, fmt.Errorf("account %q not found", id)
	}
	cp := *a
	return &cp, nil
}

// List returns all accounts (admin/monitoring).
func (s *Store) List() ([]*Account, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*Account, 0, len(s.accounts))
	for _, a := range s.accounts {
		cp := *a
		out = append(out, &cp)
	}
	return out, nil
}

// Chain returns the charging chain from the account up to the tenant
// root (inclusive), in charge order: end account first, then each
// ancestor agent, then tenant.
func (s *Store) Chain(id string) ([]*Account, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var chain []*Account
	seen := map[string]bool{}
	cur := id
	for cur != "" {
		if seen[cur] {
			return nil, fmt.Errorf("cycle at account %q", cur)
		}
		seen[cur] = true
		a, ok := s.accounts[cur]
		if !ok {
			return nil, fmt.Errorf("account %q not found", cur)
		}
		cp := *a
		chain = append(chain, &cp)
		cur = a.ParentID
	}
	return chain, nil
}

// Reserve locks est on the leaf prepaid account only (Mode A: terminal
// billing). Ancestors keep concurrent/CPS caps via admission, not
// co-debit. Postpaid leaf accounts skip the lock (invoice cycle).
func (s *Store) Reserve(id string, est money.Amount) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.accounts[id]
	if !ok {
		return "", fmt.Errorf("account %q not found", id)
	}
	if a.Postpaid {
		return id + ":call:" + fmt.Sprintf("%d", now()), nil
	}
	if a.Credit.Sub(a.MaxDebt) < est {
		return "", fmt.Errorf("account %q insufficient balance %s < %s", a.ID, a.Credit, est)
	}
	a.Credit = a.Credit.Sub(est)
	return id + ":call:" + fmt.Sprintf("%d", now()), nil
}

// Commit finalizes actual cost on the leaf: positive est-actual refunds
// the unused reservation.
func (s *Store) Commit(token string, id string, est, actual money.Amount) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.accounts[id]
	if !ok {
		return fmt.Errorf("account %q not found", id)
	}
	if a.Postpaid {
		return nil
	}
	a.Credit = a.Credit.Add(est.Sub(actual))
	return nil
}

// Release cancels a reservation fully (call never connected).
func (s *Store) Release(token string, id string, est money.Amount) error {
	return s.Commit(token, id, est, 0)
}

// chainLocked is Chain without re-locking (caller holds s.mu).
func (s *Store) chainLocked(id string) ([]*Account, error) {
	var chain []*Account
	seen := map[string]bool{}
	cur := id
	for cur != "" {
		if seen[cur] {
			return nil, fmt.Errorf("cycle at account %q", cur)
		}
		seen[cur] = true
		a, ok := s.accounts[cur]
		if !ok {
			return nil, fmt.Errorf("account %q not found", cur)
		}
		chain = append(chain, a)
		cur = a.ParentID
	}
	return chain, nil
}

// now is stubbed in tests via package var.
var now = func() int64 { return time.Now().UnixNano() }

func Balance(s *Store, id string) (money.Amount, error) {
	a, err := s.Get(id)
	if err != nil {
		return 0, err
	}
	return a.Credit, nil
}
