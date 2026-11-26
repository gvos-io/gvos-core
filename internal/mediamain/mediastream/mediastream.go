// Package mediastream implements the AI media plane: bidirectional
// audio-chunk distribution for live calls. A call's stream (chunks
// arriving from the telephony side) fans out to subscribers (ASR/TTS
// pipelines, live monitoring, recording) and accepts upstream audio
// back into the telephony direction - the semantics Vapi/Retell-style
// voice-agent platforms consume.
//
// Hot-path goals: lock-guarded maps only, zero allocation on the
// chunk path beyond the slice the caller provides, and a sub-500ms
// round-trip budget tracker per stream (PRD: AI media plane).
package mediastream

import (
	"sync"
	"time"
)

// Chunk is one audio frame in flight. Format is negotiated at
// subscription (e.g. 8kHz PCMU 20ms = 160 bytes, the native telco
// framing).
type Chunk struct {
	Data []byte
	Seq  uint64 // monotonic per stream direction
	TS   time.Time
}

// Direction of a chunk relative to the telephony leg.
type Direction int

const (
	// Uplink: audio flowing from the caller toward the network (what
	// an ASR pipeline listens to).
	Uplink Direction = iota
	// Downlink: audio flowing from the network toward the caller (what
	// a TTS pipeline produces).
	Downlink
)

// Subscriber receives chunks for one direction of one call.
type Subscriber interface {
	OnChunk(c Chunk)
}

// SubscriberFunc adapts a function to Subscriber.
type SubscriberFunc func(c Chunk)

func (f SubscriberFunc) OnChunk(c Chunk) { f(c) }

// StreamStats is the per-stream health snapshot (monitoring +
// budget checks).
type StreamStats struct {
	UplinkChunks   uint64
	DownlinkChunks uint64
	UplinkBytes    uint64
	DownlinkBytes  uint64
	// LastRTT: latency of the most recent echo round (subscriber ->
	// upstream injection -> back). Budget: 500ms hard.
	LastRTTMs   int64
	MaxRTTMs    int64
	BudgetMaxMs int64
}

// Manager owns all live streams.
type Manager struct {
	mu      sync.RWMutex
	streams map[string]*Stream
}

func New() *Manager {
	return &Manager{streams: map[string]*Stream{}}
}

// subEntry pairs a subscriber with a unique handle so removal never
// needs interface comparison (function-typed subscribers are not
// comparable in Go).
type subEntry struct {
	id  uint64
	sub Subscriber
}

// Stream is one call's bidirectional media distribution point.
type Stream struct {
	mu sync.Mutex
	id string

	upSubs   []subEntry
	downSubs []subEntry
	nextSub  uint64

	upSeq   uint64
	downSeq uint64
	upBytes uint64
	dnBytes uint64

	// RTT tracking: when a subscriber injects upstream audio echoing a
	// downlink chunk, Echo() measures the loop time.
	lastDownTS time.Time
	lastRTT    time.Duration
	maxRTT     time.Duration
	budget     time.Duration

	closed bool
}

// Open creates (or returns) the stream for a call.
func (m *Manager) Open(callID string) *Stream {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s, ok := m.streams[callID]; ok {
		return s
	}
	s := &Stream{id: callID, budget: 500 * time.Millisecond}
	m.streams[callID] = s
	return s
}

// Get returns the live stream for a call (nil when absent).
func (m *Manager) Get(callID string) *Stream {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.streams[callID]
}

// Close tears down the stream and reports final stats.
func (m *Manager) Close(callID string) (StreamStats, bool) {
	m.mu.Lock()
	s, ok := m.streams[callID]
	if ok {
		delete(m.streams, callID)
	}
	m.mu.Unlock()
	if !ok {
		return StreamStats{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	s.upSubs = nil
	s.downSubs = nil
	return s.statsLocked(), true
}

// Count returns the live stream count.
func (m *Manager) Count() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.streams)
}

// Subscribe attaches a chunk listener for one direction. The returned
// unsubscribe function is idempotent.
func (s *Stream) Subscribe(dir Direction, sub Subscriber) func() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return func() {}
	}
	s.nextSub++
	handle := s.nextSub
	if dir == Uplink {
		s.upSubs = append(s.upSubs, subEntry{id: handle, sub: sub})
	} else {
		s.downSubs = append(s.downSubs, subEntry{id: handle, sub: sub})
	}
	return func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if dir == Uplink {
			s.upSubs = removeSub(s.upSubs, handle)
		} else {
			s.downSubs = removeSub(s.downSubs, handle)
		}
	}
}

// Push feeds a chunk from the telephony side into one direction and
// fans it out to that direction's subscribers. Uplink chunks go to
// uplink subscribers (ASR), downlink to downlink (TTS consumers that
// mirror what the caller hears).
func (s *Stream) Push(dir Direction, data []byte) Chunk {
	s.mu.Lock()
	var subs []Subscriber
	if dir == Uplink {
		for _, e := range s.upSubs {
			subs = append(subs, e.sub)
		}
		s.upSeq++
		s.upBytes += uint64(len(data))
	} else {
		for _, e := range s.downSubs {
			subs = append(subs, e.sub)
		}
		s.downSeq++
		s.dnBytes += uint64(len(data))
		s.lastDownTS = time.Now()
	}
	seq := s.upSeq
	if dir == Downlink {
		seq = s.downSeq
	}
	// Copy before fan-out: producers often reuse the RTP buffer; async
	// subscribers must not observe overwritten bytes.
	dup := make([]byte, len(data))
	copy(dup, data)
	c := Chunk{Data: dup, Seq: seq, TS: time.Now()}
	s.mu.Unlock()

	for _, sub := range subs {
		sub.OnChunk(c)
	}
	return c
}

// Inject delivers AI-produced audio into the telephony uplink (TTS
// speaking to the caller). Same fanout semantics as Push.
func (s *Stream) Inject(data []byte) Chunk {
	return s.Push(Uplink, data)
}

// Echo records the round-trip of a downlink chunk that a subscriber
// processed and echoed back (the ASR->LLM->TTS loop proxy). It
// returns the measured RTT against the stream budget.
func (s *Stream) Echo() (rtt time.Duration, withinBudget bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lastDownTS.IsZero() {
		return 0, true
	}
	rtt = time.Since(s.lastDownTS)
	s.lastRTT = rtt
	if rtt > s.maxRTT {
		s.maxRTT = rtt
	}
	return rtt, rtt <= s.budget
}

// Stats returns the current snapshot.
func (s *Stream) Stats() StreamStats {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.statsLocked()
}

func (s *Stream) statsLocked() StreamStats {
	return StreamStats{
		UplinkChunks:   s.upSeq,
		DownlinkChunks: s.downSeq,
		UplinkBytes:    s.upBytes,
		DownlinkBytes:  s.dnBytes,
		LastRTTMs:      s.lastRTT.Milliseconds(),
		MaxRTTMs:       s.maxRTT.Milliseconds(),
		BudgetMaxMs:    s.budget.Milliseconds(),
	}
}

func removeSub(subs []subEntry, handle uint64) []subEntry {
	for i, e := range subs {
		if e.id == handle {
			subs[i] = subs[len(subs)-1]
			return subs[:len(subs)-1]
		}
	}
	return subs
}
