package events

import (
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
)

// stubPoster records every post (thread-safe: the pump goroutine is
// the only writer, but tests read after Close, which happens-from).
type stubPoster struct {
	mu    sync.Mutex
	posts []queued
	fail  error
}

func (s *stubPoster) Post(subject string, data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail != nil {
		return s.fail
	}
	s.posts = append(s.posts, queued{subject: subject, data: data})
	return nil
}

func (s *stubPoster) Close() error { return nil }

func (s *stubPoster) recorded() []queued {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]queued(nil), s.posts...)
}

// TestPublishFlush verifies the core contract: every published event
// reaches the transport with its subject and valid JSON, and Close
// flushes everything published before it.
func TestPublishFlush(t *testing.T) {
	p := NewWithPoster(&stubPoster{})
	p.Publish(SubjectCDRCompleted, map[string]any{"CallID": "c1", "Cost": 0.055})
	p.Publish(SubjectCallTeardown, map[string]string{"call_id": "c1", "reason": "balance-exhausted"})
	p.Close()

	stub := p.poster.(*stubPoster)
	posts := stub.recorded()
	if len(posts) != 2 {
		t.Fatalf("recorded %d posts, want 2", len(posts))
	}
	if posts[0].subject != SubjectCDRCompleted {
		t.Errorf("subject[0] = %s, want %s", posts[0].subject, SubjectCDRCompleted)
	}
	var cdr map[string]any
	if err := json.Unmarshal(posts[0].data, &cdr); err != nil {
		t.Fatalf("payload[0] not JSON: %v", err)
	}
	if cdr["CallID"] != "c1" {
		t.Errorf("payload[0] = %v", cdr)
	}
	var td map[string]string
	if err := json.Unmarshal(posts[1].data, &td); err != nil {
		t.Fatalf("payload[1] not JSON: %v", err)
	}
	if td["reason"] != "balance-exhausted" {
		t.Errorf("payload[1] = %v", td)
	}
}

// TestPublishMarshalFailure: unserializable payloads are dropped with
// a log line, never panic or enqueue garbage.
func TestPublishMarshalFailure(t *testing.T) {
	p := NewWithPoster(&stubPoster{})
	p.Publish(SubjectAdminChanged, map[string]any{"bad": make(chan int)})
	p.Close()
	if got := len(p.poster.(*stubPoster).recorded()); got != 0 {
		t.Errorf("recorded %d posts, want 0 (marshal failure must drop)", got)
	}
}

// TestPublishAfterClose: late callers (concurrent request handlers
// racing shutdown) are silent no-ops, not panics.
func TestPublishAfterClose(t *testing.T) {
	p := NewWithPoster(&stubPoster{})
	p.Close()
	p.Publish(SubjectRiskAlert, map[string]string{"account": "a"})
	if got := len(p.poster.(*stubPoster).recorded()); got != 0 {
		t.Errorf("recorded %d posts after Close, want 0", got)
	}
}

// TestQueueDrop: with the transport stalled, the queue absorbs
// queueSize events and drops the rest loudly instead of blocking the
// caller. Deterministic: the first Post blocks in the pump, so the
// channel state is fully under the test's control.
func TestQueueDrop(t *testing.T) {
	stub := newBlockingPoster()
	p := NewWithPoster(stub)

	p.Publish(SubjectCDRCompleted, "in-flight")
	// Deterministic precondition: wait until the pump is actually
	// blocked inside the first Post (fast CI schedulers race ahead
	// otherwise and the queue never fills).
	select {
	case <-stub.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("pump never entered Post")
	}
	for i := 0; i < queueSize; i++ { // fill the channel buffer
		p.Publish(SubjectCDRCompleted, i)
	}
	p.Publish(SubjectCDRCompleted, "dropped") // one beyond capacity
	close(stub.gate)
	p.Close()

	// queueSize buffered + 1 in-flight through the pump = delivered;
	// the queueSize+2nd publish was dropped (see the "queue full" log).
	if got := len(stub.recorded()); got != queueSize+1 {
		t.Errorf("delivered %d events, want %d (exactly one drop)", got, queueSize+1)
	}
}

// blockingPoster blocks the first Post until the gate opens, freezing
// the pump so the test can deterministically overfill the queue. The
// entered channel signals when the pump is truly inside Post.
type blockingPoster struct {
	gate    chan struct{}
	entered chan struct{}
	once    sync.Once
	inner   stubPoster
}

