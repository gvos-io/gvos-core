// Package rate implements tariff lookup: longest-prefix match over rate
// tables with time-window overrides, mirroring VOS semantics (7 decimal
// places, 8th rounded up on the per-minute price).
package rate

import (
	"github.com/gvos-io/gvos-core/internal/billingdomain/money"
	"fmt"
	"sort"
)

// Interval is one rating period within a week.
type Interval struct {
	// Days 0=Sunday..6=Saturday; empty means every day.
	Days  []int
	Start string // "HH:MM" inclusive
	End   string // "HH:MM" exclusive; "24:00" allowed
}

// Rate is one tariff entry.
type Rate struct {
	Prefix   string
	Price    money.Amount // per-minute (4-decimal fixed point, ADR-0003)
	MinDur   int          // minimal billed seconds
	InitDur  int          // first-step seconds (60+6: 60)
	InitRate money.Amount // first-step price; zero = Price*InitDur/60
	StepDur  int          // incremental step seconds (60+6: 6)
	// InEffect overrides for time windows; empty means always.
	Effective []Interval
}

// Entry is a resolved, windowed rate (price may differ per window).
type Entry struct {
	Prefix   string
	Price    money.Amount
	InitDur  int
	InitRate money.Amount
	StepDur  int
	MinDur   int
}

// Table holds a prefix set for one rate group.
type Table struct {
	entries   map[string][]Rate // prefix -> variants (base + time windows)
	sorted    []string          // descending, kept for debugging/iteration
	maxPrefix int               // longest prefix in the table
}

func NewTable(rates []Rate) *Table {
	m := map[string][]Rate{}
	maxP := 0
	for _, r := range rates {
		m[r.Prefix] = append(m[r.Prefix], r)
		if len(r.Prefix) > maxP {
			maxP = len(r.Prefix)
		}
	}
	ps := make([]string, 0, len(m))
	for p := range m {
		ps = append(ps, p)
	}
	// Descending length so iteration order is deterministic.
	sort.Slice(ps, func(i, j int) bool {
		if len(ps[i]) != len(ps[j]) {
			return len(ps[i]) > len(ps[j])
		}
		return ps[i] < ps[j]
	})
	return &Table{entries: m, sorted: ps, maxPrefix: maxP}
}

// Lookup returns the rate entry in effect for the number at the given
// time-of-week slot (day int, minutes-since-midnight), or nil when no
// prefix matches. Time-windowed variants win over the base rate; the
// base variant is the one with an empty Effective list.
//
// Match strategy: derive the prefixes of the number from full length
// down to 1 and probe the map — O(len(number)) hash lookups instead of
// a linear scan over the table.
func (t *Table) Lookup(number string, day int, minutes int) *Entry {
	maxLen := len(number)
	if maxLen > t.maxPrefix {
		maxLen = t.maxPrefix
	}
	for l := maxLen; l > 0; l-- {
		variants, ok := t.entries[number[:l]]
		if !ok {
			continue
		}
		// First pass: a windowed variant that matches.
		for _, v := range variants {
			if len(v.Effective) > 0 && windowMatches(v.Effective, day, minutes) {
				return toEntry(v)
			}
		}
		// Fallback: the base (window-less) variant.
		for _, v := range variants {
			if len(v.Effective) == 0 {
				return toEntry(v)
			}
		}
		// No base variant defined: last resort.
		return toEntry(variants[len(variants)-1])
	}
	return nil
}

func toEntry(v Rate) *Entry {
	return &Entry{
		Prefix:   v.Prefix,
		Price:    v.Price,
		InitDur:  v.InitDur,
		InitRate: v.InitRate,
		StepDur:  v.StepDur,
		MinDur:   v.MinDur,
	}
}

func windowMatches(ws []Interval, day, minutes int) bool {
	if len(ws) == 0 {
		return true
	}
	for _, w := range ws {
		if len(w.Days) > 0 && !containsDay(w.Days, day) {
			continue
		}
		s := toMinutes(w.Start)
		e := toMinutes(w.End)
		if e <= s { // wraps midnight
			if minutes >= s || minutes < e {
				return true
			}
			continue
		}
		if minutes >= s && minutes < e {
			return true
		}
	}
	return false
}

func containsDay(days []int, d int) bool {
	for _, x := range days {
		if x == d {
			return true
		}
	}
	return false
}

func toMinutes(hhmm string) int {
	var h, m int
	if _, err := fmt.Sscanf(hhmm, "%d:%d", &h, &m); err != nil {
		return 0
	}
	return h*60 + m
}

