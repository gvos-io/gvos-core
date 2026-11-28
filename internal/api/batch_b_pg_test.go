// Batch-B PG-mode verification (GVOS_TEST_DSN-gated, never runs in
// CI without a database): migration 014 + write-through persistence —
// API keys and AXB bindings survive a restart, gateway deletes stay
// deleted, the PG rates list is non-empty, and the idempotency
// registry replays from the table across a restart.
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func pgDSN(t *testing.T) string {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv("GVOS_TEST_DSN"))
	if dsn == "" {
		t.Skip("GVOS_TEST_DSN not set; skipping live-DB test")
	}
	return dsn
}

func TestPGPersistenceAcrossRestart(t *testing.T) {
	dsn := pgDSN(t)
	ctx := context.Background()

	// Server 1: boot. A shared throwaway DB may carry minted keys from
	// an earlier run — they would retire the legacy token for THIS
	// boot's acl too, so normalize before anything else.
	s1, err := NewPGServer(ctx, dsn)
	if err != nil {
		t.Fatalf("NewPGServer: %v", err)
	}
	if err := s1.pg.TestResetRuntimeState(ctx); err != nil {
		t.Fatalf("reset runtime state: %v", err)
	}
	s1.acl.Reset(nil)
	// Minted keys retire the legacy admin token for every later boot
	// on this database — wipe after the test so sibling PG tests keep
	// working. (Cleanup callbacks run after the test function's
	// defers, so the pool close lives here, not in a defer.)
	t.Cleanup(func() {
		if err := s1.pg.TestResetRuntimeState(context.Background()); err != nil {
			t.Logf("runtime state cleanup: %v", err)
		}
		s1.pg.Close()
	})
	srv1 := httptest.NewServer(MuxFor(s1))
	defer srv1.Close()

	// Rates: the PG backfill must surface the seeded price book (the
	// admin list used to always be empty in PG mode).
	s1.cfgMu.RLock()
	loadedRates := len(s1.rateList)
	s1.cfgMu.RUnlock()
	if loadedRates == 0 {
		t.Fatal("PG mode loaded no rates into rateList (backfill broken)")
	}
	if rec := adminGet(t, srv1, "dev-admin-token", "/v1/admin/rates"); rec.Code != 200 {
		t.Fatalf("GET rates = %d", rec.Code)
	} else {
		var body struct {
			Count int `json:"count"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		if body.Count != loadedRates {
			t.Fatalf("rates list count=%d, loaded=%d", body.Count, loadedRates)
		}
	}

	// 1. Mint a key (idempotency-keyed), then restart: the key must
	// authenticate on the new instance.
	idemKey := "pg-idem-" + time.Now().Format("150405.000000000")
	res := adminDo(t, srv1, "dev-admin-token", "POST", "/v1/admin/keys?x=1", `{"role":"operator"}`, idemKey)
	var created struct {
		Key    string `json:"key"`
		Prefix string `json:"prefix"`
	}
	if err := json.Unmarshal(res.Body.Bytes(), &created); err != nil || created.Key == "" {
		t.Fatalf("key create failed: code=%d body=%s", res.Code, res.Body.String())
	}

	s2, err := NewPGServer(ctx, dsn)
	if err != nil {
		t.Fatalf("restart: %v", err)
	}
	defer s2.pg.Close()
	srv2 := httptest.NewServer(MuxFor(s2))
	defer srv2.Close()
	if rec := adminGet(t, srv2, created.Key, "/v1/admin/keys"); rec.Code != 200 {
		t.Fatalf("minted key did not survive restart: status=%d body=%s", rec.Code, rec.Body.String())
	}

	// 2. Idempotent replay across the restart: same route + same
	// Idempotency-Key on server 2 must return the FIRST response
	// (same key material) from the idempotency_keys table.
	res2 := adminDo(t, srv2, created.Key, "POST", "/v1/admin/keys?x=1", `{"role":"agent"}`, idemKey)
	if res2.Code != 201 || res2.Header().Get("X-Idempotent-Replay") != "true" {
		t.Fatalf("cross-restart replay = %d replay=%q body=%s", res2.Code, res2.Header().Get("X-Idempotent-Replay"), res2.Body.String())
	}
	var replayed struct {
		Key string `json:"key"`
	}
	if err := json.Unmarshal(res2.Body.Bytes(), &replayed); err != nil || replayed.Key != created.Key {
		t.Fatalf("replay returned a different key material (minted twice?)")
	}

	// 3. AXB binding survives a restart.
	res3 := adminDo(t, srv1, created.Key, "POST", "/v1/admin/axb/bindings", `{"a_number":"13800001234","b_number":"13900005678","ttl_hours":2}`, "")
	var bind struct {
		XNumber string `json:"x_number"`
	}
	if err := json.Unmarshal(res3.Body.Bytes(), &bind); err != nil || bind.XNumber == "" {
		t.Fatalf("axb bind failed: code=%d body=%s", res3.Code, res3.Body.String())
	}
	s3, err := NewPGServer(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer s3.pg.Close()
	if bd, ok := s3.axb.Resolve(bind.XNumber); !ok || bd.BNumber != "13900005678" {
		t.Fatalf("AXB binding lost across restart: ok=%v bd=%+v", ok, bd)
	}

	// 4. Gateway delete persists (it used to resurrect on restart).
	adminDo(t, srv1, created.Key, "PUT", "/v1/admin/gateways",
		`{"gateways":[{"id":"pg-del-gw","name":"pg-del-gw","sip_addr":"pg-gw:5060","protocol":"udp","priority":5,"weight":100,"enabled":true}]}`, "")
	adminDo(t, srv1, created.Key, "DELETE", "/v1/admin/gateways/pg-del-gw", "", "")
	s4, err := NewPGServer(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer s4.pg.Close()
	for _, g := range s4.gws.Gateways() {
		if g.ID == "pg-del-gw" {
			t.Fatal("deleted gateway resurrected after restart")
		}
	}
}

func adminGet(t *testing.T, srv *httptest.Server, token, path string) *httptest.ResponseRecorder {
	return adminDo(t, srv, token, "GET", path, "", "")
}

// adminDo issues one authenticated request against the test server.
func adminDo(t *testing.T, srv *httptest.Server, token, method, path, body, idemKey string) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if body != "" {
		buf.WriteString(body)
	}
	req, err := http.NewRequest(method, srv.URL+path, &buf)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if idemKey != "" {
		req.Header.Set("Idempotency-Key", idemKey)
	}
	res, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	rec := httptest.NewRecorder()
	_, _ = rec.Body.ReadFrom(res.Body)
	rec.Code = res.StatusCode
	for k, vs := range res.Header {
		for _, v := range vs {
			rec.Header().Add(k, v)
		}
	}
	return rec
}