func newBlockingPoster() *blockingPoster {
	return &blockingPoster{gate: make(chan struct{}), entered: make(chan struct{})}
}

func (b *blockingPoster) Post(subject string, data []byte) error {
	b.once.Do(func() { close(b.entered); <-b.gate })
	return b.inner.Post(subject, data)
}

func (b *blockingPoster) Close() error { return nil }

func (b *blockingPoster) recorded() []queued { return b.inner.recorded() }

// TestConcurrentPublishClose exercises the mutex: concurrent request
// handlers publishing while shutdown closes the queue must never
// panic or corrupt the channel (run with -race).
func TestConcurrentPublishClose(t *testing.T) {
	p := NewWithPoster(&stubPoster{})
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				p.Publish(SubjectCDRCompleted, map[string]int{"g": g, "i": i})
			}
		}(g)
	}
	p.Close()
	wg.Wait() // publishes racing Close must not panic
}

// TestModeDefault: the stub transport reports core (JetStream only
// via New against a live server).
func TestModeDefault(t *testing.T) {
	p := NewWithPoster(&stubPoster{})
	if p.Mode() != "core" {
		t.Errorf("mode = %s, want core", p.Mode())
	}
	p.Close()
}

// stubPuller is the consumer seam: Fetch pulls from a channel and
// degrades to the real idle-poll behavior (ErrTimeout on fetch
// timeout). Messages are bare &nats.Msg{} — the worker's ack/nak
// calls hit ErrMsgNotBound, which the worker must tolerate silently.
type stubPuller struct {
	ch chan *nats.Msg
}

func newStubPuller() *stubPuller { return &stubPuller{ch: make(chan *nats.Msg, 64)} }

func (s *stubPuller) feed(data string) { s.ch <- &nats.Msg{Data: []byte(data)} }

func (s *stubPuller) Fetch(batch int, opts ...nats.PullOpt) ([]*nats.Msg, error) {
	select {
	case m := <-s.ch:
		return []*nats.Msg{m}, nil
	case <-time.After(fetchTimeout):
		return nil, nats.ErrTimeout
	}
}

// TestConsumerDispatchAndStop: every pulled message reaches the
// handler exactly once across competing workers, and Stop unblocks
// idle workers within one fetch window.
func TestConsumerDispatchAndStop(t *testing.T) {
	pull := newStubPuller()
	var mu sync.Mutex
	seen := map[string]int{}

	c := &Consumer{name: "test", puller: pull,
		handler: func(data []byte) error {
			mu.Lock()
			seen[string(data)]++
			mu.Unlock()
			return nil // acks hit ErrMsgNotBound on stub msgs: tolerated
		},
		done: make(chan struct{})}
	for i := 0; i < 2; i++ {
		c.wg.Add(1)
		go c.work()
	}
	for _, d := range []string{"a", "b", "c", "d"} {
		pull.feed(d)
	}
	// Wait for all four to be dispatched (workers poll at 1s idle).
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(seen)
		mu.Unlock()
		if n == 4 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	for d, n := range seen {
		if n != 1 {
			t.Errorf("handler saw %q %d times, want 1", d, n)
		}
	}
	if len(seen) != 4 {
		t.Errorf("dispatched %d/4 messages", len(seen))
	}
	c.Stop() // must return: idle workers check done every fetch window
}

// TestConsumerNakPath: handler errors route to NAK (tolerated on stub
// msgs) and the worker keeps pulling — no poison-message stall.
func TestConsumerNakPath(t *testing.T) {
	pull := newStubPuller()
	var calls int32
	c := &Consumer{name: "nak", puller: pull,
		handler: func(data []byte) error {
			atomic.AddInt32(&calls, 1)
			return errors.New("downstream down")
		},
		done: make(chan struct{})}
	for i := 0; i < 1; i++ {
		c.wg.Add(1)
		go c.work()
	}
	pull.feed("x")
	pull.feed("y")
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && atomic.LoadInt32(&calls) < 2 {
		time.Sleep(20 * time.Millisecond)
	}
	if atomic.LoadInt32(&calls) != 2 {
		t.Errorf("handler calls = %d, want 2 (worker kept pulling after NAK)", calls)
	}
	c.Stop()
}
