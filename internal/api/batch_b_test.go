// Batch-B regression tests (memory mode): the idempotency namespace —
// the same client key on two routes must not cross-replay (the old
// flat global namespace reused one response across routes), and a
// same-route retry must replay the first response verbatim.
package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// idemBearer is the credential the test currently presents (minting
// the first API key retires the legacy token, so the helper must
// switch to the fresh key).
var idemBearer = "dev-admin-token"

func idemReq(t *testing.T, method, path string, body any, clientKey string) *http.Request {
	t.Helper()
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(body); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Authorization", "Bearer "+idemBearer)
	req.Header.Set("Idempotency-Key", clientKey)
	return req
}

func TestIdempotencyNamespacedByRoute(t *testing.T) {
	s := NewServer()
	h := MuxFor(s)
	const key = "test-idem-key-1"
	defer func() { idemBearer = "dev-admin-token" }()

	// Route 1: mint an API key under client key K.
	rec1 := httptest.NewRecorder()
	h.ServeHTTP(rec1, idemReq(t, "POST", "/v1/admin/keys", map[string]any{"role": "operator"}, key))
	if rec1.Code != 201 {
		t.Fatalf("first keys create = %d body=%s", rec1.Code, rec1.Body.String())
	}
	var first struct {
		Key string `json:"key"`
	}
	if err := json.Unmarshal(rec1.Body.Bytes(), &first); err != nil || first.Key == "" {
		t.Fatalf("first response not a key create: err=%v body=%s", err, rec1.Body.String())
	}

	// Minting the first key retires the legacy token (Phase B): all
	// further calls in this test authenticate with the new key.
	idemBearer = first.Key

	// Route 2 (different path), same client key K: must execute fresh,
	// not replay the key-create response from route 1.
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, idemReq(t, "POST", "/v1/admin/accounts", map[string]any{"username": "idem-a", "initial_balance": "10.00"}, key))
	if rec2.Code != 201 {
		t.Fatalf("accounts create with reused key = %d body=%s", rec2.Code, rec2.Body.String())
	}
	var acct struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec2.Body.Bytes(), &acct); err != nil || acct.ID == "" {
		t.Fatalf("route 2 replayed a foreign response: err=%v body=%s", err, rec2.Body.String())
	}

	// Route 1 again, same client key: verbatim replay of the FIRST
	// response (X-Idempotent-Replay, same key material — retries must
	// not mint a second credential).
	rec3 := httptest.NewRecorder()
	h.ServeHTTP(rec3, idemReq(t, "POST", "/v1/admin/keys", map[string]any{"role": "operator"}, key))
	if rec3.Code != 201 || rec3.Header().Get("X-Idempotent-Replay") != "true" {
		t.Fatalf("replay = %d replay-header=%q", rec3.Code, rec3.Header().Get("X-Idempotent-Replay"))
	}
	var replay struct {
		Key string `json:"key"`
	}
	if err := json.Unmarshal(rec3.Body.Bytes(), &replay); err != nil {
		t.Fatal(err)
	}
	if replay.Key != first.Key {
		t.Fatal("replay did not return the first response (minted a second key?)")
	}
}
