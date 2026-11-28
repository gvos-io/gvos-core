// Shared server-side filtering for the CDR list faces (admin +
// tenant). These parameters used to be parsed nowhere — console
// filters worked only against its mock core.
package api

import (
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/gvos-io/gvos-core/internal/callcore"
)

type cdrFilter struct {
	AccountID string
	Dest      string
	CallID    string
	From, To  time.Time
}

// parseCDRFilters reads account_id/dest/call_id/from/to. from/to take
// RFC3339 or a bare date (from inclusive at midnight UTC, to
// exclusive at next midnight). Returns the offending parameter on
// error so the caller can emit a targeted 400.
func parseCDRFilters(q url.Values) (cdrFilter, string) {
	var f cdrFilter
	f.AccountID = strings.TrimSpace(q.Get("account_id"))
	f.Dest = strings.TrimSpace(q.Get("dest"))
	f.CallID = strings.TrimSpace(q.Get("call_id"))
	if v := q.Get("from"); v != "" {
		t, err := parseWhen(v, false)
		if err != nil {
			return f, "from"
		}
		f.From = t
	}
	if v := q.Get("to"); v != "" {
		t, err := parseWhen(v, true)
		if err != nil {
			return f, "to"
		}
		f.To = t
	}
	return f, ""
}

func parseWhen(v string, end bool) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t, nil
	}
	if t, err := time.Parse("2006-01-02", v); err == nil {
		if end {
			return t.AddDate(0, 0, 1), nil // exclusive day end
		}
		return t, nil
	}
	return time.Time{}, fmt.Errorf("must be RFC3339 or YYYY-MM-DD")
}

// match applies the filters to one ring entry (memory mode).
func (f cdrFilter) match(c *callcore.CDR) bool {
	if f.AccountID != "" && c.AccountID != f.AccountID {
		return false
	}
	if f.Dest != "" && c.Dest != f.Dest {
		return false
	}
	if f.CallID != "" && c.CallID != f.CallID {
		return false
	}
	if !f.From.IsZero() && c.StartTime.Before(f.From) {
		return false
	}
	if !f.To.IsZero() && !c.StartTime.Before(f.To) {
		return false
	}
	return true
}
