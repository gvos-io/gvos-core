// Circuit breaker for the online gateway directory (M12 / CB-1,
// docs/gateway-circuit-breaker-design.md): the legacy gateways table
// carried locked_until for a one-time bootstrap path only — the live
// settings-owned directory had no failure accounting, so a dead
// gateway kept top routing priority forever (failover masked it while
// every call paid the retry cost).
//
// Accounting runs off the settlement hook (callEnd → Observe), never
// on the call path itself. Per-gateway state machine:
//
//	healthy ──N consecutive failures──> tripped(cooldown T)
//	  ▲                                    │ cooldown expiry
//	  └──── one success ────────────── half-open (probe slot)
//	                                        │ probe success → healthy
//	                                        │ probe failure → tripped(T×2, cap)
//
// 404/486 are explicitly NOT failures (callee faults are not gateway
// faults — tripping a good gateway on an empty number is the most
// expensive mistake a breaker can make). Counters are per-replica;
// state transitions converge via cfg.reload. When EVERY gateway is
// tripped the route face falls back to the full set (a working route
// with an alarm beats no route at all).
package gateway

import (
	"fmt"
	"log"
	"os"
	"strconv"
	"sync"
	"time"
)

// BreakerState names one gateway's breaker phase.
type BreakerState string

const (
	BreakerHealthy  BreakerState = "healthy"
	BreakerTripped  BreakerState = "tripped"
	BreakerHalfOpen BreakerState = "half-open"
)

// BreakerConfig tunes the state machine (settings key
// gateway_breaker; env GVOS_BREAKER_* seeds the defaults).
type BreakerConfig struct {
	ConsecutiveFailures int     `json:"consecutive_failures"` // trip after N in a row
	WindowSeconds       int     `json:"window_seconds"`       // failure-rate window
	WindowMinCalls      int     `json:"window_min_calls"`     // min samples before rate matters
	FailureRate         float64 `json:"failure_rate"`         // window failure threshold
	CooldownS           int     `json:"cooldown_s"`           // base cooldown
	CooldownMaxS        int     `json:"cooldown_max_s"`       // exponential cap
}

// DefaultBreakerConfig returns the design-doc defaults (env-tunable).
func DefaultBreakerConfig() BreakerConfig {
	cfg := BreakerConfig{
		ConsecutiveFailures: 5,
		WindowSeconds:       120,
		WindowMinCalls:      10,
		FailureRate:         0.5,
		CooldownS:           300,
		CooldownMaxS:        3600,
	}
	envInt := func(name string, dst *int) {
		if v := os.Getenv(name); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				*dst = n
			}
		}
	}
	envInt("GVOS_BREAKER_CONSECUTIVE_FAILURES", &cfg.ConsecutiveFailures)
	envInt("GVOS_BREAKER_WINDOW_SECONDS", &cfg.WindowSeconds)
	envInt("GVOS_BREAKER_WINDOW_MIN_CALLS", &cfg.WindowMinCalls)
	if v := os.Getenv("GVOS_BREAKER_FAILURE_RATE"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f > 0 && f <= 1 {
			cfg.FailureRate = f
		}
	}
	envInt("GVOS_BREAKER_COOLDOWN_S", &cfg.CooldownS)
	envInt("GVOS_BREAKER_COOLDOWN_MAX_S", &cfg.CooldownMaxS)
	return cfg
}

// breakerGateway is one gateway's accounting state.
type breakerGateway struct {
	consecutive int
	windowStart time.Time
	windowTotal int
	windowFail  int
	state       BreakerState
	tripsUntil  time.Time
	backoff     int // doubled cooldown multiplier count
}

// Breaker is the per-replica accounting table. onTransition is called
// with (gatewayID, old, new) whenever a phase change happens — the
// server wires it to the cfg.reload broadcast + observation event.
type Breaker struct {
	mu    sync.Mutex
	cfg   BreakerConfig
	gws   map[string]*breakerGateway
	onTx  func(gatewayID string, from, to BreakerState)
	clock func() time.Time // injectable for tests
}

// NewBreaker wires one breaker.
func NewBreaker(cfg BreakerConfig, onTransition func(gatewayID string, from, to BreakerState)) *Breaker {
	return &Breaker{
		cfg:   cfg,
		gws:   map[string]*breakerGateway{},
		onTx:  onTransition,
		clock: time.Now,
	}
}

// BreakerView is the observation projection (admin face).
type BreakerView struct {
	State        BreakerState `json:"state"`
	Consecutive  int          `json:"consecutive_failures"`
	WindowTotal  int          `json:"window_calls"`
	WindowFail   int          `json:"window_failures"`
	TripsUntil   string       `json:"trips_until,omitempty"`
	CooldownBase int          `json:"cooldown_base_s"`
}

// View projects one gateway's state (healthy defaults when unseen).
func (b *Breaker) View(gatewayID string) BreakerView {
	b.mu.Lock()
	defer b.mu.Unlock()
	g, ok := b.gws[gatewayID]
	if !ok {
		return BreakerView{State: BreakerHealthy, CooldownBase: b.cfg.CooldownS}
	}
	v := BreakerView{
		State: g.state, Consecutive: g.consecutive,
		WindowTotal: g.windowTotal, WindowFail: g.windowFail,
		CooldownBase: b.cfg.CooldownS,
	}
	if !g.tripsUntil.IsZero() && g.state == BreakerTripped {
		v.TripsUntil = g.tripsUntil.Format(time.RFC3339)
	}
	return v
}

// TrippedOut reports whether the gateway must leave the route set
// (tripped = excluded; half-open stays as the trailing probe slot).
func (b *Breaker) TrippedOut(gatewayID string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	g, ok := b.gws[gatewayID]
	if !ok {
		return false
	}
	b.maybeExpire(g, gatewayID)
	return g.state == BreakerTripped
}

