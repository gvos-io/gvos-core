// Package money is the billing-domain money type: Amount is a
// fixed-point int64 with a 1e-4 yuan scale (ADR-0003, the VOS
// school's 20-year drift-free practice). All balance, price and cost
// arithmetic runs in Amount; the decimal world (PG NUMERIC, JSON,
// Kamailio payloads) is only touched at the edges via the string
// converters here.
//
// Named type, not bare int64: the compiler rejects accidental
// Amount/second/counter arithmetic — the whole point of the ADR.
package money

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// Scale: 1 Amount unit = 1e-4 yuan ("厘" position). 9e14 yuan headroom
// (int64 max / 1e4) is far beyond any carrier balance.
const Scale = 10_000

// Amount is a fixed-point money value (1 unit = 0.0001 yuan).
type Amount int64

// FromYuan converts a yuan float to Amount. Input beyond 7 decimals
// is an error (the legacy roundUp7 precision contract); use
// FromDecimal for exact strings.
func FromYuan(y float64) (Amount, error) {
	if y < -9e14 || y > 9e14 {
		return 0, fmt.Errorf("money: %v out of range", y)
	}
	// Round-half-up at the scale edge, then verify the input did not
	// carry more precision than the type holds.
	a := Amount(y*Scale + 0.5)
	if y < 0 {
		a = Amount(y*Scale - 0.5)
	}
	if back := a.AsFloat(); !nearlyEqual(back, y) {
		return 0, fmt.Errorf("money: %v has more than 4 decimal places", y)
	}
	return a, nil
}

// MustFromYuan is FromYuan for literals and tests.
func MustFromYuan(y float64) Amount {
	a, err := FromYuan(y)
	if err != nil {
		panic(err)
	}
	return a
}

// FromDecimal parses "12.3456" (yuan) exactly; >4 decimals error.
func FromDecimal(s string) (Amount, error) {
	s = strings.TrimSpace(s)
	neg := strings.HasPrefix(s, "-")
	if neg {
		s = s[1:]
	}
	intPart, fracPart, hasFrac := strings.Cut(s, ".")
	if intPart == "" {
		intPart = "0"
	}
	whole, err := strconv.ParseInt(intPart, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("money: bad decimal %q", s)
	}
	var frac int64
	if hasFrac {
		for len(fracPart) > 4 {
			if fracPart[len(fracPart)-1] != '0' {
				return 0, fmt.Errorf("money: %q has more than 4 decimal places", s)
			}
			fracPart = fracPart[:len(fracPart)-1] // tolerate trailing zeros
		}
		for len(fracPart) < 4 {
			fracPart += "0"
		}
		if frac, err = strconv.ParseInt(fracPart, 10, 64); err != nil {
			return 0, fmt.Errorf("money: bad decimal %q", s)
		}
	}
	a := Amount(whole*Scale + frac)
	if neg {
		a = -a
	}
	return a, nil
}

// AsFloat returns the yuan value (display/comparison only; never
// re-arithmetic).
func (a Amount) AsFloat() float64 { return float64(a) / Scale }

// String renders the exact decimal, 4 places ("0.0550"). This is the
// canonical wire form (JSON/PG NUMERIC).
func (a Amount) String() string {
	neg := a < 0
	if neg {
		a = -a
	}
	return fmt.Sprintf("%s%d.%04d", map[bool]string{true: "-", false: ""}[neg], int64(a)/Scale, int64(a)%Scale)
}

// Add/Sub/Mul-per-second helpers — the Charger's only operations.
func (a Amount) Add(b Amount) Amount { return a + b }
func (a Amount) Sub(b Amount) Amount { return a - b }

// MulFrac scales by a rational (num/den) with ceil rounding: the
// price-per-second × seconds computation, preserving the legacy
// "8th decimal rounds up" contract as ceil-at-4.
func (a Amount) MulFrac(num, den int64) Amount {
	if den == 0 {
		panic("money: zero denominator")
	}
	n := int64(a) * num
	q := n / den
	if n%den != 0 && (n > 0) == (den > 0) {
		q++ // ceil away from zero for positive products
	}
	return Amount(q)
}

// nearlyEqual guards FromYuan's precision check at 1e-9.
func nearlyEqual(a, b float64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d < 1e-9
}

// MarshalJSON renders the canonical decimal string ("0.0550") — the
// API wire form (api-conventions: money fields are decimal strings).
func (a Amount) MarshalJSON() ([]byte, error) { return []byte(`"` + a.String() + `"`), nil }

// UnmarshalJSON accepts both a decimal string and a bare number.
func (a *Amount) UnmarshalJSON(b []byte) error {
	s := strings.TrimSpace(string(b))
	if strings.HasPrefix(s, `"`) {
		v, err := FromDecimal(strings.Trim(s, `"`))
		if err != nil {
			return err
		}
		*a = v
		return nil
	}
	// Bare JSON number: parse via float (edge input, e.g. 10 or 5.5).
	var f float64
	if err := json.Unmarshal(b, &f); err != nil {
		return err
	}
	v, err := FromYuan(f)
	if err != nil {
		return err
	}
	*a = v
	return nil
}
