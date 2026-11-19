// Event-bus integration: with a bus attached, the call lifecycle and
// admin mutations publish events — including in memory mode (demo
// deployments are a valid publisher target).
package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/lixuanqun/gvos-core/internal/billingdomain/account"
	"github.com/lixuanqun/gvos-core/internal/shared/events"
)

// capturePoster records every published event (subject + decoded
// JSON) for assertions after Publisher.Close flushes the queue.
type capturePoster struct {
	mu     sync.Mutex
	names  []string
	bodies []map[string]any
}

func (c *capturePoster) Post(subject string, data []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.names = append(c.names, subject)
	var m map[string]any
	_ = json.Unmarshal(data, &m)
	c.bodies = append(c.bodies, m)
	return nil
}

func (c *capturePoster) Close() error { return nil }

func (c *capturePoster) snapshot() (subjects []string, bodies []map[string]any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.names...), append([]map[string]any(nil), c.bodies...)
}

func flush(s *Server) { // flush the queue so pump-side assertions are safe
	if s.events != nil {
		s.events.Close()
	}
}

// TestEventBusCallAndAdminEvents: one call publishes cdr.completed;
// an admin account creation publishes admin.changed — both in memory
// mode (no PG audit path involved).
func TestEventBusCallAndAdminEvents(t *testing.T) {
	s := NewServer()
	cap := &capturePoster{}
	s.events = events.NewWithPoster(cap)

	base := time.Unix(1_700_000_000, 0)
	cur := base
	s.calls.SetClock(func() time.Time { return cur })

	srv := httptest.NewServer(MuxFor(s))
	defer srv.Close()

	post := func(path string, body any, auth bool) int {
		t.Helper()
		b, _ := json.Marshal(body)
		req, _ := http.NewRequest("POST", srv.URL+path, bytes.NewReader(b))
		if auth {
			req.Header.Set("Authorization", "Bearer "+adminToken)
		}
		req.Header.Set("X-GVOS-Internal", os.Getenv("GVOS_INTERNAL_TOKEN"))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("POST %s: %v", path, err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}

	if code := post("/v1/call/start", map[string]string{
		"call_id": "ev-1", "from_user": "1000", "to_user": "8613800138000",
	}, false); code != 200 {
		t.Fatalf("start: %d", code)
	}
	cur = base.Add(66 * time.Second)
	if code := post("/v1/call/end", map[string]any{"call_id": "ev-1", "duration": 66}, false); code != 200 {
		t.Fatalf("end: %d", code)
	}
	if code := post("/v1/admin/accounts", map[string]any{"username": "evuser", "credit": 5}, true); code != 201 {
		t.Fatalf("account create: %d", code)
	}
	flush(s)

	subjects, bodies := cap.snapshot()
	seen := map[string]map[string]any{}
	for i, name := range subjects {
		seen[name] = bodies[i]
	}
	cdr, ok := seen[events.SubjectCDRCompleted]
	if !ok {
		t.Fatalf("no %s published; got %v", events.SubjectCDRCompleted, subjects)
	}
	if cdr["CallID"] != "ev-1" || cdr["BilledSecs"] != float64(66) || cdr["Cost"] != 0.055 {
		t.Errorf("cdr event = %v", cdr)
	}
	admin, ok := seen[events.SubjectAdminChanged]
	if !ok {
		t.Fatalf("no %s published; got %v", events.SubjectAdminChanged, subjects)
	}
	if admin["action"] != "create" || admin["entity"] != "account" {
		t.Errorf("admin event = %v", admin)
	}
}

// TestEventBusTeardown: draining the balance mid-call fires the
// teardown callback, which publishes gvos.call.teardown; the
// subsequent BYE still settles and publishes the CDR.
func TestEventBusTeardown(t *testing.T) {
	s := NewServer()
	cap := &capturePoster{}
	s.events = events.NewWithPoster(cap)

	base := time.Unix(1_700_000_000, 0)
	cur := base
	s.calls.SetClock(func() time.Time { return cur })

	srv := httptest.NewServer(MuxFor(s))
	defer srv.Close()

	// Caller with barely-affordable balance: lock (0.05) succeeds,
	// interim at 60s (0.05) fails after we drain the account.
	s.store.Put(&account.Account{ID: "1001", ParentID: "agent1", Credit: 10.06, RateGroup: "demo"})

	b, _ := json.Marshal(map[string]string{
		"call_id": "ev-td", "from_user": "1001", "to_user": "8613800138000",
	})
	req, _ := http.NewRequest("POST", srv.URL+"/v1/call/start", bytes.NewReader(b))
	req.Header.Set("X-GVOS-Internal", os.Getenv("GVOS_INTERNAL_TOKEN"))
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("start: %v code=%d", err, resp.StatusCode)
	}
	resp.Body.Close()

	cur = base.Add(60 * time.Second)
	s.store.Reserve("1001", 10) // leave 0.01 < the 0.05 interim charge
	if alive, err := s.calls.Interim("ev-td"); err != nil || alive {
		t.Fatalf("interim: alive=%v err=%v (teardown expected)", alive, err)
	}

	// BYE arrives: the torn-down call still settles into a CDR.
	cur = base.Add(66 * time.Second)
	req2, _ := http.NewRequest("POST", srv.URL+"/v1/call/end",
		bytes.NewReader([]byte(`{"call_id":"ev-td","duration":66}`)))
	req2.Header.Set("X-GVOS-Internal", os.Getenv("GVOS_INTERNAL_TOKEN"))
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil || resp2.StatusCode != 200 {
		t.Fatalf("end: %v code=%d", err, resp2.StatusCode)
	}
	resp2.Body.Close()
	flush(s)

	subjects, bodies := cap.snapshot()
	var td, cdr map[string]any
	for i, name := range subjects {
		if name == events.SubjectCallTeardown {
			td = bodies[i]
		} else if name == events.SubjectCDRCompleted {
			cdr = bodies[i]
		}
	}
	if td == nil {
		t.Fatalf("no %s published; got %v", events.SubjectCallTeardown, subjects)
	}
	if td["call_id"] != "ev-td" || td["reason"] != "balance-exhausted" {
		t.Errorf("teardown event = %v", td)
	}
	if cdr == nil {
		t.Fatalf("no %s published after teardown BYE", events.SubjectCDRCompleted)
	}
	if cdr["Release"] != "balance-exhausted" {
		t.Errorf("cdr release = %v, want balance-exhausted", cdr["Release"])
	}
}