// AllTripped reports whether every gateway in the directory is
// currently tripped (the route face falls back to the full set in
// that case). total is the directory size — untracked gateways count
// as healthy (no evidence, no breaker), so one failing gateway among
// two does NOT trigger the fallback.
func (b *Breaker) AllTripped(total int) bool {
	if total == 0 {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	tripped := 0
	for _, g := range b.gws {
		b.maybeExpireLocked(g)
		if g.state == BreakerTripped {
			tripped++
		}
	}
	return tripped >= total
}

// Observe records one settled call outcome. gatewayID "" (registered
// user termination, no carrier leg) is ignored. sipCode semantics:
// 2xx answered = success; 404/486 = NOT counted (callee fault);
// other 4xx/5xx/0-timeout = failure.
func (b *Breaker) Observe(gatewayID string, sipCode int, answered bool, release string) (from, to BreakerState, changed bool) {
	if gatewayID == "" {
		return "", "", false
	}
	success := answered && sipCode >= 200 && sipCode < 300
	if !success {
		// Callee-side faults are not gateway faults.
		switch sipCode {
		case 404, 486:
			return "", "", false
		}
		// A normal release after a connected call is a success even
		// when the final SIP code on the by-path is a 4xx.
		if release == "normal" && answered {
			success = true
		}
	}

	b.mu.Lock()
	g, ok := b.gws[gatewayID]
	if !ok {
		g = &breakerGateway{state: BreakerHealthy, windowStart: b.clock()}
		b.gws[gatewayID] = g
	}
	b.maybeExpireLocked(g)
	before := g.state

	// Window accounting (both outcomes sample the window).
	now := b.clock()
	if now.Sub(g.windowStart) > time.Duration(b.cfg.WindowSeconds)*time.Second {
		g.windowStart = now
		g.windowTotal, g.windowFail = 0, 0
	}
	g.windowTotal++
	if success {
		// One success clears the consecutive streak; half-open probes
		// and tripped recovery close on it. The backoff multiplier is
		// FORGIVEN only on a natural half-open probe success — a
		// success squeezed through during cooldown (all-tripped
		// fallback) keeps the escalation, or repeated trips could
		// never reach the cap.
		g.consecutive = 0
		if g.state == BreakerHalfOpen {
			g.state = BreakerHealthy
			g.backoff = 0
			g.tripsUntil = time.Time{}
			g.windowTotal, g.windowFail = 0, 0
		} else if g.state == BreakerTripped {
			// A success during cooldown (probe call routed through the
			// all-tripped fallback) recovers too — but keeps the
			// backoff escalation.
			g.state = BreakerHealthy
			g.tripsUntil = time.Time{}
		}
	} else {
		g.consecutive++
		g.windowFail++
	}
	// Trip conditions: consecutive streak, or a fat window.
	trip := false
	if !success {
		if g.consecutive >= b.cfg.ConsecutiveFailures {
			trip = true
		}
		if g.windowTotal >= b.cfg.WindowMinCalls &&
			float64(g.windowFail)/float64(g.windowTotal) >= b.cfg.FailureRate {
			trip = true
		}
	}
	if trip && g.state != BreakerTripped {
		g.backoff++
		cooldown := time.Duration(b.cfg.CooldownS*(1<<(g.backoff-1))) * time.Second
		if max := time.Duration(b.cfg.CooldownMaxS) * time.Second; cooldown > max {
			cooldown = max
		}
		g.state = BreakerTripped
		g.tripsUntil = now.Add(cooldown)
	} else if g.state == BreakerTripped && b.clock().After(g.tripsUntil) {
		g.state = BreakerHalfOpen
	}
	b.maybeExpireLocked(g)
	after := g.state
	b.mu.Unlock()

	if before != after && b.onTx != nil {
		log.Printf("BREAKER gateway %s: %s -> %s", gatewayID, before, after)
		b.onTx(gatewayID, before, after)
	}
	if before != after {
		return before, after, true
	}
	return before, after, false
}

// Unlock clears one gateway manually (admin face): state healthy,
// counters zeroed. Returns whether it changed anything.
func (b *Breaker) Unlock(gatewayID string) (from, to BreakerState, changed bool) {
	b.mu.Lock()
	g, ok := b.gws[gatewayID]
	if !ok {
		b.mu.Unlock()
		return "", "", false
	}
	before := g.state
	g.consecutive = 0
	g.windowTotal, g.windowFail = 0, 0
	g.backoff = 0
	g.tripsUntil = time.Time{}
	g.state = BreakerHealthy
	after := g.state
	b.mu.Unlock()
	return before, after, before != after
}

// maybeExpire flips expired cooldowns to half-open (locked caller).
func (b *Breaker) maybeExpire(g *breakerGateway, gatewayID string) {
	b.maybeExpireLocked(g)
}

func (b *Breaker) maybeExpireLocked(g *breakerGateway) {
	if g.state == BreakerTripped && b.clock().After(g.tripsUntil) {
		g.state = BreakerHalfOpen
	}
}

// ReplayWindow is the boot-time reconstruction query shape: the caller
// aggregates the CDR window and feeds the same observations.
func (b *Breaker) Replay(obs []struct {
	GatewayID string
	SipCode   int
	Answered  bool
	Release   string
}) {
	for _, o := range obs {
		b.Observe(o.GatewayID, o.SipCode, o.Answered, o.Release)
	}
}

// String renders the state for logs.
func (s BreakerState) String() string { return fmt.Sprintf("%s", string(s)) }
