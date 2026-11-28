package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"

	"github.com/gvos-io/gvos-core/internal/billingdomain/account"
	"github.com/gvos-io/gvos-core/internal/billingdomain/money"
	"github.com/gvos-io/gvos-core/internal/callcore"
	"github.com/gvos-io/gvos-core/internal/shared/acl"
	"github.com/gvos-io/gvos-core/internal/shared/rds"
)

// Regression tests for the 2026-11 billing audit, at the API seam
// (the HTTP face and the NATS service face share these functions).

// outageAuthority is a balance authority that always fails the same way.
type outageAuthority struct{ err error }

func (a outageAuthority) ReserveCtx(context.Context, string, money.Amount) (string, error) {
	return "", a.err
}
func (a outageAuthority) CommitCtx(context.Context, string, string, money.Amount, money.Amount) error {
	return a.err
}
func (a outageAuthority) GetBalance(context.Context, string) (money.Amount, error) {
	return 0, a.err
}

func postCall(t *testing.T, mux http.Handler, path, body string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest("POST", path, strings.NewReader(body))
	req.Header.Set("X-GVOS-Internal", os.Getenv("GVOS_INTERNAL_TOKEN"))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

// An end event for an unanswered call — duration 0, no answer — bills
// nothing. It used to be timed off the INVITE, so a call that rang for
// a while and failed was charged a full first step.
func TestCallEndUnansweredBillsNothing(t *testing.T) {
	s := NewServer()
	if code, _ := s.callStart(callStartReq{CallID: "un-1", From: "1000", To: "8613800138000"}); code != 200 {
		t.Fatalf("start code = %d", code)
	}
	code, body := s.callEnd(context.Background(), callEndReq{CallID: "un-1", DurSecs: 0, DurMs: 0, AnswerTsMs: 0})
	cdr, ok := body.(*callcore.CDR)
	if code != 200 || !ok {
		t.Fatalf("end code = %d body = %v", code, body)
	}
	if cdr.Duration != 0 || cdr.BilledSecs != 0 || cdr.Cost != 0 {
		t.Errorf("unanswered end: duration %d billed %d cost %s, want 0 / 0 / 0",
			cdr.Duration, cdr.BilledSecs, cdr.Cost)
	}
}

// An answered call whose end event carries no usable duration is timed
// since its ANSWER (the edge's timestamp, else the recorded one).
func TestCallEndAnsweredWithoutDurationTimesSinceAnswer(t *testing.T) {
	for _, tc := range []struct {
		name    string
		useEdge bool // carry the answer timestamp in the end event
	}{
		{"edge answer timestamp", true},
		{"locally recorded answer", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := NewServer()
			// Put the fake clock 100s in the past so the wall-clock
			// fallback has a real interval to measure.
			answeredAt := time.Now().Add(-70 * time.Second)
			cur := answeredAt.Add(-30 * time.Second)
			s.calls.SetClock(func() time.Time { return cur })
			if code, _ := s.callStart(callStartReq{CallID: "an-1", From: "1000", To: "8613800138000"}); code != 200 {
				t.Fatalf("start code = %d", code)
			}
			cur = answeredAt
			if code, _ := s.callAnswer("an-1"); code != 200 {
				t.Fatalf("answer code = %d", code)
			}
			var ts int64
			if tc.useEdge {
				ts = answeredAt.UnixMilli()
			}
			_, body := s.callEnd(context.Background(), callEndReq{CallID: "an-1", AnswerTsMs: ts})
			cdr, ok := body.(*callcore.CDR)
			if !ok {
				t.Fatalf("end body = %v", body)
			}
			// ~70s of talk at 0.05/min, 60+6: bills 72s = 0.05 + 12/60*0.05.
			if cdr.Duration < 69 || cdr.Duration > 72 || cdr.Cost != money.MustFromYuan(0.06) {
				t.Errorf("duration %d cost %s, want ~70s / 0.0600", cdr.Duration, cdr.Cost)
			}
		})
	}
}

// The answer event starts the billing clock.
func TestCallAnswerMarksTheCallAnswered(t *testing.T) {
	s := NewServer()
	s.callStart(callStartReq{CallID: "aw-1", From: "1000", To: "8613800138000"})
	if _, ok := s.calls.AnswerTime("aw-1"); ok {
		t.Fatal("answered before the answer event")
	}
	if code, _ := s.callAnswer("aw-1"); code != 200 {
		t.Fatalf("answer code = %d", code)
	}
	if _, ok := s.calls.AnswerTime("aw-1"); !ok {
		t.Fatal("answer event did not mark the call answered")
	}
}

// A second dialog under a live Call-ID is refused with 409; the same
// dialog again is idempotent. The tag rides the same JSON both edge
// transports send.
func TestCallStartRefusesSecondDialogUnderLiveCallID(t *testing.T) {
	s := NewServer()
	mux := MuxFor(s)
	start := func(tag string) (int, map[string]any) {
		return postCall(t, mux, "/v1/call/start",
			`{"call_id":"dup","from_user":"1000","to_user":"8613800138000","from_tag":"`+tag+`"}`)
	}
	if code, _ := start("tag-a"); code != 200 {
		t.Fatalf("first start: code = %d", code)
	}
	if code, _ := start("tag-a"); code != 200 {
		t.Errorf("retransmission of the same dialog: code = %d, want 200", code)
	}
	code, body := start("tag-b")
	if code != 409 {
		t.Fatalf("second dialog under a live Call-ID: code = %d, want 409", code)
	}
	if env, _ := body["error"].(map[string]any); env["code"] != "CALL_ID_INUSE" {
		t.Errorf("error envelope = %v, want code CALL_ID_INUSE", body["error"])
	}
	if s.calls.Live() != 1 {
		t.Errorf("live = %d, want 1", s.calls.Live())
	}
	if bal, _ := account.Balance(s.store, "1000"); bal != money.MustFromYuan(10-0.05) {
		t.Errorf("balance = %s, want 9.9500 (one lock only)", bal)
	}
}

