package rate

import "testing"

func TestLongestPrefixMatch(t *testing.T) {
	tbl := NewTable([]Rate{
		{Prefix: "86", Price: 0.05, InitDur: 60, StepDur: 6},
		{Prefix: "86755", Price: 0.02, InitDur: 60, StepDur: 6},
		{Prefix: "8675513", Price: 0.015, InitDur: 60, StepDur: 6},
	})
	cases := []struct {
		num  string
		want string
	}{
		{"86755130123", "8675513"}, // longest wins
		{"8675598765", "86755"},    // mid prefix
		{"8610123456", "86"},       // base prefix
		{"8521234567", ""},         // no match
		{"", ""},
	}
	for _, c := range cases {
		e := tbl.Lookup(c.num, 1, 600)
		got := ""
		if e != nil {
			got = e.Prefix
		}
		if got != c.want {
			t.Errorf("Lookup(%q) = %q, want %q", c.num, got, c.want)
		}
	}
}

func TestTimeWindowOverride(t *testing.T) {
	tbl := NewTable([]Rate{
		{Prefix: "1", Price: 0.01, InitDur: 60, StepDur: 6}, // base
		{Prefix: "1", Price: 0.005, InitDur: 60, StepDur: 6,
			Effective: []Interval{{Days: []int{1, 2, 3, 4, 5}, Start: "20:00", End: "08:00"}}}, // night
	})
	// Monday 10:00 -> base price.
	if e := tbl.Lookup("12125550123", 1, 600); e == nil || e.Price != 0.01 {
		t.Errorf("weekday day price = %+v, want 0.01", e)
	}
	// Monday 23:00 -> night price (window wraps midnight).
	if e := tbl.Lookup("12125550123", 1, 23*60); e == nil || e.Price != 0.005 {
		t.Errorf("weekday night price = %+v, want 0.005", e)
	}
	// Tuesday 06:00 -> night price (before 08:00).
	if e := tbl.Lookup("12125550123", 2, 6*60); e == nil || e.Price != 0.005 {
		t.Errorf("early morning price = %+v, want 0.005", e)
	}
	// Saturday 22:00 -> base (window excludes Sat).
	if e := tbl.Lookup("12125550123", 6, 22*60); e == nil || e.Price != 0.01 {
		t.Errorf("weekend price = %+v, want 0.01", e)
	}
}

func TestRoundUp7(t *testing.T) {
	cases := []struct{ in, want float64 }{
		{0.12345678, 0.1234568}, // 8th digit rounds up
		{0.1234567, 0.1234567},  // exact 7 decimals
		{0, 0},
		{-0.1, -0.1},
		{0.1, 0.1},
	}
	for _, c := range cases {
		if got := roundUp7(c.in); got != c.want {
			t.Errorf("roundUp7(%v) = %v, want %v", c.in, got, c.want)
		}
	}
}

func BenchmarkLookup(b *testing.B) {
	var rs []Rate
	for i := 0; i < 100000; i++ {
		rs = append(rs, Rate{Prefix: prefixFor(i), Price: 0.01, InitDur: 60, StepDur: 6})
	}
	tbl := NewTable(rs)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		tbl.Lookup("8613800138000", 1, 600)
	}
}

func prefixFor(i int) string {
	// deterministic 5-8 digit prefixes
	s := ""
	n := i
	for j := 0; j < 6; j++ {
		s += string(rune('0' + n%10))
		n /= 10
	}
	return "86" + s
}
