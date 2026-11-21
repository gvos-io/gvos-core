package metrics

import (
	"strings"
	"testing"
)

func TestCounterAndGaugeRender(t *testing.T) {
	r := NewRegistry()
	c := r.Counter("gvos_calls_started_total", "calls started")
	c.Inc(1)
	c.Inc(4.5)
	g := r.Gauge("gvos_calls_live", "live calls")
	g.Set(12)

	out := r.Render()
	for _, want := range []string{
		"# HELP gvos_calls_started_total calls started",
		"# TYPE gvos_calls_started_total counter",
		"gvos_calls_started_total 5.5",
		"# TYPE gvos_calls_live gauge",
		"gvos_calls_live 12",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("render missing %q\n%s", want, out)
		}
	}
}

func TestHistogramCumulativeBuckets(t *testing.T) {
	r := NewRegistry()
	h := r.Histogram("gvos_rpc_seconds", "rpc seconds", []float64{0.005, 0.05, 1})
	h.Observe(0.003) // <= 0.005
	h.Observe(0.02)  // <= 0.05
	h.Observe(3)     // > 1, only +Inf
	out := r.Render()
	for _, want := range []string{
		`gvos_rpc_seconds_bucket{le="0.005"} 1`,
		`gvos_rpc_seconds_bucket{le="0.05"} 2`,
		`gvos_rpc_seconds_bucket{le="1"} 2`,
		`gvos_rpc_seconds_bucket{le="+Inf"} 3`,
		"gvos_rpc_seconds_sum 3.023",
		"gvos_rpc_seconds_count 3",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("render missing %q\n%s", want, out)
		}
	}
}

func TestIdempotentRegistration(t *testing.T) {
	r := NewRegistry()
	a := r.Counter("m_total", "x")
	a.Inc(2)
	b := r.Counter("m_total", "x") // same name returns the live metric
	b.Inc(1)
	if got := r.Render(); !strings.Contains(got, "m_total 3") {
		t.Errorf("duplicate registration lost increments:\n%s", got)
	}
}
