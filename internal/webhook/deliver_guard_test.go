package webhook

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// Tests for the two webhook guards added after the 2026-11 security
// review: tenant scoping of fan-out, and the dial-time SSRF check.

// The deliverer refuses loopback at dial time, whatever the
// subscription-time check said.
func TestDialTimeGuardRefusesLoopback(t *testing.T) {
	if egressProxyConfigured() {
		t.Skip("an egress proxy is configured: the dial-time check does not apply")
	}
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(200)
	}))
	defer srv.Close()

	d := NewDeliverer()
	res := d.deliverOnce(context.Background(),
		Subscription{ID: "s1", URL: srv.URL, Secret: "sec", Active: true}, "gvos.cdr.completed", []byte(`{}`))
	if res.OK {
		t.Fatal("delivery to a loopback address succeeded")
	}
	if calls != 0 {
		t.Errorf("endpoint was reached %d times, want 0", calls)
	}
	if !strings.Contains(res.Err, "dial-time check") {
		t.Errorf("error = %q, want the dial-time refusal", res.Err)
	}
}

// DNS rebinding: a name that resolves to a private address is refused
// when the dial happens, even though nothing here is a literal IP.
func TestDialTimeGuardRefusesPrivateDestinations(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := strconv.Itoa(l.Addr().(*net.TCPAddr).Port)
	l.Close()

	for _, host := range []string{"localhost", "127.0.0.1", "::1", "10.0.0.1", "100.100.100.200", "169.254.169.254"} {
		addr := net.JoinHostPort(host, port)
		conn, err := safeDialContext(context.Background(), "tcp", addr)
		if conn != nil {
			conn.Close()
		}
		if err == nil {
			t.Errorf("dial %s succeeded, want refused", addr)
		}
	}
}

// The subscription-time check covers the address forms that used to
// slip past net.ParseIP (zoned IPv6 literals) and the carrier-grade
// NAT range.
func TestSubscriptionGuardCoversZonedAndCGNAT(t *testing.T) {
	for _, raw := range []string{
		"https://[fe80::1%25lo]/hook",
		"https://100.100.100.200/latest/meta-data",
		"https://[::ffff:127.0.0.1]/hook",
		"https://0.0.0.0/hook",
	} {
		if err := ValidateSubscription(raw); err == nil {
			t.Errorf("ValidateSubscription(%q) accepted a private target", raw)
		}
	}
	if err := ValidateSubscription("https://8.8.8.8/hook"); err != nil {
		t.Errorf("ValidateSubscription(public literal) = %v, want nil", err)
	}
}

// recordTransport answers every request and records its path and body.
type recordTransport struct {
	mu     sync.Mutex
	bodies map[string][]string // path -> request bodies
}

func (rt *recordTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	b, _ := io.ReadAll(req.Body)
	rt.mu.Lock()
	if rt.bodies == nil {
		rt.bodies = map[string][]string{}
	}
	rt.bodies[req.URL.Path] = append(rt.bodies[req.URL.Path], string(b))
	rt.mu.Unlock()
	return &http.Response{
		StatusCode: 200, Status: "200 OK", Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1,
		Header: http.Header{}, Body: io.NopCloser(strings.NewReader("")), Request: req,
	}, nil
}

func (rt *recordTransport) count(path string) int {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return len(rt.bodies[path])
}

func (rt *recordTransport) first(path string) string {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if len(rt.bodies[path]) == 0 {
		return ""
	}
	return rt.bodies[path][0]
}

func (rt *recordTransport) String() string {
	return fmt.Sprintf("tenant-a=%d tenant-b=%d platform=%d", rt.count("/ta"), rt.count("/tb"), rt.count("/platform"))
}

// waitUntil polls cond for a while (deliveries run on their own goroutines).
func waitUntil(cond func() bool) bool {
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return cond()
}

// A tenant-owned subscription receives only the events inside its own
// scope; a platform (operator-created) subscription receives all. The
// loop used to match on the event name alone, so any tenant that
// registered cdr.completed was mailed every call on the platform.
func TestFanOutIsScopedToTheOwningTenant(t *testing.T) {
	rt := &recordTransport{}
	dsp := NewDispatcher(context.Background(), &Deliverer{Client: &http.Client{Transport: rt}})
	dsp.SetSubscriptions([]Subscription{
		{ID: "a", URL: "https://a.example/ta", Secret: "s", Active: true, Tenant: "tenant-a", Events: []string{"gvos.cdr.completed", "gvos.call.teardown"}},
		{ID: "b", URL: "https://b.example/tb", Secret: "s", Active: true, Tenant: "tenant-b", Events: []string{"gvos.cdr.completed", "gvos.call.teardown"}},
		{ID: "p", URL: "https://p.example/platform", Secret: "s", Active: true, Events: []string{"gvos.cdr.completed", "gvos.call.teardown"}},
	})

	// A call by account acc-1, whose chain runs up to tenant-a.
	dsp.PublishEnvelopeScoped("cdr.completed", "demo", []string{"acc-1", "agent-x", "tenant-a"}, map[string]string{"call_id": "c-1"})
	dsp.PublishScoped("gvos.call.teardown", []string{"acc-2", "tenant-b"}, map[string]string{"call_id": "c-2", "reason": "balance-exhausted"})
	// An event whose owner cannot be named reaches the platform alone.
	dsp.PublishEnvelopeScoped("cdr.completed", "demo", []string{}, map[string]string{"call_id": "c-3"})

	if !waitUntil(func() bool { return rt.count("/platform") == 3 }) {
		t.Fatalf("deliveries: %s, want 1 / 1 / 3", rt)
	}
	time.Sleep(50 * time.Millisecond) // let a wrongful extra delivery land
	if rt.count("/ta") != 1 || rt.count("/tb") != 1 || rt.count("/platform") != 3 {
		t.Fatalf("deliveries: %s, want 1 / 1 / 3", rt)
	}
	// Each tenant's envelope names its own tenant, not a platform-wide one.
	for path, want := range map[string]string{"/ta": "tenant-a", "/tb": "tenant-b"} {
		var env struct {
			Tenant string `json:"tenant"`
			Data   struct {
				CallID string `json:"call_id"`
			} `json:"data"`
		}
		if err := json.Unmarshal([]byte(rt.first(path)), &env); err != nil {
			t.Fatalf("%s: %v (body %s)", path, err, rt.first(path))
		}
		if env.Tenant != want {
			t.Errorf("%s envelope tenant = %q, want %q", path, env.Tenant, want)
		}
	}
	if strings.Contains(rt.first("/ta"), `"c-2"`) || strings.Contains(rt.first("/tb"), `"c-1"`) {
		t.Error("a tenant received another tenant's event")
	}
}

// Unscoped publishes keep their meaning: everyone subscribed sees them.
func TestUnscopedPublishReachesEverySubscriber(t *testing.T) {
	rt := &recordTransport{}
	dsp := NewDispatcher(context.Background(), &Deliverer{Client: &http.Client{Transport: rt}})
	dsp.SetSubscriptions([]Subscription{
		{ID: "a", URL: "https://a.example/ta", Secret: "s", Active: true, Tenant: "tenant-a", Events: []string{"gvos.ops.note"}},
		{ID: "b", URL: "https://b.example/tb", Secret: "s", Active: true, Tenant: "tenant-b", Events: []string{"gvos.ops.note"}},
	})
	dsp.Publish("gvos.ops.note", map[string]string{"k": "v"})
	if !waitUntil(func() bool { return rt.count("/ta") == 1 && rt.count("/tb") == 1 }) {
		t.Fatalf("deliveries: %s, want 1 / 1", rt)
	}
}
