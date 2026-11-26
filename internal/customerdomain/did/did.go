// Package did is the inbound-DID registry (DID 入局终结): a carrier
// INVITE for a DID number resolves here. The account that OWNS the
// number is the billing account (被叫方计费 — pipecat plan O1,
// resolved 2026-11), rated by that account's own rate_group; the
// destination decides where the edge terminates the call:
//
//	user         — registered SIP username, lookup("location") at the edge
//	external     — E.164 second leg, dispatched via the gateway GW_DISPATCH
//	               answers (AXB callback closes here in reverse)
//	media_agent  — reserved seam for the AI voice-agent media plane; the
//	               control plane answers 501 until that ships (it must
//	               never silently bill a call it cannot route)
//
// Mirrors axb/ipallow: in-memory registry (PG store shares the
// interface, migration 013), snapshot-broadcast to the edge htable.
package did

import (
	"fmt"
	"strings"
	"sync"
)

// DestinationType enumerates where an inbound DID terminates.
type DestinationType string

const (
	DestUser     DestinationType = "user"
	DestExternal DestinationType = "external"
	DestAgent    DestinationType = "media_agent"
)

// DID is one inbound number registration.
type DID struct {
	ID              string          `json:"id"`
	Number          string          `json:"did_number"`
	AccountID       string          `json:"account_id"`
	DestinationType DestinationType `json:"destination_type"`
	Destination     string          `json:"destination"`
	Enabled         bool            `json:"enabled"`
}

// Store is the in-memory registry.
type Store struct {
	mu    sync.RWMutex
	byNum map[string]*DID
	byID  map[string]*DID
}

// NewStore returns an empty registry.
func NewStore() *Store {
	return &Store{byNum: map[string]*DID{}, byID: map[string]*DID{}}
}

// Validate checks one registration before it enters the registry.
func Validate(d DID) error {
	if strings.TrimSpace(d.Number) == "" {
		return fmt.Errorf("did_number required")
	}
	if len(strings.TrimSpace(d.Number)) > 20 {
		return fmt.Errorf("did_number too long (max 20 digits)")
	}
	if strings.TrimSpace(d.AccountID) == "" {
		return fmt.Errorf("account_id required (callee billing account)")
	}
	switch d.DestinationType {
	case DestUser, DestExternal, DestAgent:
	default:
		return fmt.Errorf("destination_type must be user|external|media_agent, got %q", d.DestinationType)
	}
	if strings.TrimSpace(d.Destination) == "" {
		return fmt.Errorf("destination required")
	}
	return nil
}

// Upsert registers (or replaces) a DID. The number is the key.
func (s *Store) Upsert(d DID) error {
	if err := Validate(d); err != nil {
		return err
	}
	d.Number = strings.TrimSpace(d.Number)
	s.mu.Lock()
	defer s.mu.Unlock()
	if old, ok := s.byNum[d.Number]; ok {
		delete(s.byID, old.ID)
	}
	s.byNum[d.Number] = &d
	s.byID[d.ID] = &d
	return nil
}

// Delete removes a registration by number.
func (s *Store) Delete(number string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if d, ok := s.byNum[number]; ok {
		delete(s.byNum, number)
		delete(s.byID, d.ID)
		return true
	}
	return false
}

// Resolve looks up the number.
func (s *Store) Resolve(number string) (DID, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if d, ok := s.byNum[number]; ok && d.Enabled {
		return *d, true
	}
	return DID{}, false
}

// List returns all registrations.
func (s *Store) List() []DID {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]DID, 0, len(s.byNum))
	for _, d := range s.byNum {
		out = append(out, *d)
	}
	return out
}

// Count returns the registry size.
func (s *Store) Count() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.byNum)
}

// Reload atomically replaces the registry (settings restore / snapshot
// apply share this entry point). Snapshot entries come from List() —
// every field is explicit, so the flag is honored as-is.
func (s *Store) Reload(list []DID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.byNum = make(map[string]*DID, len(list))
	s.byID = make(map[string]*DID, len(list))
	for i := range list {
		d := list[i]
		s.byNum[d.Number] = &d
		s.byID[d.ID] = &d
	}
}
