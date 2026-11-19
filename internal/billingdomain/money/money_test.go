package money

import "testing"

func TestFromYuanRoundTrip(t *testing.T) {
	cases := []float64{0, 0.055, 0.05, 10, 369.78, 10000, 0.0001, 1.9999}
	for _, y := range cases {
		a, err := FromYuan(y)
		if err != nil {
			t.Fatalf("FromYuan(%v): %v", y, err)
		}
		if got := a.AsFloat(); got != y {
			t.Errorf("round trip %v -> %v", y, got)
		}
	}
	if _, err := FromYuan(0.00001); err == nil {
		t.Error("5th decimal must be rejected")
	}
	if _, err := FromYuan(0.1234567); err == nil {
		t.Error("7-decimal input must be rejected (4-decimal scale)")
	}
}

func TestFromDecimalExact(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"0.055", "0.0550"}, {"12", "12.0000"}, {"0.0001", "0.0001"},
		{"-3.21", "-3.2100"}, {"369.78", "369.7800"}, {"0.0500", "0.0500"},
	}
	for _, c := range cases {
		a, err := FromDecimal(c.in)
		if err != nil {
			t.Fatalf("FromDecimal(%q): %v", c.in, err)
		}
		if a.String() != c.want {
			t.Errorf("%q -> %s, want %s", c.in, a.String(), c.want)
		}
	}
	if _, err := FromDecimal("0.12345"); err == nil {
		t.Error(">4 decimals rejected")
	}
}

func TestVOSCases(t *testing.T) {
	// The capacity report's zero-drift case: 66s at 0.05/min, 60+6.
	price := MustFromYuan(0.05)
	// init: 60s priced as price*60/60 = 0.05; inc 6s: 0.05*6/60 = 0.005.
	inc := price.MulFrac(6, 60)
	if inc.String() != "0.0050" {
		t.Errorf("6s increment = %s", inc)
	}
	// 66s billed = init(60) + one step(6) = 0.055 exactly.
	total := price.Add(inc)
	if total.String() != "0.0550" {
		t.Errorf("66s cost = %s, want 0.0550", total)
	}
	// Accumulate 6714 calls of that: integer closedness, zero drift.
	var sum Amount
	for i := 0; i < 6714; i++ {
		sum = sum.Add(total)
	}
	if sum.String() != "369.2700" {
		t.Errorf("6714 x 0.0550 = %s, want 369.2700", sum)
	}
}

func TestMulFracCeil(t *testing.T) {
	// 0.05 * 7 / 60 = 0.005833... -> ceil at 4 decimals = 0.0059.
	a := MustFromYuan(0.05)
	if got := a.MulFrac(7, 60); got.String() != "0.0059" {
		t.Errorf("ceil frac = %s, want 0.0059", got)
	}
	// Exact division stays exact.
	if got := a.MulFrac(60, 60); got.String() != "0.0500" {
		t.Errorf("identity frac = %s", got)
	}
}
