// M11 F1 origination lifecycle tests (memory mode): the customer
// plane semantics — scoped account, admission rejections, the
// pending→dialing→answered→ended state machine driven by the normal
// signaling events, dedupe, and cancel.
package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gvos-io/gvos-core/internal/customerdomain/blocklist"
	"github.com/gvos-io/gvos-core/internal/shared/acl"
)

type httpHandler2 = http.Handler
type stdRec = httptest.ResponseRecorder

func doCallReq(t *testing.T, h httpHandler2, key, method, path string, body any) *stdRec {
	t.Helper()
	return doKey(t, h, key, method, path, body)
}

func blocklistEntry(scope, pattern string, isRegex bool) blocklist.Entry {
	return blocklist.Entry{Kind: "black", Scope: scope, Pattern: pattern, IsRegex: isRegex}
}

func (s *Server) liveCallIDs() []string {
	s.origMu.RLock()
	defer s.origMu.RUnlock()
	out := make([]string, 0, len(s.origIndex))
	for id := range s.origIndex {
		out = append(out, id)
	}
	return out
}

func TestCallOriginationLifecycle(t *testing.T) {
	s := NewServer()
	h := MuxFor(s)
	key := issueKey(s, acl.RoleTenant, "agent1") // subtree: 1000

	// Happy path: provision → pending.
	rec := doKey(t, h, key, "POST", "/v1/calls", map[string]any{
		"caller": "13800001234", "callee": "13900005678", "account_id": "1000", "max_duration_s": 600,
	})
	if rec.Code != 202 {
		t.Fatalf("create = %d body=%s", rec.Code, rec.Body.String())
	}
	var created struct {
		CallID string `json:"call_id"`
		State  string `json:"state"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	if created.State != "pending" || created.CallID == "" {
		t.Fatalf("created = %+v", created)
	}

	// Signaling events drive the machine (the UAC uses call_id as the
	// SIP Call-ID; the edge reports through the normal faces).
	if code, _ := s.callStart(callStartReq{CallID: created.CallID, From: "1000", To: "13800001234"}); code != 200 {
		t.Fatalf("simulated start = %d", code)
	}
	if code, _ := s.callAnswer(created.CallID); code != 200 {
		t.Fatalf("simulated answer = %d", code)
	}
	rec = doKey(t, h, key, "GET", "/v1/calls/"+created.CallID, nil)
	var got struct {
		Call struct {
			State  string `json:"state"`
			Caller string `json:"caller"`
		} `json:"call"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got.Call.State != "answered" {
		t.Fatalf("state after answer = %s, want answered", got.Call.State)
	}
	if code, cdr := s.callEnd(nil, callEndReq{CallID: created.CallID, DurSecs: 30}); code != 200 {
		t.Fatalf("simulated end = %d", code)
	} else {
		_ = cdr
	}
	rec = doKey(t, h, key, "GET", "/v1/calls/"+created.CallID, nil)
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got.Call.State != "ended" {
		t.Fatalf("state after end = %s, want ended", got.Call.State)
	}
	// Cancel after end: 409.
	if rec := doKey(t, h, key, "DELETE", "/v1/calls/"+created.CallID, nil); rec.Code != 409 {
		t.Fatalf("cancel ended = %d, want 409", rec.Code)
	}
	// CDR exists for the origination call (billed by the normal path).
	if n := len(s.cdrs); n != 1 {
		t.Fatalf("cdrs = %d, want 1", n)
	}
}

func TestCallOriginationAdmissionAndScopes(t *testing.T) {
	s := NewServer()
	h := MuxFor(s)
	key := issueKey(s, acl.RoleTenant, "agent1")
	foreign := issueKey(s, acl.RoleTenant, "elsewhere")

	mk := func(caller string) int {
		return doKey(t, h, key, "POST", "/v1/calls", map[string]any{
			"caller": caller, "callee": "13900005678", "account_id": "1000",
		}).Code
	}
	// Blacklisted destination.
	s.blocklist.Add(blocklistEntry("dest", "13800000000", false))
	if code := mk("13800000000"); code != 403 {
		t.Fatalf("blacklisted = %d, want 403", code)
	}
	// No rate for destination.
	if code := mk("99999999999"); code != 404 {
		t.Fatalf("no-rate = %d, want 404", code)
	}
	// Anti-storm dedupe: same pair twice inside the window → 409.
	if code := mk("13800001234"); code != 202 {
		t.Fatalf("first = %d, want 202", code)
	}
	if code := mk("13800001234"); code != 409 {
		t.Fatalf("dedupe = %d, want 409", code)
	}
	// Foreign tenant cannot read (or even confirm) the call.
	list := s.liveCallIDs()
	if len(list) == 0 {
		t.Fatal("no live call to probe with")
	}
	if rec := doKey(t, h, foreign, "GET", "/v1/calls/"+list[0], nil); rec.Code != 404 {
		t.Fatalf("foreign GET = %d, want 404", rec.Code)
	}
	// Cancel the live one: pending → cancelled, tracked id removed.
	if rec := doKey(t, h, key, "DELETE", "/v1/calls/"+list[0], nil); rec.Code != 200 {
		t.Fatalf("cancel = %d", rec.Code)
	}
	if s.isTracked(list[0]) {
		t.Fatal("cancelled call still tracked")
	}
	// to_agent XOR callee: both set → 400.
	if code := doKey(t, h, key, "POST", "/v1/calls", map[string]any{
		"caller": "13800001234", "callee": "1", "to_agent": "bot", "account_id": "1000",
	}).Code; code != 400 {
		t.Fatalf("callee+to_agent = %d, want 400", code)
	}
}