// TestEventBusCfgReloadBroadcast: rate upserts and blocklist changes
// broadcast gvos.cfg.reload so peer replicas rebuild their snapshots
// from the authoritative store (the instant-propagation path).
func TestEventBusCfgReloadBroadcast(t *testing.T) {
	s := NewServer()
	cap := &capturePoster{}
	s.events = events.NewWithPoster(cap)

	srv := httptest.NewServer(MuxFor(s))
	defer srv.Close()

	admin := func(path string, body any) int {
		t.Helper()
		b, _ := json.Marshal(body)
		req, _ := http.NewRequest("POST", srv.URL+path, bytes.NewReader(b))
		req.Header.Set("Authorization", "Bearer "+adminToken)
		req.Header.Set("X-GVOS-Internal", os.Getenv("GVOS_INTERNAL_TOKEN"))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("POST %s: %v", path, err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := admin("/v1/admin/rates", map[string]any{"prefix": "44", "price": 0.2}); code != 200 {
		t.Fatalf("rate upsert: %d", code)
	}
	if code := admin("/v1/admin/blocklist", map[string]any{"kind": "black", "scope": "dest", "pattern": "44"}); code != 201 {
		t.Fatalf("blocklist add: %d", code)
	}
	flush(s)

	rateReloads := 0
	subjects, bodies := cap.snapshot()
	for i, name := range subjects {
		if name != events.SubjectCfgReload {
			continue
		}
		switch bodies[i]["entity"] {
		case "rate":
			rateReloads++
		case "blocklist":
			// Rule changes carry the operation payload, not a reload
			// notice: peers apply them directly.
			if bodies[i]["op"] != "add" {
				t.Errorf("blocklist op = %v, want add", bodies[i]["op"])
			}
			entry, ok := bodies[i]["entry"].(map[string]any)
			if !ok || entry["pattern"] != "44" {
				t.Errorf("blocklist entry = %v", bodies[i]["entry"])
			}
		}
	}
	if rateReloads != 1 {
		t.Errorf("rate reload broadcasts = %d, want 1", rateReloads)
	}
}

// TestStatsNatsOff: /v1/stats reports the bus state so deployments
// can monitor it alongside mode.
func TestStatsNatsOff(t *testing.T) {
	s := NewServer()
	srv := httptest.NewServer(MuxFor(s))
	defer srv.Close()
	code, stats := get(t, srv.URL+"/v1/stats")
	if code != 200 || stats["nats"] != "off" {
		t.Errorf("stats = %v, want nats=off", stats)
	}
}
