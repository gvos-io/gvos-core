package gvos

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/gvos-io/gvos-core/internal/api"
	"github.com/gvos-io/gvos-core/internal/shared/acl"
)

// The SDK speaks to a real in-process server (memory mode): the wire
// contract is what's under test, not a mock.
func newTestClient(t *testing.T) *Client {
	t.Helper()
	s := api.NewServer()
	h := api.MuxFor(s)
	key := api.NewTestKey(s, acl.RoleTenant, "agent1")
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return New(srv.URL, key)
}

func TestSDKLifecycle(t *testing.T) {
	c := newTestClient(t)

	// Read face.
	me, err := c.Me()
	if err != nil || me["tenant_id"] != "agent1" {
		t.Fatalf("me = %v err=%v", me, err)
	}
	stats, err := c.Stats()
	if err != nil {
		t.Fatalf("stats: %v", err)
	}

	// Write face: provision + allocate (transfer semantics).
	cred, err := c.ProvisionChild("sdk-u1", "SDK Child", 2)
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if cred.SIPPassword == "" {
		t.Fatal("one-time SIP password missing")
	}
	if err := c.Allocate(cred.ID, 1); err != nil {
		t.Fatalf("allocate: %v", err)
	}

	// Origination: 202 + state readable + cancel lifecycle.
	call, err := c.Originate("13800001234", "13900005678", "1000", "sdk-idem-1")
	if err != nil {
		t.Fatalf("originate: %v", err)
	}
	st, err := c.CallStatus(call.CallID)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	var state struct {
		Call struct {
			State string `json:"state"`
		} `json:"call"`
	}
	b, _ := json.Marshal(st)
	_ = json.Unmarshal(b, &state)
	if state.Call.State != "pending" {
		t.Fatalf("state = %s, want pending", state.Call.State)
	}
	if err := c.CancelCall(call.CallID); err != nil {
		t.Fatalf("cancel: %v", err)
	}

	// Error envelope: cancel again → 409 with the stable code.
	err = c.CancelCall(call.CallID)
	if !IsCode(err, "INVALID_STATE") {
		t.Fatalf("second cancel err = %v, want INVALID_STATE", err)
	}
	_ = stats
}
