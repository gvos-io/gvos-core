// Batch-C PG-mode verification (GVOS_TEST_DSN-gated): the CDR
// history authority — QueryCDRs keyset pagination, dest/call filters,
// and the unified envelope on the PG branch of both list faces.
package api

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestPGCDRHistoryQueryAndFilters(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("GVOS_TEST_DSN"))
	if dsn == "" {
		t.Skip("GVOS_TEST_DSN not set; skipping live-DB test")
	}
	ctx := context.Background()
	s, err := NewPGServer(ctx, dsn)
	if err != nil {
		t.Fatalf("NewPGServer: %v", err)
	}
	if err := s.pg.TestResetRuntimeState(ctx); err != nil {
		t.Fatal(err)
	}
	s.acl.Reset(nil)
	t.Cleanup(func() {
		_ = s.pg.TestResetRuntimeState(context.Background())
		s.pg.Close()
	})
	srv := httptest.NewServer(MuxFor(s))
	defer srv.Close()

	// Three settled calls, two destinations.
	for i, dest := range []string{"8613800138000", "8613800138000", "8613900139000"} {
		cid := "pgc-" + dest + "-" + string(rune('a'+i))
		if code, _ := s.callStart(callStartReq{CallID: cid, From: "1000", To: dest}); code != 200 {
			t.Fatalf("start %s: code=%d", cid, code)
		}
		if code, _ := s.callEnd(ctx, callEndReq{CallID: cid, DurSecs: 60}); code != 200 {
			t.Fatalf("end %s: code=%d", cid, code)
		}
	}

	// Page 1 (limit=2): keyset cursor present.
	rec := adminDo(t, srv, "dev-admin-token", "GET", "/v1/admin/cdrs?limit=2", "", "")
	var p1 struct {
		Count      int    `json:"count"`
		NextCursor string `json:"next_cursor"`
		Items      []struct {
			CallID string `json:"call_id"`
			Cost   string `json:"cost"`
		} `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &p1); err != nil {
		t.Fatalf("page1: %v body=%s", err, rec.Body.String())
	}
	if p1.Count != 2 || p1.NextCursor == "" || len(p1.Items) != 2 {
		t.Fatalf("page1 = %+v (cdrs table may hold rows from a prior suite)", p1)
	}
	// Money crosses as decimal strings (ADR-0003).
	for _, it := range p1.Items {
		if it.Cost == "" {
			t.Fatalf("cdr %s: empty cost string", it.CallID)
		}
	}

	// Page 2 via cursor: the remaining row, empty next_cursor.
	rec = adminDo(t, srv, "dev-admin-token", "GET", "/v1/admin/cdrs?limit=2&cursor="+p1.NextCursor, "", "")
	var p2 struct {
		Count      int    `json:"count"`
		NextCursor string `json:"next_cursor"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &p2); err != nil {
		t.Fatal(err)
	}
	if p2.Count != 1 || p2.NextCursor != "" {
		t.Fatalf("page2 count=%d next=%q, want 1/empty", p2.Count, p2.NextCursor)
	}

	// Dest filter narrows to one.
	rec = adminDo(t, srv, "dev-admin-token", "GET", "/v1/admin/cdrs?dest=8613900139000", "", "")
	var f1 struct {
		Count int `json:"count"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &f1)
	if f1.Count != 1 {
		t.Fatalf("dest filter count=%d, want 1", f1.Count)
	}

	// Garbage cursor (URL-safe chars, undecodable payload): 400
	// BAD_CURSOR, not 500. (Raw "%%%" would be dropped by the URL
	// parser before it reaches the handler.)
	rec = adminDo(t, srv, "dev-admin-token", "GET", "/v1/admin/cdrs?cursor=zzzz", "", "")
	if rec.Code != 400 {
		t.Fatalf("bad cursor = %d, want 400", rec.Code)
	}
}
