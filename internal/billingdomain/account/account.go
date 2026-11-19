// Package account models tenants, agent chains and prepaid balances.
package account

import (
	"github.com/lixuanqun/gvos-core/internal/billingdomain/money"
	"fmt"
	"sync"
	"time"
)

// Account is one billable identity in a chain (tenant -> agents ->
// end account). Parent chains drive cascade charging: each level's rate
// group can add margin on the same CDR.
type Account struct {
	ID         string
	Username   string // SIP auth username (PG mode index key)
	Name       string
	ParentID   string // "" for tenant root
	RateGroup  string // which rate.Table applies
	Currency   string
	Postpaid   bool    // skip prepaid locks when true
	Credit     money.Amount // prepaid balance (ADR-0003 fixed point)
	MaxDebt    money.Amount // allowed negative balance
	Concurrent int     // concurrent-call cap, 0 = unlimited
	CPS        int     // calls-per-second cap, 0 = unlimited
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

// Reserve locks est for a call on every prepaid account in the chain.
// Postpaid accounts (Postpaid=true) are skipped — they charge on the
// invoice cycle instead. It is all-or-nothing: on any failure the whole
// reservation is rolled back. Returns the reservation token used by
// Commit/Release.
func (s *Store) Reserve(id string, est money.Amount) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	chain, err := s.chainLocked(id)
	if err != nil {
		return "", err
	}
	var charged []*Account
	for _, a := range chain {
		if a.Postpaid {
			continue
		}
		if a.Credit.Sub(a.MaxDebt) < est {
			for _, c := range charged {
				c.Credit = c.Credit.Add(est)
			}
			return "", fmt.Errorf("account %q insufficient balance %s < %s", a.ID, a.Credit, est)
		}
		a.Credit = a.Credit.Sub(est)
		charged = append(charged, a)
	}
	return id + ":call:" + fmt.Sprintf("%d", now()), nil
}

// Commit finalizes actual cost: returns deltas are applied so the
// released remainder returns to balances (est >= actual normally).
func (s *Store) Commit(token string, id string, est, actual money.Amount) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	chain, err := s.chainLocked(id)
	if err != nil {
		return err
	}
	diff := est.Sub(actual) // positive: refund remainder
	for _, a := range chain {
		a.Credit = a.Credit.Add(diff)
	}
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
