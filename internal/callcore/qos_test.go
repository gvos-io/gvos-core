package callcore

import (
	"math"
	"testing"
)

func TestParseRTPStat(t *testing.T) {
	mos, jit, loss := ParseRTPStat("packet loss=1.2%, jitter=3 ms, round trip time=15 ms, MOS=4.2")
	if math.Abs(mos-4.2) > 1e-9 || math.Abs(jit-3) > 1e-9 || math.Abs(loss-1.2) > 1e-9 {
		t.Errorf("parsed mos=%v jit=%v loss=%v", mos, jit, loss)
	}
	// Kamailio's escaped form with pipes.
	mos, jit, loss = ParseRTPStat("packet loss=0.00%||jitter=0 ms||MOS=4.4")
	if math.Abs(mos-4.4) > 1e-9 || loss != 0 {
		t.Errorf("pipe form mos=%v loss=%v", mos, loss)
	}
	// Empty/garbage parse to zeros.
	if m, j, l := ParseRTPStat(""); m != 0 || j != 0 || l != 0 {
		t.Errorf("empty: %v %v %v", m, j, l)
	}
}
