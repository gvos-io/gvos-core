package mediastream

import (
	"bytes"
	"testing"
	"time"
)

func TestFanoutBothDirections(t *testing.T) {
	m := New()
	s := m.Open("call-1")

	var upGot, downGot bytes.Buffer
	unsubUp := s.Subscribe(Uplink, SubscriberFunc(func(c Chunk) { upGot.Write(c.Data) }))
	s.Subscribe(Downlink, SubscriberFunc(func(c Chunk) { downGot.Write(c.Data) }))

	// Telephony pushes caller audio (uplink) and network audio
	// (downlink).
	s.Push(Uplink, []byte("caller-audio"))
	s.Push(Downlink, []byte("network-audio"))

	if upGot.String() != "caller-audio" {
		t.Errorf("uplink subscriber got %q", upGot.String())
	}
	if downGot.String() != "network-audio" {
		t.Errorf("downlink subscriber got %q", downGot.String())
	}

	// Unsubscribe stops delivery.
	unsubUp()
	unsubUp() // idempotent
	s.Push(Uplink, []byte("more"))
	if upGot.String() != "caller-audio" {
		t.Errorf("unsubscribed uplink still receiving: %q", upGot.String())
	}
}

func TestInjectSemantics(t *testing.T) {
	m := New()
	s := m.Open("call-2")

	var heard bytes.Buffer
	s.Subscribe(Uplink, SubscriberFunc(func(c Chunk) { heard.Write(c.Data) }))

	// A TTS pipeline injects speech: it lands on the uplink (toward
	// the caller) and is visible to other uplink consumers.
	s.Inject([]byte("tts-audio"))

	if heard.String() != "tts-audio" {
		t.Errorf("injected audio not on uplink: %q", heard.String())
	}
}

func TestEchoBudgetTracking(t *testing.T) {
	m := New()
	s := m.Open("call-3")

	s.Push(Downlink, []byte("net"))
	rtt, ok := s.Echo()
	if !ok || rtt > 500*time.Millisecond {
		t.Errorf("fresh echo: rtt=%v ok=%v", rtt, ok)
	}

	st := s.Stats()
	if st.LastRTTMs < 0 || st.BudgetMaxMs != 500 {
		t.Errorf("stats: %+v", st)
	}

	// An RTT over budget flags out-of-budget.
	s.lastDownTS = time.Now().Add(-600 * time.Millisecond)
	if _, ok := s.Echo(); ok {
		t.Error("600ms echo must exceed the 500ms budget")
	}
	if got := s.Stats().MaxRTTMs; got < 600 {
		t.Errorf("max RTT not tracked: %d", got)
	}
}

func TestCloseFinalizes(t *testing.T) {
	m := New()
	s := m.Open("call-4")
	s.Push(Uplink, []byte("x"))
	st, ok := m.Close("call-4")
	if !ok || st.UplinkChunks != 1 {
		t.Fatalf("close: ok=%v stats=%+v", ok, st)
	}
	if m.Get("call-4") != nil {
		t.Error("closed stream still reachable")
	}
	if _, ok := m.Close("call-4"); ok {
		t.Error("double close must be a no-op")
	}

	// Subscriptions on closed streams no-op and never deliver.
	var got bytes.Buffer
	s.Subscribe(Uplink, SubscriberFunc(func(c Chunk) { got.Write(c.Data) }))
	s.Push(Uplink, []byte("y"))
	if got.Len() != 0 {
		t.Errorf("closed stream delivered: %q", got.String())
	}
}

func TestOpenIdempotent(t *testing.T) {
	m := New()
	a := m.Open("call-5")
	b := m.Open("call-5")
	if a != b {
		t.Error("Open must return the same stream for one call")
	}
	if m.Count() != 1 {
		t.Errorf("count=%d, want 1", m.Count())
	}
}

func TestSequenceMonotonic(t *testing.T) {
	m := New()
	s := m.Open("call-6")

	c1 := s.Push(Uplink, []byte("a"))
	c2 := s.Push(Uplink, []byte("b"))
	c3 := s.Push(Downlink, []byte("c"))
	if c1.Seq != 1 || c2.Seq != 2 {
		t.Errorf("uplink seqs: %d %d", c1.Seq, c2.Seq)
	}
	if c3.Seq != 1 {
		t.Errorf("downlink seq must be independent: %d", c3.Seq)
	}
}

func BenchmarkPushFanout(b *testing.B) {
	m := New()
	s := m.Open("bench")
	var sink int
	s.Subscribe(Uplink, SubscriberFunc(func(c Chunk) { sink += len(c.Data) }))
	data := make([]byte, 160) // 20ms PCMU frame
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s.Push(Uplink, data)
	}
}
