// Regression tests for the batch-A P0 fixes (api-gap-analysis.md §3):
// recording GET used to 405, blocklist DELETE could not lift white
// entries, DID PUT defaulted enabled=false (silent unroutable), and
// memory-mode webhook creates evaporated before ever delivering.
package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func doAdmin(t *testing.T, h http.Handler, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatal(err)
		}
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Authorization", "Bearer dev-admin-token")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestRecordingGetReportsToggleState(t *testing.T) {
	s := NewServer()
	h := MuxFor(s)

	// The query half used to fall through to requirePOST (405).
	if rec := doAdmin(t, h, "GET", "/v1/admin/recording", nil); rec.Code != 200 {
		t.Fatalf("GET recording = %d, want 200", rec.Code)
	}
	if rec := doAdmin(t, h, "POST", "/v1/admin/recording", map[string]any{"enabled": true}); rec.Code != 200 {
		t.Fatalf("POST recording = %d, want 200", rec.Code)
	}
	rec := doAdmin(t, h, "GET", "/v1/admin/recording", nil)
	var body struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if !body.Enabled {
		t.Fatal("GET after enable reported enabled=false")
	}
}

func TestBlocklistWhiteThreePartDelete(t *testing.T) {
	s := NewServer()
	h := MuxFor(s)

	add := func(kind, scope, pattern string) {
		rec := doAdmin(t, h, "POST", "/v1/admin/blocklist-entries", map[string]any{
			"kind": kind, "scope": scope, "pattern": pattern,
		})
		if rec.Code != 201 {
			t.Fatalf("add %s %s = %d", kind, pattern, rec.Code)
		}
	}
	add("white", "dest", "1800")
	add("black", "dest", "1900")

	// GET must list real entries (it used to return only a count).
	rec := doAdmin(t, h, "GET", "/v1/admin/blocklist-entries", nil)
	var list struct {
		Items []struct{ Kind, Scope, Pattern string } `json:"items"`
		Count int                                     `json:"count"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if list.Count != 2 || len(list.Items) != 2 {
		t.Fatalf("GET items = %+v, want 2 entries", list)
	}

	// Three-part id lifts a white entry (used to be impossible: kind
	// was hardcoded black).
	if rec := doAdmin(t, h, "DELETE", "/v1/admin/blocklist-entries/white:dest:1800", nil); rec.Code != 200 {
		t.Fatalf("DELETE white = %d, want 200", rec.Code)
	}
	// Two-part id still defaults to black.
	if rec := doAdmin(t, h, "DELETE", "/v1/admin/blocklist-entries/dest:1900", nil); rec.Code != 200 {
		t.Fatalf("DELETE black 2-part = %d, want 200", rec.Code)
	}
	if s.blocklist.Count() != 0 {
		t.Fatalf("count after deletes = %d, want 0", s.blocklist.Count())
	}
	if rec := doAdmin(t, h, "DELETE", "/v1/admin/blocklist-entries/bogus:x", nil); rec.Code != 400 {
		t.Fatalf("malformed id = %d, want 400", rec.Code)
	}
}

func TestDIDPutEnabledDefaultsTrue(t *testing.T) {
	s := NewServer()
	h := MuxFor(s)

	// Omitted enabled used to register as false: 201 but Resolve
	// never matched — a silently unroutable DID.
	if rec := doAdmin(t, h, "POST", "/v1/admin/dids", map[string]any{
		"did_number": "8613900", "account_id": "acc-1", "destination_type": "external", "destination": "8613900",
	}); rec.Code != 200 {
		t.Fatalf("PUT did = %d", rec.Code)
	}
	if _, ok := s.dids.Resolve("8613900"); !ok {
		t.Fatal("DID registered without enabled is not routable; want default true")
	}
	if rec := doAdmin(t, h, "POST", "/v1/admin/dids", map[string]any{
		"did_number": "8613901", "account_id": "acc-1", "destination_type": "external", "destination": "8613901", "enabled": false,
	}); rec.Code != 200 {
		t.Fatalf("PUT did disabled = %d", rec.Code)
	}
	if _, ok := s.dids.Resolve("8613901"); ok {
		t.Fatal("explicit enabled=false was overridden")
	}
}

func TestWebhookMemoryModeSubscriptions(t *testing.T) {
	s := NewServer()
	StartWebhookFanout(s)
	h := MuxFor(s)

	rec := doAdmin(t, h, "POST", "/v1/admin/webhooks", map[string]any{
		"url": "https://8.8.8.8/hook", "events": []string{"gvos.call.teardown"},
	})
	if rec.Code != 201 {
		t.Fatalf("create = %d", rec.Code)
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}

	// The subscription used to evaporate: list empty, dispatcher empty.
	if got := len(s.whDispatch.SubscriptionCount()); got != 1 {
		t.Fatalf("dispatcher subscriptions = %d, want 1", got)
	}
	for _, sub := range s.whDispatch.SubscriptionCount() {
		if sub.ID != created.ID || sub.URL != "https://8.8.8.8/hook" || sub.Secret == "" {
			t.Fatalf("subscription = %+v", sub)
		}
	}
	rec = doAdmin(t, h, "GET", "/v1/admin/webhooks", nil)
	var list struct {
		Webhooks []map[string]any `json:"webhooks"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Webhooks) != 1 {
		t.Fatalf("list = %+v, want 1", list.Webhooks)
	}
	if _, hasSecret := list.Webhooks[0]["secret"]; hasSecret {
		t.Fatal("list leaked the one-time secret")
	}

	if rec := doAdmin(t, h, "DELETE", "/v1/admin/webhooks/wh-nonexistent", nil); rec.Code != 404 {
		t.Fatalf("delete unknown = %d, want 404 (memory mode used to 200)", rec.Code)
	}
	if rec := doAdmin(t, h, "DELETE", "/v1/admin/webhooks/"+created.ID, nil); rec.Code != 200 {
		t.Fatalf("delete = %d", rec.Code)
	}
	if got := len(s.whDispatch.SubscriptionCount()); got != 0 {
		t.Fatalf("dispatcher subscriptions after delete = %d, want 0", got)
	}
}
