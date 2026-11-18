package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/lixuanqun/gvos-core/internal/migrate"
)

// mockCore is a minimal stand-in for the gv-core admin API: enough
// bookkeeping to verify what load does (creates accounts, tops up
// credits, upserts rates, answers call/start with the rate).
type mockCore struct {
	mu       sync.Mutex
	accounts map[string]map[string]any // id -> account
	byUser   map[string]string         // username -> id
	rates    map[string]float64        // prefix -> price
	nextID   int
}

func newMockCore() *mockCore {
	return &mockCore{
		accounts: map[string]map[string]any{},
		byUser:   map[string]string{},
		rates:    map[string]float64{},
	}
}

func (m *mockCore) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The event plane (call/start) is Kamailio-internal: no token.
		// Admin plane requires the bearer token.
		if r.URL.Path != "/v1/call/start" && r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(401)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		switch r.URL.Path {
		case "/v1/admin/accounts":
			if r.Method != "POST" {
				w.WriteHeader(405)
				return
			}
			m.mu.Lock()
			m.nextID++
			id := fmt.Sprintf("id-%d", m.nextID)
			user, _ := body["username"].(string)
			m.accounts[id] = body
			m.byUser[user] = id
			m.mu.Unlock()
			write(w, 201, map[string]any{"id": id, "username": user})
		case "/v1/admin/topup":
			m.mu.Lock()
			id, _ := body["account_id"].(string)
			delta, _ := body["delta"].(float64)
			acc := m.accounts[id]
			var bal float64
			if acc != nil {
				cur, _ := acc["credit"].(float64) // absent/nil -> 0
				bal = cur + delta
				acc["credit"] = bal
			}
			m.mu.Unlock()
			write(w, 200, map[string]any{"account_id": id, "balance": bal})
		case "/v1/admin/rates":
			m.mu.Lock()
			prefix, _ := body["prefix"].(string)
			price, _ := body["price"].(float64)
			m.rates[prefix] = price
			m.mu.Unlock()
			write(w, 200, map[string]any{"prefix": prefix, "price": price})
		case "/v1/call/start":
			m.mu.Lock()
			dest, _ := body["to_user"].(string)
			var entry map[string]any
			for i := len(dest); i > 0; i-- {
				if p, ok := m.rates[dest[:i]]; ok {
					entry = map[string]any{"rate_prefix": dest[:i], "price": p}
					break
				}
			}
			m.mu.Unlock()
			if entry == nil {
				write(w, 404, map[string]string{"error": "no rate"})
				return
			}
			entry["call_id"] = body["call_id"]
			write(w, 200, entry)
		default:
			w.WriteHeader(404)
		}
	})
}

