// Package acl is the API-key access-control layer: keys bind
// (tenant, role) and replace the Phase A constant admin token.
//
// The in-memory Store here is the demo/test implementation; PG mode
// swaps in a table-backed Store with the same interface (api-roadmap
// Phase B, migration 009). Key material is stored as SHA-256 only —
// the plaintext is shown exactly once at creation time.
package acl

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"sync"
	"time"
)

// Role scopes a key's authority. admin/operator manage the whole
// deployment; tenant/agent are the self-service faces (Phase C).
type Role string

const (
	RoleAdmin    Role = "admin"
	RoleOperator Role = "operator"
	RoleTenant   Role = "tenant"
	RoleAgent    Role = "agent"
)

// Key is one API key record.
type Key struct {
	ID        string
	TenantID  string
	Role      Role
	Prefix    string // display handle, e.g. gvos_admin_ab12
	Hash      string // sha256 hex of the full key
	CreatedBy string
	RevokedAt *time.Time
	LastUsed  *time.Time
}

// Store is the key registry (in-memory demo impl; PG impl same shape).
type Store struct {
	mu   sync.RWMutex
	keys map[string]*Key // hash -> key
}

// NewStore returns an empty registry.
func NewStore() *Store { return &Store{keys: map[string]*Key{}} }

// GenerateKey produces a fresh key of the form
// gvos_<scope>_<32 hex chars>. It returns the plaintext exactly once
// plus the stored record (hash only).
func GenerateKey(role Role) (plaintext string, prefix string, hash string) {
	scope := "tenant"
	if role == RoleAdmin || role == RoleOperator {
		scope = "admin"
	}
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic("acl: rand unavailable: " + err.Error())
	}
	body := hex.EncodeToString(b)
	plaintext = "gvos_" + scope + "_" + body
	sum := sha256.Sum256([]byte(plaintext))
	return plaintext, "gvos_" + scope + "_" + body[:4], hex.EncodeToString(sum[:])
}

// HashKey hashes a presented key for lookup.
func HashKey(plaintext string) string {
	sum := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(sum[:])
}

// Add registers a key (idempotent per hash).
func (s *Store) Add(k Key) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.keys[k.Hash] = &k
}

// Lookup returns the live key for a presented plaintext. Revoked
// keys miss. Hit updates LastUsed (best effort; races are fine —
// this is observability, not authority).
func (s *Store) Lookup(plaintext string) (Key, bool) {
	s.mu.RLock()
	k, ok := s.keys[HashKey(plaintext)]
	s.mu.RUnlock()
	if !ok || k.RevokedAt != nil {
		return Key{}, false
	}
	now := time.Now()
	s.mu.Lock()
	k.LastUsed = &now
	s.mu.Unlock()
	return *k, true
}

// Revoke removes a key by prefix.
func (s *Store) Revoke(prefix string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, k := range s.keys {
		if k.Prefix == prefix {
			k.RevokedAt = &time.Time{}
			*k.RevokedAt = time.Now()
			n++
		}
	}
	return n
}

// Count reports the number of live keys.
func (s *Store) Count() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	n := 0
	for _, k := range s.keys {
		if k.RevokedAt == nil {
			n++
		}
	}
	return n
}

// List returns the live key records (no hash material).
func (s *Store) List() []Key {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := []Key{}
	for _, k := range s.keys {
		if k.RevokedAt == nil {
			out = append(out, *k)
		}
	}
	return out
}
