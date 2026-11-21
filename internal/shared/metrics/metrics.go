// Package metrics is a dependency-free Prometheus exposition registry.
//
// The platform deliberately avoids pulling in client_golang: the metric
// surface is small (call lifecycle, bus health, billing writes), the
// text exposition format is trivial to emit correctly, and the build
// stays hermetic. Semantics follow the Prometheus conventions: counters
// are cumulative and monotonic, gauges can go either way, histograms
// bucket cumulative observations with _bucket/_sum/_count series.
package metrics

import (
	"fmt"
	"sort"
	"strings"
	"sync"
)

// Registry holds every metric the process exports. A single process
// registry is the expected shape; NewRegistry exists for tests.
type Registry struct {
	mu         sync.Mutex
	counters   map[string]*Counter
	gauges     map[string]*Gauge
	histograms map[string]*Histogram
	helps      map[string]string
}

func NewRegistry() *Registry {
	return &Registry{
		counters:   map[string]*Counter{},
		gauges:     map[string]*Gauge{},
		histograms: map[string]*Histogram{},
		helps:      map[string]string{},
	}
}

// Default is the process-wide registry the /metrics handler renders.
var Default = NewRegistry()

// Counter is a cumulative monotonic metric.
type Counter struct {
	v float64
}

// Inc adds delta (which may be fractional for aggregate callers).
func (c *Counter) Inc(delta float64) {
	metricsMu.Lock()
	c.v += delta
	metricsMu.Unlock()
}

func (c *Counter) value() float64 { return c.v }

// Gauge is a point-in-time value that can rise and fall.
type Gauge struct {
	v float64
}

// Set replaces the value.
func (g *Gauge) Set(v float64) {
	metricsMu.Lock()
	g.v = v
	metricsMu.Unlock()
}

func (g *Gauge) value() float64 { return g.v }

// Histogram buckets cumulative observations (Prometheus cumulative
// bucket semantics, +Inf implicit, sum and count tracked separately).
type Histogram struct {
	bounds []float64
	counts []uint64
	sum    float64
	count  uint64
}

// Observe records one value.
func (h *Histogram) Observe(v float64) {
	metricsMu.Lock()
	defer metricsMu.Unlock()
	h.sum += v
	h.count++
	for i, b := range h.bounds {
		if v <= b {
			h.counts[i]++
		}
	}
}

func (h *Histogram) write(b *strings.Builder, name, help string) {
	fmt.Fprintf(b, "# HELP %s %s\n# TYPE %s histogram\n", name, help, name)
	for i, bound := range h.bounds {
		fmt.Fprintf(b, "%s_bucket{le=\"%s\"} %d\n", name, formatFloat(bound), h.counts[i])
	}
	fmt.Fprintf(b, "%s_bucket{le=\"+Inf\"} %d\n", name, h.count)
	fmt.Fprintf(b, "%s_sum %s\n", name, formatFloat(h.sum))
	fmt.Fprintf(b, "%s_count %d\n", name, h.count)
}

// metricsMu serializes value mutation. One lock for every metric type
// keeps the registry code trivial; contention is irrelevant at the
// rates this platform exposes (per-call, not per-packet).
var metricsMu sync.Mutex

func formatFloat(v float64) string {
	s := fmt.Sprintf("%g", v)
	return s
}

// Counter registers (or returns) a named counter. Names must be valid
// Prometheus metric names ([a-zA-Z_:][a-zA-Z0-9_:]*); a duplicate
// registration with the same name returns the existing metric so
// callers can register idempotently.
func (r *Registry) Counter(name, help string) *Counter {
	metricsMu.Lock()
	defer metricsMu.Unlock()
	if c, ok := r.counters[name]; ok {
		return c
	}
	c := &Counter{}
	r.counters[name] = c
	r.helps[name] = help
	return c
}

// Gauge registers (or returns) a named gauge.
func (r *Registry) Gauge(name, help string) *Gauge {
	metricsMu.Lock()
	defer metricsMu.Unlock()
	if g, ok := r.gauges[name]; ok {
		return g
	}
	g := &Gauge{}
	r.gauges[name] = g
	r.helps[name] = help
	return g
}

// Histogram registers (or returns) a named histogram with explicit
// cumulative bucket bounds (sorted ascending; +Inf is implicit).
func (r *Registry) Histogram(name, help string, bounds []float64) *Histogram {
	metricsMu.Lock()
	defer metricsMu.Unlock()
	if h, ok := r.histograms[name]; ok {
		return h
	}
	h := &Histogram{bounds: bounds, counts: make([]uint64, len(bounds))}
	r.histograms[name] = h
	r.helps[name] = help
	return h
}

// Render writes the registry in the Prometheus text exposition format
// (version 0.0.4), series sorted by name for stable scrapes.
func (r *Registry) Render() string {
	metricsMu.Lock()
	defer metricsMu.Unlock()
	var b strings.Builder
	names := make([]string, 0, len(r.helps))
	for n := range r.helps {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		help := r.helps[n]
		if c, ok := r.counters[n]; ok {
			fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s counter\n%s %s\n", n, help, n, n, formatFloat(c.value()))
			continue
		}
		if g, ok := r.gauges[n]; ok {
			fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s gauge\n%s %s\n", n, help, n, n, formatFloat(g.value()))
			continue
		}
		if h, ok := r.histograms[n]; ok {
			h.write(&b, n, help)
			continue
		}
	}
	return b.String()
}