func write(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// TestLoadEndToEnd: sample VOS CSVs -> extract -> validate -> load ->
// verify accounts/rates landed and the migrated user can bill.
func TestLoadEndToEnd(t *testing.T) {
	dir := t.TempDir()
	acctPath := filepath.Join(dir, "a.csv")
	ratePath := filepath.Join(dir, "r.csv")
	gwPath := filepath.Join(dir, "g.csv")

	os.WriteFile(acctPath, []byte("账户,名称,上级账户,费率组,余额\nagent_a,华北代理,,wholesale,500.5\nuser_1,客户甲,agent_a,retail,86.42\n"), 0o644)
	os.WriteFile(ratePath, []byte("前缀,费率,计费步长\n86,0.0600000,60+6\n8613,0.0500000,60+6\n"), 0o644)
	os.WriteFile(gwPath, []byte("网关,地址\ngw1,10.0.0.11:5060\n"), 0o644)

	af, _ := os.Open(acctPath)
	accs, iss, err := migrate.ParseAccounts(af)
	af.Close()
	if err != nil || len(accs) != 2 || len(iss) != 0 {
		t.Fatalf("extract accounts: %v accs=%v iss=%v", err, accs, iss)
	}
	rf, _ := os.Open(ratePath)
	rates, _, err := migrate.ParseRates(rf)
	rf.Close()
	if err != nil || len(rates) != 2 {
		t.Fatalf("extract rates: %v %v", err, rates)
	}
	gf, _ := os.Open(gwPath)
	gws, _, err := migrate.ParseGateways(gf)
	gf.Close()
	if err != nil || len(gws) != 1 {
		t.Fatalf("extract gateways: %v %v", err, gws)
	}

	viss, _ := migrate.Validate(accs, rates, gws)
	for _, i := range viss {
		if i.Sev == "error" {
			t.Fatalf("validate error: %s", i.Msg)
		}
	}

	// load against the mock core (same request shapes as cmdLoad).
	core := newMockCore()
	srv := httptest.NewServer(core.handler())
	defer srv.Close()

	created, err := loadAccounts(srv.URL, "tok", accs)
	if err != nil || created != 2 {
		t.Fatalf("loadAccounts: %v created=%d", err, created)
	}
	for _, r := range rates {
		if err := upsertRate(srv.URL, "tok", r); err != nil {
			t.Fatalf("upsert rate %s: %v", r.Prefix, err)
		}
	}

	// Balances and rates landed (release the lock before the HTTP call
	// below - the mock handler takes the same mutex).
	core.mu.Lock()
	if got, _ := core.accounts[core.byUser["user_1"]]["credit"].(float64); got != 86.42 {
		t.Errorf("user_1 credit = %v, want 86.42", got)
	}
	if got, _ := core.accounts[core.byUser["agent_a"]]["credit"].(float64); got != 500.5 {
		t.Errorf("agent_a credit = %v, want 500.5", got)
	}
	if core.rates["8613"] != 0.05 {
		t.Errorf("rate 8613 = %v", core.rates["8613"])
	}
	core.mu.Unlock()

	// Migrated rate answers a call-start (longest prefix).
	body, _ := json.Marshal(map[string]string{
		"call_id": "mig-e2e-1", "from_user": "user_1", "to_user": "8613800138000",
	})
	resp, err := http.Post(srv.URL+"/v1/call/start", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var m map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&m)
	if resp.StatusCode != 200 || m["rate_prefix"] != "8613" || m["price"] != 0.05 {
		t.Fatalf("call start: %d %v", resp.StatusCode, m)
	}
}

// TestLoadRejectsInvalidSnapshot: orphan accounts never reach load.
func TestLoadRejectsInvalidSnapshot(t *testing.T) {
	accs := []migrate.VOSAccount{{Username: "x", Parent: "ghost"}}
	iss, _ := migrate.Validate(accs, nil, nil)
	blocked := false
	for _, i := range iss {
		if i.Sev == "error" {
			blocked = true
		}
	}
	if !blocked {
		t.Fatal("orphan must produce an error, blocking load")
	}
}

// TestNegativeBalanceImportsAsZero: debt accounts load without topup.
func TestNegativeBalanceImportsAsZero(t *testing.T) {
	core := newMockCore()
	srv := httptest.NewServer(core.handler())
	defer srv.Close()

	created, err := loadAccounts(srv.URL, "tok", []migrate.VOSAccount{{Username: "debt", Credit: -3.7}})
	if err != nil || created != 1 {
		t.Fatalf("loadAccounts: %v created=%d", err, created)
	}
	core.mu.Lock()
	defer core.mu.Unlock()
	id := core.byUser["debt"]
	if acc, ok := core.accounts[id]["credit"].(float64); ok && acc != 0 {
		// credit key absent means no topup was issued - correct.
		t.Errorf("debt account got a topup: credit=%v", acc)
	}
}

// ---- helpers shared with cmdLoad's request shapes ----

func loadAccounts(base, token string, accs []migrate.VOSAccount) (int, error) {
	client := &http.Client{}
	n := 0
	for _, a := range accs {
		body := map[string]any{
			"username": a.Username, "name": pick(a.Name, a.Username),
			"rate_group": pick(a.RateGroup, "demo"),
		}
		id, err := postJSON(client, base+"/v1/admin/accounts", token, body)
		if err != nil {
			return n, err
		}
		if a.Credit > 0 {
			tb := map[string]any{
				"account_id": id, "delta": a.Credit, "reason": "vos-migration",
			}
			if _, err := postJSON(client, base+"/v1/admin/topup", token, tb); err != nil {
				return n, err
			}
		}
		n++
	}
	return n, nil
}

func upsertRate(base, token string, r migrate.VOSRate) error {
	body := map[string]any{
		"prefix": r.Prefix, "price": r.Price, "init_dur": r.InitDur, "step_dur": r.StepDur,
	}
	_, err := postJSON(&http.Client{}, base+"/v1/admin/rates", token, body)
	return err
}
