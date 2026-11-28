// Circuit breaker tests (CB-1/2/3): the state machine (consecutive
// failures trip, 404/486 not counted, success resets, exponential
// cooldown, half-open probe), the route-face e2e — a tripped gateway
// leaves the cfg.reload broadcast and unlock restores it — and the
// gvos.gw.breaker observation event.
package api

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/gvos-io/gvos-core/internal/customerdomain/gateway"
	"github.com/gvos-io/gvos-core/internal/shared/events"
)

func TestBreakerStateMachine(t *testing.T) {
	var transitions []string
	b := gateway.NewBreaker(gateway.BreakerConfig{
		ConsecutiveFailures: 3, WindowSeconds: 60, WindowMinCalls: 1000,
		FailureRate: 0.5, CooldownS: 300, CooldownMaxS: 900,
	}, func(gw string, from, to gateway.BreakerState) {
		transitions = append(transitions, gw+":"+string(to))
	})

	// 404/486 never count (callee faults are not gateway faults).
	for i := 0; i < 10; i++ {
		b.Observe("gw-a", 404, false, "")
	}
	if v := b.View("gw-a"); v.State != gateway.BreakerHealthy || v.Consecutive != 0 {
		t.Fatalf("404 tripped the breaker: %+v", v)
	}

	// Three consecutive 5xx: trip on the third.
	for i := 0; i < 2; i++ {
		if _, _, changed := b.Observe("gw-a", 503, false, ""); changed {
			t.Fatal("tripped before the configured streak")
		}
	}
	if _, _, changed := b.Observe("gw-a", 503, false, ""); !changed {
		t.Fatal("third consecutive failure did not trip")
	}
	view := b.View("gw-a")
	if view.State != gateway.BreakerTripped || view.CooldownBase != 300 {
		t.Fatalf("view after trip = %+v", view)
	}
	if view.TripsUntil == "" {
		t.Fatal("trips_until missing")
	}

	// One success during cooldown (probe through the all-tripped
	// fallback) recovers; the streak resets.
	if _, _, changed := b.Observe("gw-a", 200, true, "normal"); !changed {
		t.Fatal("probe success did not recover")
	}
	if v := b.View("gw-a"); v.State != gateway.BreakerHealthy || v.Consecutive != 0 {
		t.Fatalf("post-recovery view = %+v", v)
	}

	// A connected call that releases normally is a success even with a
	// 4xx final code on the by-path.
	if _, _, changed := b.Observe("gw-a", 403, true, "normal"); changed {
		t.Fatal("normal release counted as failure")
	}

	// Answered-2xx is a success; unanswered 2xx is not.
	if _, _, changed := b.Observe("gw-a", 200, false, ""); changed {
		t.Fatal("unanswered 2xx counted as success")
	}
	// Transitions were broadcast exactly on the phase changes.
	if len(transitions) != 2 || transitions[0] != "gw-a:tripped" || transitions[1] != "gw-a:healthy" {
		t.Fatalf("transitions = %v", transitions)
	}
}

func TestBreakerExponentialCooldown(t *testing.T) {
	b := gateway.NewBreaker(gateway.BreakerConfig{
		ConsecutiveFailures: 1, WindowSeconds: 1, WindowMinCalls: 1000,
		FailureRate: 1, CooldownS: 60, CooldownMaxS: 240,
	}, nil)
	// Trip 1 → 60s, trip 2 → 120s, trip 3 → 240s (cap), trip 4 → 240s.
	want := []int{60, 120, 240, 240}
	for i, w := range want {
		b.Observe("gw-x", 500, false, "")
		v := b.View("gw-x")
		if v.TripsUntil == "" {
			t.Fatalf("trip %d: no deadline", i+1)
		}
		until, err := time.Parse(time.RFC3339, v.TripsUntil)
		if err != nil {
			t.Fatal(err)
		}
		got := int(time.Until(until).Seconds())
		if got < w-5 || got > w+5 {
			t.Fatalf("trip %d cooldown = %ds, want ~%ds", i+1, got, w)
		}
		// Recover, then fail again for the next rung.
		b.Observe("gw-x", 200, true, "normal")
	}
}

