// F1 PG-mode verification (GVOS_TEST_DSN-gated): the calls table
// lifecycle — create persists, signaling events advance the state in
// PG, a restart re-tracks live originations, and the list face
// filters by state.
package api

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestPGCallOriginationLifecycle(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("GVOS_TEST_DSN"))
	if dsn == "" {
		t.Skip("GVOS_TEST_DSN not set; skipping live-DB test")
	}
	ctx := context.Background()
	s1, err := NewPGServer(ctx, dsn)
	if err != nil {
		t.Fatalf("NewPGServer: %v", err)
	}
	if err := s1.pg.TestResetRuntimeState(ctx); err != nil {
		t.Fatal(err)
	}
	s1.acl.Reset(nil)
	t.Cleanup(func() {
		_ = s1.pg.TestResetRuntimeState(context.Background())
		s1.pg.Close()
	})
	srv1 := httptest.NewServer(MuxFor(s1))
	defer srv1.Close()

	// The origination plane requires a real API key (no legacy token).
	res := adminDo(t, srv1, "dev-admin-token", "POST", "/v1/admin/keys", `{"role":"operator"}`, "")
	var key struct {
		Key string `json:"key"`
	}
	if err := json.Unmarshal(res.Body.Bytes(), &key); err != nil || key.Key == "" {
		t.Fatalf("key mint failed: %d %s", res.Code, res.Body.String())
	}

	// Originate (seeded demo rates cover the caller prefix; account
	// "1000" resolves via the username index).
	res = adminDo(t, srv1, key.Key, "POST", "/v1/calls",
		`{"caller":"13800001234","callee":"13900005678","account_id":"1000","max_duration_s":600}`, "")
	if res.Code != 202 {
		t.Fatalf("create = %d body=%s", res.Code, res.Body.String())
	}
	var created struct {
		CallID string `json:"call_id"`
	}
	_ = json.Unmarshal(res.Body.Bytes(), &created)
	if created.CallID == "" {
		t.Fatalf("no call_id: %s", res.Body.String())
	}

	// Drive the machine through the signaling seams.
	if code, _ := s1.callStart(callStartReq{CallID: created.CallID, From: "1000", To: "13800001234"}); code != 200 {
		t.Fatalf("simulated start = %d", code)
	}
	if code, _ := s1.callAnswer(created.CallID); code != 200 {
		t.Fatalf("simulated answer = %d", code)
	}

	// Restart: the live origination is re-tracked (boot load).
	s2, err := NewPGServer(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	if !s2.isTracked(created.CallID) {
		t.Fatal("answered call not re-tracked after restart")
	}
	row, err := s2.pg.GetCall(ctx, created.CallID)
	if err != nil || row.State != "answered" {
		t.Fatalf("state across restart = %v / %v", row, err)
	}
	s2.pg.Close()

	// End: terminal in PG.
	if code, _ := s1.callEnd(ctx, callEndReq{CallID: created.CallID, DurSecs: 30}); code != 200 {
		t.Fatalf("simulated end = %d", code)
	}
	row, err = s1.pg.GetCall(ctx, created.CallID)
	if err != nil || row.State != "ended" {
		t.Fatalf("terminal state = %v / %v", row, err)
	}

	// List face filters by state.
	res = adminDo(t, srv1, "dev-admin-token", "GET", "/v1/calls?state=pending", "", "")
	var list struct {
		Count int `json:"count"`
	}
	_ = json.Unmarshal(res.Body.Bytes(), &list)
	if list.Count != 0 {
		t.Fatalf("pending filter = %d, want 0 (call ended)", list.Count)
	}
}
