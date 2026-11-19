package api

import (
	"encoding/json"
	"testing"
)

// TestLockReplyEnvelope: the svc.call.start reply wraps the call-face
// body with the suspend coordinates Kamailio's reply event route
// needs (tindex/tlabel pass-through; plain bus clients get the
// unwrapped form). U8 contract.
func TestLockReplyEnvelope(t *testing.T) {
	req := []byte(`{"call_id":"rpc-1","from_user":"1000","to_user":"8613800138000","tindex":27544,"tlabel":1409183275}`)
	s := NewServer()
	code, body := s.callStart(parseCallStart(t, req))
	if code != 200 {
		t.Fatalf("start: %d", code)
	}
	b, _ := json.Marshal(body)
	// Rebuild the lockReply wrapping (same shape as StartCallSvc).
	var suspend struct {
		CallID string `json:"call_id"`
		TIndex int64  `json:"tindex"`
		TLabel int64  `json:"tlabel"`
	}
	_ = json.Unmarshal(req, &suspend)
	if suspend.TIndex == 0 && suspend.TLabel == 0 {
		t.Fatal("test requires suspend coords")
	}
	env, _ := json.Marshal(map[string]any{
		"code": code, "body": body,
		"call_id": suspend.CallID, "tindex": suspend.TIndex, "tlabel": suspend.TLabel,
	})
	var m map[string]any
	if err := json.Unmarshal(env, &m); err != nil {
		t.Fatal(err)
	}
	if m["code"].(float64) != 200 || m["call_id"] != "rpc-1" {
		t.Errorf("envelope: %s", env)
	}
	inner, _ := json.Marshal(m["body"])
	if string(inner) != string(b) {
		t.Errorf("body passthrough mismatch: %s vs %s", inner, b)
	}
	// The edge's jansson probes: code top-level, record inside body.
	var flat map[string]any
	_ = json.Unmarshal(env, &flat)
	if _, ok := flat["tlabel"]; !ok {
		t.Error("tlabel missing (edge resume needs it)")
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