func TestGatewayBreakerRouteFace(t *testing.T) {
	s := NewServer()
	cp := &capturePoster{}
	s.events = events.NewWithPoster(cp)
	h := MuxFor(s)

	// Two gateways; gw-dead keeps failing (the breaker seam is the
	// settlement hook — five failures trip the default streak).
	s.gws.Reload([]gateway.Gateway{
		{ID: "gw-live", Name: "live", SipAddr: "live:5060", Protocol: "udp", Priority: 5, Weight: 100, Enabled: true},
		{ID: "gw-dead", Name: "dead", SipAddr: "dead:5060", Protocol: "udp", Priority: 1, Weight: 100, Enabled: true},
	})
	for i := 0; i < 5; i++ {
		s.gwBreaker.Observe("gw-dead", 503, false, "")
	}

	// The latest cfg.reload broadcast excludes the tripped gateway.
	flush(s)
	subjects, bodies := cp.snapshot()
	lastGw := []string{}
	for i := len(subjects) - 1; i >= 0; i-- {
		if subjects[i] == events.SubjectCfgReload {
			if gws, ok := bodies[i]["gateways"].([]any); ok {
				for _, g := range gws {
					if gm, ok := g.(map[string]any); ok {
						if id, ok := gm["id"].(string); ok {
							lastGw = append(lastGw, id)
						}
					}
				}
			}
			break
		}
	}
	for _, id := range lastGw {
		if id == "gw-dead" {
			t.Fatal("tripped gateway still in the route broadcast")
		}
	}
	found := false
	for _, id := range lastGw {
		if id == "gw-live" {
			found = true
		}
	}
	if !found {
		t.Fatalf("route broadcast lost the healthy gateway: %v", lastGw)
	}

	// Observation face + unlock lifecycle.
	h = MuxFor(s)
	rec := doAdmin(t, h, "GET", "/v1/admin/gateways/gw-dead/breaker", nil)
	var view struct {
		Breaker struct {
			State       string `json:"state"`
			Consecutive int    `json:"consecutive_failures"`
		} `json:"breaker"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &view)
	if view.Breaker.State != "tripped" || view.Breaker.Consecutive != 5 {
		t.Fatalf("breaker view = %+v", view.Breaker)
	}
	if rec := doAdmin(t, h, "POST", "/v1/admin/gateways/gw-dead/unlock", nil); rec.Code != 200 {
		t.Fatalf("unlock = %d body=%s", rec.Code, rec.Body.String())
	}
	if v := s.gwBreaker.View("gw-dead"); v.State != gateway.BreakerHealthy {
		t.Fatalf("post-unlock = %+v", v)
	}
	// Unlock of a healthy gateway: 404 (nothing to unlock).
	if rec := doAdmin(t, h, "POST", "/v1/admin/gateways/gw-dead/unlock", nil); rec.Code != 404 {
		t.Fatalf("unlock healthy = %d, want 404", rec.Code)
	}
	// Unknown gateway breaker view: 404.
	if rec := doAdmin(t, h, "GET", "/v1/admin/gateways/nope/breaker", nil); rec.Code != 404 {
		t.Fatalf("unknown breaker view = %d, want 404", rec.Code)
	}

	// The gvos.gw.breaker observation event rode the transition.
	sawBreakerEvent := false
	for i, subj := range subjects {
		if subj == events.SubjectGatewayBreaker {
			if to, _ := bodies[i]["to"].(string); to == "tripped" {
				sawBreakerEvent = true
			}
		}
	}
	if !sawBreakerEvent {
		t.Fatal("no gvos.gw.breaker observation event on trip")
	}
}
