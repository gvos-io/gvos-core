// Package-2 robustness regressions: tenant CDR stream protocol
// (since validation, tap drop counter), media WS per-call scoping,
// and accounts list pagination.
package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gvos-io/gvos-core/internal/shared/acl"
)

func httpNewRequest(method, path string) (*http.Request, error) {
	return http.NewRequest(method, path, nil)
}

func httptestNewRecorder() *httptest.ResponseRecorder {
	return httptest.NewRecorder()
}

func TestTenantStreamSinceValidation(t *testing.T) {
	s := NewServer()
	h := MuxFor(s)
	key := issueKey(s, acl.RoleTenant, "agent1")

	doWS := func(query string) int {
		req, _ := httpNewRequest("GET", "/v1/tenant/cdr-stream"+query)
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Upgrade", "websocket")
		req.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
		rec := httptestNewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	if code := doWS("?since=not-a-time"); code != 400 {
		t.Fatalf("bad since = %d, want 400", code)
	}
	// (The full 101 handshake needs a hijack-capable socket; covered by
	// the live smoke — the recorder cannot Hijack.)
}

func TestCDRTapDropCounter(t *testing.T) {
	s := NewServer()
	tap := &cdrTap{set: map[string]bool{"a": true}, ch: make(chan []byte, 2)}
	s.cdrTapsMu.Lock()
	s.cdrTaps = append(s.cdrTaps, tap)
	s.cdrTapsMu.Unlock()

	for i := 0; i < 5; i++ {
		s.fanCDRTap([]byte("frame"), "a")
	}
	if n := tap.dropped.Load(); n != 3 {
		t.Fatalf("dropped = %d, want 3 (5 frames into a 2-slot buffer)", n)
	}
	// Out-of-scope taps never see the frame, never count a drop.
	s.fanCDRTap([]byte("frame"), "b")
	if n := tap.dropped.Load(); n != 3 {
		t.Fatalf("out-of-scope frame counted: dropped = %d", n)
	}
}

func TestMediaStreamTenantScope(t *testing.T) {
	s := NewServer()
	h := MuxFor(s)
	agent1Key := issueKey(s, acl.RoleTenant, "agent1") // subtree holds "1000"
	foreignKey := issueKey(s, acl.RoleTenant, "no-such-root")

	// A live call owned by account 1000 (under agent1).
	if code, _ := s.callStart(callStartReq{CallID: "scope-1", From: "1000", To: "8613800138000"}); code != 200 {
		t.Fatalf("call start = %d", code)
	}

	doWS := func(key, callID string) int {
		req, _ := httpNewRequest("GET", "/v1/stream/"+callID)
		req.Header.Set("Authorization", "Bearer "+key)
		rec := httptestNewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	// No upgrade header → 400 upgrade-required means AUTH PASSED.
	if code := doWS(agent1Key, "scope-1"); code != 400 {
		t.Fatalf("in-scope tenant = %d, want 400 (past auth)", code)
	}
	// A foreign tenant: denied without revealing the call.
	if code := doWS(foreignKey, "scope-1"); code != 403 {
		t.Fatalf("out-of-scope tenant = %d, want 403", code)
	}
	// Unknown call: denied.
	if code := doWS(agent1Key, "call-none"); code != 403 {
		t.Fatalf("unknown call = %d, want 403", code)
	}
}

func TestAccountsListPagination(t *testing.T) {
	s := NewServer()
	h := MuxFor(s)
	for _, u := range []string{"page-a", "page-b", "page-c"} {
		if rec := doAdmin(t, h, "POST", "/v1/admin/accounts", map[string]any{"username": u, "initial_balance": "1"}); rec.Code != 201 {
			t.Fatalf("create %s = %d", u, rec.Code)
		}
	}
	rec := doAdmin(t, h, "GET", "/v1/admin/accounts?limit=2", nil)
	var p1 struct {
		Count      int    `json:"count"`
		Total      int    `json:"total"`
		NextCursor string `json:"next_cursor"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &p1)
	if p1.Count != 2 || p1.NextCursor == "" || p1.Total < 3 {
		t.Fatalf("page1 = %+v", p1)
	}
	rec = doAdmin(t, h, "GET", "/v1/admin/accounts?limit=2&cursor="+p1.NextCursor, nil)
	var p2 struct {
		Count int `json:"count"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &p2)
	if p2.Count < 1 {
		t.Fatalf("page2 empty: %s", rec.Body.String())
	}
}