// A balance-authority outage is a 503, never a 403 "insufficient
// credit": the caller is not broke, and the edge must not tell it so.
func TestCallStartAuthorityFaultIs503(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"authority down", account.ErrUnavailable},
		{"authority holds no balance", account.ErrNotSeeded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := NewServer()
			s.calls.SetBalanceAuthority(outageAuthority{err: tc.err})
			code, body := s.callStart(callStartReq{CallID: "o-1", From: "1000", To: "8613800138000"})
			if code != 503 {
				t.Fatalf("code = %d, want 503 (body %v)", code, body)
			}
			if env, _ := body.(map[string]errorEnvelope); env["error"].Code != "AUTHORITY_UNAVAILABLE" {
				t.Errorf("envelope = %v, want AUTHORITY_UNAVAILABLE", body)
			}
		})
	}
}

// Same for the admission ledger (the shared-ledger path): an
// unreachable ledger refuses the call with 503, not a limit rejection.
func TestCallStartAdmissionFaultIs503(t *testing.T) {
	mr := miniredis.RunT(t)
	c, err := rds.New("redis://" + mr.Addr())
	if err != nil {
		t.Fatal(err)
	}
	mr.Close() // the ledger goes away under a live client (a closed client would only panic)
	s := NewServer()
	s.calls.SetAdmissionLedger(callcore.NewAdmissionR(c))
	code, body := s.callStart(callStartReq{CallID: "o-2", From: "1000", To: "8613800138000"})
	if code != 503 {
		t.Fatalf("code = %d, want 503 (body %v)", code, body)
	}
	if s.calls.Live() != 0 {
		t.Errorf("live = %d, want 0", s.calls.Live())
	}
}

// A genuinely broke caller still gets the 403.
func TestCallStartBrokeCallerIs403(t *testing.T) {
	s := NewServer()
	s.store.Put(&account.Account{ID: "broke", ParentID: "agent1", RateGroup: "demo"})
	code, body := s.callStart(callStartReq{CallID: "b-1", From: "broke", To: "8613800138000"})
	if code != 403 {
		t.Fatalf("code = %d, want 403", code)
	}
	if env, _ := body.(map[string]errorEnvelope); env["error"].Code != "INSUFFICIENT_CREDIT" {
		t.Errorf("envelope = %v, want INSUFFICIENT_CREDIT", body)
	}
}

// The media stream carries live call audio: it is fail-closed and no
// longer honours the published admin constant. An authenticated
// request gets past the gate and stops at the missing upgrade (400).
func TestMediaStreamAuthIsFailClosed(t *testing.T) {
	s := NewServer()
	mux := MuxFor(s)
	hit := func(h map[string]string) int {
		req := httptest.NewRequest("GET", "/v1/stream/call-1", nil)
		for k, v := range h {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec.Code
	}
	tenantKey, _, tenantHash := acl.GenerateKey(acl.RoleTenant)
	s.acl.Add(acl.Key{ID: "t", TenantID: "tenant", Role: acl.RoleTenant, Hash: tenantHash})
	adminKey, _, adminHash := acl.GenerateKey(acl.RoleAdmin)
	s.acl.Add(acl.Key{ID: "a", Role: acl.RoleAdmin, Hash: adminHash})

	t.Setenv("GVOS_INTERNAL_TOKEN", "")
	for _, tc := range []struct {
		name string
		hdr  map[string]string
	}{
		{"no credentials", nil},
		{"legacy admin constant", map[string]string{"Authorization": "Bearer " + adminToken}},
		{"wrong internal token", map[string]string{"X-GVOS-Internal": "nope"}},
	} {
		if code := hit(tc.hdr); code != 401 {
			t.Errorf("%s: code = %d, want 401", tc.name, code)
		}
	}
	// Tenant keys are now scope-authorized instead of locked out: an
	// unknown call answers 403 (denied without leaking existence).
	if code := hit(map[string]string{"Authorization": "Bearer " + tenantKey}); code != 403 {
		t.Errorf("tenant API key: code = %d, want 403 (scope denial on unknown call)", code)
	}
	if code := hit(map[string]string{"Authorization": "Bearer " + adminKey}); code != 400 {
		t.Errorf("admin API key: code = %d, want 400 (past the gate, no upgrade)", code)
	}
	t.Setenv("GVOS_INTERNAL_TOKEN", "internal-token")
	if code := hit(map[string]string{"X-GVOS-Internal": "internal-token"}); code != 400 {
		t.Errorf("internal token: code = %d, want 400 (past the gate, no upgrade)", code)
	}
}

// The remote-settle guard: ending a call twice settles once.
func TestCallEndTwiceSettlesOnce(t *testing.T) {
	s := NewServer()
	s.callStart(callStartReq{CallID: "td-1", From: "1000", To: "8613800138000"})
	if code, _ := s.callEnd(context.Background(), callEndReq{CallID: "td-1", DurSecs: 60}); code != 200 {
		t.Fatal("first end failed")
	}
	bal, _ := account.Balance(s.store, "1000")
	if code, _ := s.callEnd(context.Background(), callEndReq{CallID: "td-1", DurSecs: 60}); code != 404 {
		t.Fatalf("second end: code = %d, want 404", code)
	}
	if after, _ := account.Balance(s.store, "1000"); after != bal {
		t.Errorf("balance moved on a duplicate end: %s -> %s", bal, after)
	}
}
