// Package blocklist implements source/destination number screening
// with prefix and regex rules, mirroring the number_lists table
// (tenant NULL = global). Matching semantics:
//
//   - black list: any hit rejects the call
//   - white list: when a scope has white entries, only hits pass
//
// Global and tenant rules compose: a global black hit always
// rejects; within one list kind, most-specific (tenant) wins.
package blocklist

import (
	"regexp"
	"sync"
)

// Entry is one screening rule.
type Entry struct {
	TenantID string // "" = global
	Kind     string // black | white
	Scope    string // src | dest
	Pattern  string
	IsRegex  bool
	re       *regexp.Regexp
}

// List is the compiled screening set. Prefix rules are indexed by
// length (hash probe from the number's full length downward, same
// strategy as the rate engine) so screening stays O(len(number)) at
// 1M+ rules; regex rules stay a linear scan (they are rare).
type List struct {
	mu sync.RWMutex
	// black[scope][len] -> set of prefixes of that exact length
	black map[string]map[int]map[string]bool
	// white[scope][len] -> set of prefixes; presence of any white for
	// a scope puts it in default-deny mode
	white map[string]map[int]map[string]bool
	// whiteAny[scope] -> whites configured (default-deny marker)
	whiteAny map[string]bool
	regexes  []Entry
}

func New() *List {
	return &List{
		black:    map[string]map[int]map[string]bool{},
		white:    map[string]map[int]map[string]bool{},
		whiteAny: map[string]bool{},
	}
}

func (l *List) reset() {
	l.black = map[string]map[int]map[string]bool{}
	l.white = map[string]map[int]map[string]bool{}
	l.whiteAny = map[string]bool{}
	l.regexes = nil
}

func (l *List) addEntry(e Entry) {
	if e.IsRegex {
		if re, err := regexp.Compile(e.Pattern); err == nil {
			e.re = re
			l.regexes = append(l.regexes, e)
		}
		return
	}
	bucket := l.black
	if e.Kind == "white" {
		bucket = l.white
		l.whiteAny[e.Scope] = true
	}
	byLen := bucket[e.Scope]
	if byLen == nil {
		byLen = map[int]map[string]bool{}
		bucket[e.Scope] = byLen
	}
	set := byLen[len(e.Pattern)]
	if set == nil {
		set = map[string]bool{}
		byLen[len(e.Pattern)] = set
	}
	set[e.Pattern] = true
}

// Reload atomically replaces the rule set.
func (l *List) Reload(entries []Entry) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.reset()
	for _, e := range entries {
		l.addEntry(e)
	}
}

// Add appends one rule live (admin API path).
func (l *List) Add(e Entry) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.addEntry(e)
}

// Snapshot returns the prefix entries currently active (regex entries
// included as-is, re unexported). The compiled index drops the tenant
// dimension, so entries come back tenant-flattened — consumers that
// need per-tenancy (audit) read the admin list; this snapshot is for
// rebroadcasting the full rule set to replicas/edges.
func (l *List) Snapshot() []Entry {
	l.mu.RLock()
	defer l.mu.RUnlock()
	var out []Entry
	for scope, byLen := range l.black {
		for _, set := range byLen {
			for pattern := range set {
				out = append(out, Entry{Kind: "black", Scope: scope, Pattern: pattern})
			}
		}
	}
	for scope, byLen := range l.white {
		for _, set := range byLen {
			for pattern := range set {
				out = append(out, Entry{Kind: "white", Scope: scope, Pattern: pattern})
			}
		}
	}
	out = append(out, l.regexes...)
	return out
}

// Remove drops prefix rules matching tenant/kind/scope/pattern.
func (l *List) Remove(tenant, kind, scope, pattern string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	bucket := l.black
	if kind == "white" {
		bucket = l.white
	}
	byLen := bucket[scope]
	if byLen == nil {
		return 0
	}
	set := byLen[len(pattern)]
	if set == nil || !set[pattern] {
		return 0
	}
	delete(set, pattern)
	if len(set) == 0 {
		delete(byLen, len(pattern))
	}
	if len(byLen) == 0 {
		delete(bucket, scope)
	}
	if kind == "white" && l.whiteAny[scope] {
		// Recompute the default-deny marker: only clear it when no
		// white rules remain for the scope.
		if len(l.white[scope]) == 0 {
			delete(l.whiteAny, scope)
		}
	}
	return 1
}

// CheckScreen returns (allowed, matchedPattern) for a number in a
// scope. Decision order:
//  1. white prefix hit -> allowed
//  2. black prefix hit -> rejected
//  3. regex rules (linear, rare): white hit allows, black rejects
//  4. whites configured for the scope -> rejected (default deny)
//  5. nothing configured -> allowed
func (l *List) CheckScreen(number, scope string) (bool, string) {
	l.mu.RLock()
	defer l.mu.RUnlock()

	// Prefix probes: longest first so the most specific rule wins.
	for n := len(number); n > 0; n-- {
		prefix := number[:n]
		if set := l.white[scope][n]; set != nil && set[prefix] {
			return true, prefix
		}
		if set := l.black[scope][n]; set != nil && set[prefix] {
			return false, prefix
		}
	}
	// Regex rules (linear by design - they are the rare case).
	for _, e := range l.regexes {
		if e.Scope != scope || e.re == nil || !e.re.MatchString(number) {
			continue
		}
		if e.Kind == "white" {
			return true, e.Pattern
		}
		return false, e.Pattern
	}
	if l.whiteAny[scope] {
		return false, "white-list-default-deny"
	}
	return true, ""
}

// Count returns the prefix-rule count (monitoring/tests).
func (l *List) Count() int {
	l.mu.RLock()
	defer l.mu.RUnlock()
	n := 0
	for _, byLen := range l.black {
		for _, set := range byLen {
			n += len(set)
		}
	}
	for _, byLen := range l.white {
		for _, set := range byLen {
			n += len(set)
		}
	}
	return n + len(l.regexes)
}
