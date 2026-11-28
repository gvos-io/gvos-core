// Batch-C regression tests (memory mode): single-account GET, the
// unified list envelope (items/next_cursor present), stats counts
// without the dead caller_bal field, and server-side CDR filtering.
package api

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/gvos-io/gvos-core/internal/billingdomain/account"
	"github.com/gvos-io/gvos-core/internal/billingdomain/money"
)

func TestAccountGetSingle(t *testing.T) {
	s := NewServer()
	h := MuxFor(s)

	rec := doAdmin(t, h, "POST", "/v1/admin/accounts", map[string]any{"username": "get-me", "initial_balance": "5.00"})
	if rec.Code != 201 {
		t.Fatalf("create = %d body=%s", rec.Code, rec.Body.String())
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil || created.ID == "" {
		t.Fatalf("no id in create response: %s", rec.Body.String())
	}

	rec = doAdmin(t, h, "GET", "/v1/admin/accounts/"+created.ID, nil)
	if rec.Code != 200 {
		t.Fatalf("GET by id = %d body=%s", rec.Code, rec.Body.String())
	}
	var got struct {
		Account struct {
			ID       string `json:"id"`
			Username string `json:"username"`
		} `json:"account"`
		Item json.RawMessage `json:"item"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Account.ID != created.ID || got.Account.Username != "get-me" {
		t.Fatalf("account = %+v", got.Account)
	}
	if len(got.Item) == 0 {
		t.Fatal("item envelope missing")
	}

	// Username lookup resolves through the user index.
	rec = doAdmin(t, h, "GET", "/v1/admin/accounts/get-me", nil)
	if rec.Code != 200 {
		t.Fatalf("GET by username = %d", rec.Code)
	}
	if rec := doAdmin(t, h, "GET", "/v1/admin/accounts/acc-none", nil); rec.Code != 404 {
		t.Fatalf("GET unknown = %d, want 404", rec.Code)
	}
}

func TestCDRListFiltersMemoryRing(t *testing.T) {
	s := NewServer()
	h := MuxFor(s)

	// Two completed calls to different destinations.
	for _, dest := range []string{"8613800138000", "8613900139000"} {
		if code, _ := s.callStart(callStartReq{CallID: "cdrf-" + dest, From: "1000", To: dest}); code != 200 {
			t.Fatalf("start %s: code=%d", dest, code)
		}
		if code, _ := s.callEnd(nil, callEndReq{CallID: "cdrf-" + dest, DurSecs: 60}); code != 200 {
			t.Fatalf("end %s: code=%d", dest, code)
		}
	}

	// No filter: both rows, items envelope present.
	rec := doAdmin(t, h, "GET", "/v1/admin/cdrs?limit=10", nil)
	var all struct {
		Count      int             `json:"count"`
		Items      json.RawMessage `json:"items"`
		NextCursor string          `json:"next_cursor"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &all); err != nil {
		t.Fatal(err)
	}
	if all.Count != 2 || len(all.Items) == 0 {
		t.Fatalf("unfiltered count=%d items=%d", all.Count, len(all.Items))
	}

	// dest filter narrows to one.
	rec = doAdmin(t, h, "GET", "/v1/admin/cdrs?dest=8613900139000", nil)
	var one struct {
		Count int `json:"count"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &one)
	if one.Count != 1 {
		t.Fatalf("dest filter count=%d, want 1", one.Count)
	}

	// account_id filter matching nothing returns zero rows (the param
	// used to be silently ignored and returned everything).
	rec = doAdmin(t, h, "GET", "/v1/admin/cdrs?account_id=acc-nope", nil)
	_ = json.Unmarshal(rec.Body.Bytes(), &one)
	if one.Count != 0 {
		t.Fatalf("account filter count=%d, want 0", one.Count)
	}

	// Time window strictly after the calls: zero rows (+2 days is a
	// future calendar date regardless of the local hour).
	future := time.Now().UTC().AddDate(0, 0, 2).Format("2006-01-02")
	rec = doAdmin(t, h, "GET", "/v1/admin/cdrs?from="+future, nil)
	_ = json.Unmarshal(rec.Body.Bytes(), &one)
	if one.Count != 0 {
		t.Fatalf("from-filter count=%d, want 0", one.Count)
	}

	// Bad time format: targeted 400.
	if rec := doAdmin(t, h, "GET", "/v1/admin/cdrs?from=nope", nil); rec.Code != 400 {
		t.Fatalf("bad from = %d, want 400", rec.Code)
	}
}

func TestStatsCountsNoCallerBal(t *testing.T) {
	s := NewServer()
	h := MuxFor(s)
	rec := doAdmin(t, h, "GET", "/v1/stats", nil)
	if rec.Code != 200 {
		t.Fatalf("stats = %d", rec.Code)
	}
	var body struct {
		Counts struct {
			Accounts      int `json:"accounts"`
			Gateways      int `json:"gateways"`
			Registrations int `json:"registrations"`
			Tenants       int `json:"tenants"`
		} `json:"counts"`
		CallerBal any `json:"caller_bal"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Counts.Accounts <= 0 || body.Counts.Gateways <= 0 || body.Counts.Tenants <= 0 {
		t.Fatalf("counts = %+v, want populated", body.Counts)
	}
	if body.CallerBal != nil {
		t.Fatal("caller_bal must be gone (hardcoded demo account, dead in PG mode)")
	}
}

func TestListEnvelopeItemsAliases(t *testing.T) {
	s := NewServer()
	h := MuxFor(s)
	doAdmin(t, h, "POST", "/v1/admin/dids", map[string]any{
		"did_number": "8613900", "account_id": "acc-1", "destination_type": "external", "destination": "8613900",
	})
	for _, path := range []string{
		"/v1/admin/dids", "/v1/admin/registrations", "/v1/admin/risk-rules",
		"/v1/admin/gateways", "/v1/admin/webhooks", "/v1/admin/axb/bindings",
		"/v1/admin/keys",
	} {
		rec := doAdmin(t, h, "GET", path, nil)
		if rec.Code != 200 {
			t.Fatalf("GET %s = %d", path, rec.Code)
		}
		var body map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if _, ok := body["items"]; !ok {
			t.Fatalf("GET %s: unified items envelope missing (keys: %v)", path, body)
		}
		if _, ok := body["count"]; !ok {
			t.Fatalf("GET %s: count missing", path)
		}
	}
}

func TestAccountDisableAdmission(t *testing.T) {
	s := NewServer()
	h := MuxFor(s)

	// Disable via PATCH (memory: default Enabled=true on creation).
	rec := doAdmin(t, h, "PATCH", "/v1/admin/accounts/1000", map[string]any{"enabled": false})
	if rec.Code != 200 {
		t.Fatalf("patch disable = %d body=%s", rec.Code, rec.Body.String())
	}
	// Admission: 403 DISABLED, balance untouched.
	code, body := s.callStart(callStartReq{CallID: "dis-1", From: "1000", To: "8613800138000"})
	if code != 403 {
		t.Fatalf("callStart on disabled = %d, want 403", code)
	}
	if m, ok := body.(map[string]errorEnvelope); !ok || m["error"].Code != "DISABLED" {
		t.Fatalf("disabled body = %v", body)
	}
	// Re-enable: instant.
	if rec := doAdmin(t, h, "PATCH", "/v1/admin/accounts/1000", map[string]any{"enabled": true}); rec.Code != 200 {
		t.Fatalf("re-enable = %d", rec.Code)
	}
	if code, _ := s.callStart(callStartReq{CallID: "dis-2", From: "1000", To: "8613800138000"}); code != 200 {
		t.Fatalf("callStart after re-enable = %d, want 200", code)
	}
	if code, _ := s.callEnd(nil, callEndReq{CallID: "dis-2", DurSecs: 1}); code != 200 {
		t.Fatalf("end = %d", code)
	}
	// Balance survived the disabled window.
	if bal, err := account.Balance(s.store, "1000"); err != nil || bal == money.MustFromYuan(10) {
		t.Fatalf("balance drifted during disable: %s", bal.String())
	}
}
