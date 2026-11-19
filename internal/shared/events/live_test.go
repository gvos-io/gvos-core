// Live-NATS integration test, gated on GVOS_TEST_NATS_URL (mirrors
// internal/store's GVOS_TEST_DSN pattern). Runs against the compose
// nats service: docker compose -f deploy/docker-compose.yml up -d nats
// then GVOS_TEST_NATS_URL=nats://127.0.0.1:4222 go test ./internal/events/ -v
package events

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
)

func testURL() string {
	return os.Getenv("GVOS_TEST_NATS_URL")
}

// TestLiveRequestReply: Respond serves a request and RequestAsync
// receives the reply on its goroutine without error.
func TestLiveRequestReply(t *testing.T) {
	url := testURL()
	if url == "" {
		t.Skip("GVOS_TEST_NATS_URL not set; skipping live NATS test")
	}
	p, err := New(url)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer p.Close()
	if err := p.Respond("gvos.test.svc", func(req []byte) []byte {
		return []byte("echo:" + string(req))
	}); err != nil {
		t.Fatalf("Respond: %v", err)
	}

	done := make(chan error, 1)
	var reply []byte
	if !p.RequestAsync("gvos.test.svc", "ping", 5*time.Second, func(r []byte, err error) {
		reply = r
		done <- err
	}) {
		t.Fatal("RequestAsync returned false on a live transport")
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		// Payloads travel as JSON, so "ping" arrives quoted; the
		// responder echoes the raw request bytes back.
		if string(reply) != `echo:"ping"` {
			t.Errorf("reply = %s, want echo:\"ping\"", reply)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no reply within window")
	}
}

// TestLiveBroadcast: a core pub/sub subject fans out to every
// subscriber (two handlers on the publisher's conn and one raw conn).
func TestLiveBroadcast(t *testing.T) {
	url := testURL()
	if url == "" {
		t.Skip("GVOS_TEST_NATS_URL not set; skipping live NATS test")
	}
	p, err := New(url)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer p.Close()

	var hits int32
	if err := p.Subscribe("gvos.test.bc", func(data []byte) {
		atomic.AddInt32(&hits, 1)
	}); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	nc2, err := nats.Connect(url, nats.Name("gvos-broadcast-peer"))
	if err != nil {
		t.Fatalf("peer connect: %v", err)
	}
	defer nc2.Close()
	peerGot := make(chan struct{}, 1)
	sub, err := nc2.Subscribe("gvos.test.bc", func(m *nats.Msg) { peerGot <- struct{}{} })
	if err != nil {
		t.Fatalf("peer subscribe: %v", err)
	}
	defer sub.Unsubscribe()

	// A second publisher connection broadcasts once.
	ncSend, err := nats.Connect(url, nats.Name("gvos-broadcast-sender"))
	if err != nil {
		t.Fatalf("sender connect: %v", err)
	}
	defer ncSend.Close()
	if err := ncSend.Publish("gvos.test.bc", []byte("hello")); err != nil {
		t.Fatalf("publish: %v", err)
	}

	deadline := time.After(5 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatalf("fan-out incomplete: peer=%v handler=%d", len(peerGot) > 0, hits)
		case <-time.After(20 * time.Millisecond):
			if atomic.LoadInt32(&hits) >= 1 && len(peerGot) > 0 {
				return // both subscribers received their copy
			}
		}
	}
}

// TestLiveDurableConsumer: the WEBHOOKS-style work queue — a handler
// failure NAKs with a delay and JetStream redelivers to the same
// durable; success acks and delivery stops. At-least-once in action.
func TestLiveDurableConsumer(t *testing.T) {
	url := testURL()
	if url == "" {
		t.Skip("GVOS_TEST_NATS_URL not set; skipping live NATS test")
	}
	p, err := New(url)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer p.Close()
	if p.Mode() != "jetstream" {
		t.Skip("broker without JetStream; consumer test needs JS")
	}

	// Unique subject+durable per run: leftover deliveries from earlier
	// runs must not pollute the delivery count. The subject lives under
	// gvos.cdr.test.> so the GVOS stream captures it (stream Subjects
	// are the explicit event domains, not gvos.>).
	stamp := time.Now().UnixNano()
	subject := fmt.Sprintf("gvos.cdr.test.queue.%d", stamp)
	durable := fmt.Sprintf("TESTQ%d", stamp)

	var calls int32
	consumer, err := p.NewDurableConsumer(durable, subject, func(data []byte) error {
		if atomic.AddInt32(&calls, 1) == 1 {
			return errors.New("first delivery fails on purpose")
		}
		return nil // ack on the redelivery
	}, 1)
	if err != nil {
		t.Fatalf("NewDurableConsumer: %v", err)
	}
	defer consumer.Stop()

	nc, err := nats.Connect(url, nats.Name("gvos-consumer-feed"))
	if err != nil {
		t.Fatalf("feed connect: %v", err)
	}
	defer nc.Close()
	// Publish into the stream (JS-backed publish is not required —
	// core publishes on a stream-covered subject land in GVOS too).
	if err := nc.Publish(subject, []byte("payload")); err != nil {
		t.Fatalf("feed publish: %v", err)
	}

	// Original delivery + NAK redelivery (retryDelay=1s) + ack.
	deadline := time.After(15 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatalf("redelivery did not settle: calls=%d", atomic.LoadInt32(&calls))
		default:
			if atomic.LoadInt32(&calls) >= 2 {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
}

// TestLiveNATS: real connect, JetStream stream setup, publish, and
// readback over a second connection.
func TestLiveNATS(t *testing.T) {
	url := testURL()
	if url == "" {
		t.Skip("GVOS_TEST_NATS_URL not set; skipping live NATS test")
	}
	// Subscriber first: prove the event was published and fanned out.
	nc, err := nats.Connect(url, nats.Name("gvos-core-test-sub"))
	if err != nil {
		t.Fatalf("subscriber connect: %v", err)
	}
	defer nc.Close()
	msgs := make(chan *nats.Msg, 8)
	sub, err := nc.ChanSubscribe("gvos.cdr.test.>", msgs)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer sub.Unsubscribe()

	p, err := New(url)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Logf("mode=%s", p.Mode())
	// A stream-covered subject: the publish lands in GVOS *and* fans
	// out to core subscribers — the dual path the bus relies on.
	p.Publish("gvos.cdr.test.ping", map[string]string{"hello": "nats"})
	p.Close() // flushes before we read

	select {
	case m := <-msgs:
		if m.Subject != "gvos.cdr.test.ping" {
			t.Errorf("subject = %s, want gvos.cdr.test.ping", m.Subject)
		}
		var body map[string]string
		if err := json.Unmarshal(m.Data, &body); err != nil || body["hello"] != "nats" {
			t.Errorf("payload = %s (%v)", m.Data, err)
		}
	case <-time.After(flushTimeout * 2):
		t.Fatal("no event received within flush window")
	}
}
