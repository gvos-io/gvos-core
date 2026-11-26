package api

// Integration test against a live PostgreSQL (GVOS_TEST_DSN), for the
// PG-without-Redis deployment shape: the in-memory balance is the
// authority and accounts.credit mirrors it.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gvos-io/gvos-core/internal/billingdomain/account"
	"github.com/gvos-io/gvos-core/internal/billingdomain/money"
)

// pgCredit is what a restart would load for the account.
func pgCredit(t *testing.T, s *Server, id string) money.Amount {
	t.Helper()
	acc := account.NewStore()
	if _, err := s.pg.LoadAccounts(context.Background(), acc); err != nil {
		t.Fatal(err)
	}
	bal, err := account.Balance(acc, id)
	if err != nil {
		t.Fatal(err)
	}
	return bal
}

// Money spent on calls must reach accounts.credit: it used to be
// touched by top-ups only, so a restart — or the next top-up — handed
// back everything the account had spent since.
func TestPGWithoutRedisKeepsCreditAcrossRestartAndTopup(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("GVOS_TEST_DSN"))
	if dsn == "" {
		t.Skip("GVOS_TEST_DSN not set; skipping live-DB test")
	}
	ctx := context.Background()
	seed, err := NewPGServer(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	uname := "restart" + time.Now().Format("150405.000000000")
	id, err := seed.pg.CreateAccountWithHA1(ctx, demoTenant, "00000000-0000-0000-0000-0000000000a2",
		"audit", uname, strings.Repeat("0", 32), "demo", false, money.MustFromYuan(10))
	if err != nil {
		t.Fatal(err)
	}
	// Process start.
	s, err := NewPGServer(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	const calls = 5
	for i := 0; i < calls; i++ {
		cid := uname + "-" + string(rune('a'+i))
		if code, _ := s.callStart(callStartReq{CallID: cid, From: uname, To: "8613800138000"}); code != 200 {
			t.Fatalf("start %d: code = %d", i, code)
		}
		if code, _ := s.callEnd(ctx, callEndReq{CallID: cid, DurSecs: 60}); code != 200 {
			t.Fatalf("end %d: code = %d", i, code)
		}
	}
	live, _ := account.Balance(s.store, id)
	if live != money.MustFromYuan(10-0.05*calls) {
		t.Fatalf("live balance = %s, want %s", live, money.MustFromYuan(10-0.05*calls))
	}
	if got := pgCredit(t, s, id); got != live {
		t.Fatalf("accounts.credit = %s, want %s (a restart would hand back the difference)", got, live)
	}

	// A top-up moves the live balance by its delta; the mirror follows.
	mux := MuxFor(s)
	req, _ := http.NewRequest("POST", "/v1/admin/accounts/"+id+"/topup", strings.NewReader(`{"delta":1,"reason":"topup"}`))
	req.Header.Set("Authorization", "Bearer "+adminToken)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("topup: code = %d body = %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Balance money.Amount `json:"balance"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Balance != live.Add(money.MustFromYuan(1)) {
		t.Errorf("top-up answered %s, want %s", resp.Balance, live.Add(money.MustFromYuan(1)))
	}
	if got := pgCredit(t, s, id); got != resp.Balance {
		t.Errorf("accounts.credit after top-up = %s, want %s", got, resp.Balance)
	}
}
