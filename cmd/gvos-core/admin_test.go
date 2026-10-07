package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gvos/gvos-core/internal/account"
)

// TestAdminAuth guards all admin routes.
func TestAdminAuth(t *testing.T) {
	s := NewServer()
	srv := httptest.NewServer(muxFor(s))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v1/admin/accounts")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Errorf("no token: code = %d, want 401", resp.StatusCode)
	}

	req, _ := http.NewRequest("GET", srv.URL+"/v1/admin/accounts", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Errorf("wrong token: code = %d, want 401", resp.StatusCode)
	}
}

// TestAdminAccountLifecycle: create -> list -> topup -> verify live
// balance is used by the call path (memory mode).
func TestAdminAccountLifecycle(t *testing.T) {
	s := NewServer()
	srv := httptest.NewServer(muxFor(s))
	defer srv.Close()

	authed := func(method, path string, body any) (int, map[string]any) {
		t.Helper()
		b, _ := json.Marshal(body)
		req, _ := http.NewRequest(method, srv.URL+path, bytes.NewReader(b))
		req.Header.Set("Authorization", "Bearer "+adminToken)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		defer resp.Body.Close()
		var m map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&m)
		return resp.StatusCode, m
	}

	// 1. Create an account under agent1.
	code, m := authed("POST", "/v1/admin/accounts", map[string]any{
		"name": "New User", "username": "2000", "credit": 1.0,
	})
	if code != 201 {
		t.Fatalf("create: code=%d body=%v", code, m)
	}
	newID, _ := m["id"].(string)
	if newID == "" {
		t.Fatal("create returned empty id")
	}

	// 2. It shows up in the list.
	code, m = authed("GET", "/v1/admin/accounts", nil)
	if code != 200 {
		t.Fatalf("list: code=%d", code)
	}
	if m["count"].(float64) < 4 {
		t.Errorf("account count = %v, want >=4", m["count"])
	}

	// 3. Top up +5.
	code, m = authed("POST", "/v1/admin/topup", map[string]any{
		"account_id": newID, "delta": 5.0, "reason": "topup",
	})
	if code != 200 {
		t.Fatalf("topup: code=%d body=%v", code, m)
	}
	if m["balance"].(float64) != 6.0 {
		t.Errorf("balance after topup = %v, want 6", m["balance"])
	}

	// 4. The new account can start a billable call immediately (live
	// snapshot semantics).
	code, m = authed("POST", "/v1/call/start", map[string]any{
		"call_id": "admin-e2e-1", "from_user": newID, "to_user": "8613800138000",
	})
	if code != 200 {
		t.Fatalf("call start with new account: code=%d body=%v", code, m)
	}
	code, m = authed("POST", "/v1/call/end", map[string]any{
		"call_id": "admin-e2e-1", "duration": 66,
	})
	if code != 200 {
		t.Fatalf("call end: code=%d body=%v", code, m)
	}
	// 6.00 - 0.055 (66s at 0.05/min 60+6)
	bal, _ := account.Balance(s.store, newID)
	if !approxEqual(bal, 6-0.055, 1e-7) {
		t.Errorf("post-call balance = %.7f, want %.7f", bal, 6-0.055)
	}
}

// TestAdminRateUpsertLiveEffect: a rate change reflects in the next
// call without restart (memory mode).
func TestAdminRateUpsertLiveEffect(t *testing.T) {
	s := NewServer()
	srv := httptest.NewServer(muxFor(s))
	defer srv.Close()

	authedPost := func(path string, body any) (int, map[string]any) {
		t.Helper()
		b, _ := json.Marshal(body)
		req, _ := http.NewRequest("POST", srv.URL+path, bytes.NewReader(b))
		req.Header.Set("Authorization", "Bearer "+adminToken)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var m map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&m)
		return resp.StatusCode, m
	}

	// Halve the CN mobile price: 0.05 -> 0.025.
	code, m := authedPost("/v1/admin/rates", map[string]any{
		"prefix": "8613", "price": 0.025,
	})
	if code != 200 {
		t.Fatalf("rate upsert: code=%d body=%v", code, m)
	}

	// A 66s call now costs 0.0275.
	code, m = authedPost("/v1/call/start", map[string]any{
		"call_id": "rate-e2e-1", "from_user": "1000", "to_user": "8613800138000",
	})
	if code != 200 {
		t.Fatalf("start: code=%d body=%v", code, m)
	}
	if m["price"].(float64) != 0.025 {
		t.Errorf("new price not applied: %v", m["price"])
	}
	code, m = authedPost("/v1/call/end", map[string]any{
		"call_id": "rate-e2e-1", "duration": 66,
	})
	if code != 200 {
		t.Fatalf("end: code=%d", code)
	}
	if m["Cost"].(float64) != 0.0275 {
		t.Errorf("cost after rate change = %v, want 0.0275", m["Cost"])
	}
}

// TestAdminValidation: bad inputs get clean 400s.
func TestAdminValidation(t *testing.T) {
	s := NewServer()
	srv := httptest.NewServer(muxFor(s))
	defer srv.Close()

	authedPost := func(path string, body any) int {
		b, _ := json.Marshal(body)
		req, _ := http.NewRequest("POST", srv.URL+path, bytes.NewReader(b))
		req.Header.Set("Authorization", "Bearer "+adminToken)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}

	if code := authedPost("/v1/admin/accounts", map[string]any{"name": "no-username"}); code != 400 {
		t.Errorf("create without username: %d, want 400", code)
	}
	if code := authedPost("/v1/admin/topup", map[string]any{"account_id": "x", "delta": 0}); code != 400 {
		t.Errorf("topup zero delta: %d, want 400", code)
	}
	if code := authedPost("/v1/admin/rates", map[string]any{"prefix": "", "price": 1}); code != 400 {
		t.Errorf("rate empty prefix: %d, want 400", code)
	}
	if code := authedPost("/v1/admin/rates", map[string]any{"prefix": "86", "price": -1}); code != 400 {
		t.Errorf("rate negative price: %d, want 400", code)
	}
}
