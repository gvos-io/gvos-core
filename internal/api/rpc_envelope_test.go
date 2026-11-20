package api

import (
	"encoding/json"
	"testing"

	"github.com/gvos-io/gvos-core/internal/customerdomain/blocklist"
)

// TestLockReplyEnvelope: the svc.call.start reply wraps the call-face
// body with the suspend coordinates Kamailio's reply event route
// needs (tindex/tlabel pass-through; plain bus clients get the
// unwrapped form). U8 contract. Exercises the production lockReply
// (the same closure StartCallSvc serves), not a rebuilt shape.
func TestLockReplyEnvelope(t *testing.T) {
	req := []byte(`{"call_id":"rpc-1","from_user":"1000","to_user":"8613800138000","tindex":27544,"tlabel":1409183275}`)
	s := NewServer()
	code, body := s.callStart(parseCallStart(t, req))
	if code != 200 {
		t.Fatalf("start: %d", code)
	}
	b, _ := json.Marshal(body)
	env := lockReply(code, body, req)

	var m map[string]any
	if err := json.Unmarshal(env, &m); err != nil {
		t.Fatal(err)
	}
	if m["code"].(float64) != 200 || m["call_id"] != "rpc-1" {
		t.Errorf("envelope: %s", env)
	}
	if m["tindex"].(float64) != 27544 || m["tlabel"].(float64) != 1409183275 {
		t.Errorf("suspend coords not echoed: %s", env)
	}
	inner, _ := json.Marshal(m["body"])
	if string(inner) != string(b) {
		t.Errorf("body passthrough mismatch: %s vs %s", inner, b)
	}
	// The edge's jansson probes: code top-level, record inside body.
	if _, ok := m["tlabel"]; !ok {
		t.Error("tlabel missing (edge resume needs it)")
	}
}

// TestLockReplyPlainClient: no suspend coordinates on the request
// (bus-native client) means the plain {code, body} envelope —
// nothing for an edge reply route to resume.
func TestLockReplyPlainClient(t *testing.T) {
	req := []byte(`{"call_id":"rpc-2","from_user":"1000","to_user":"8613800138000"}`)
	s := NewServer()
	code, body := s.callStart(parseCallStart(t, req))
	env := lockReply(code, body, req)
	var m map[string]any
	if err := json.Unmarshal(env, &m); err != nil {
		t.Fatal(err)
	}
	if _, ok := m["tindex"]; ok {
		t.Errorf("plain client reply must not carry suspend coords: %s", env)
	}
	if m["code"].(float64) != float64(code) {
		t.Errorf("code mismatch: %s", env)
	}
}

// TestLockReplyRejectShape: a rejected start (risk-blocked /
// insufficient credit) still carries the suspend coordinates — the
// edge resume route keys its 403-vs-relay branch on code, and a
// reject without coords would leave the INVITE suspended until the
// janitor's 2s timeout.
func TestLockReplyRejectShape(t *testing.T) {
	req := []byte(`{"call_id":"rpc-3","from_user":"1000","to_user":"8613800138000","tindex":9,"tlabel":77}`)
	s := NewServer()
	// Blackhole the destination so callStart 403s: no account under
	// this caller in memory mode defaults to the demo account, so
	// block the destination instead.
	s.blocklist.Add(blocklist.Entry{Kind: "black", Scope: "dest", Pattern: "8613800138000"})
	code, body := s.callStart(parseCallStart(t, req))
	if code == 200 {
		t.Fatalf("expected reject, got %d", code)
	}
	env := lockReply(code, body, req)
	var m map[string]any
	_ = json.Unmarshal(env, &m)
	if m["code"].(float64) != float64(code) {
		t.Errorf("code not carried on reject: %s", env)
	}
	if _, ok := m["tlabel"]; !ok {
		t.Errorf("reject reply missing suspend coords (edge 403 branch needs them): %s", env)
	}
}

func parseCallStart(t *testing.T, b []byte) callStartReq {
	t.Helper()
	var r callStartReq
	if err := json.Unmarshal(b, &r); err != nil {
		t.Fatal(err)
	}
	return r
}
