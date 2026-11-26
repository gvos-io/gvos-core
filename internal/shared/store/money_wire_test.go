package store

import (
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/gvos-io/gvos-core/internal/billingdomain/money"
)

// money.Amount must reach a NUMERIC parameter as yuan, whether it is
// passed directly or via .String(). pgx would otherwise encode the
// underlying int64 as-is and the column would receive the value x10^4
// (the audit finding: every CDR and ledger row stored 500 for 0.05).
func TestMoneyAmountEncodesAsYuan(t *testing.T) {
	m := pgtype.NewMap()
	for _, tc := range []struct {
		yuan string
	}{
		{"0.05"}, {"0.066"}, {"-0.066"}, {"10"}, {"0.0001"},
	} {
		want, err := money.FromDecimal(tc.yuan)
		if err != nil {
			t.Fatal(err)
		}
		// A plain string has no binary plan for numeric: pgx's conn
		// layer retries it as text, which is what the call sites rely on.
		for _, form := range []struct {
			name   string
			arg    any
			format int16
		}{{"direct", want, pgtype.BinaryFormatCode}, {"String()", want.String(), pgtype.TextFormatCode}} {
			buf, err := m.Encode(uint32(pgtype.NumericOID), form.format, form.arg, nil)
			if err != nil {
				t.Fatalf("%s as %s: encode: %v", tc.yuan, form.name, err)
			}
			var n pgtype.Numeric
			if err := m.Scan(pgtype.NumericOID, form.format, buf, &n); err != nil {
				t.Fatalf("%s as %s: scan: %v", tc.yuan, form.name, err)
			}
			raw, err := n.Value()
			if err != nil {
				t.Fatalf("%s as %s: value: %v", tc.yuan, form.name, err)
			}
			txt, _ := raw.(string)
			got, err := money.FromDecimal(txt)
			if err != nil {
				t.Fatalf("%s as %s: %q: %v", tc.yuan, form.name, txt, err)
			}
			if got != want {
				t.Errorf("%s passed %s -> numeric column receives %s", tc.yuan, form.name, got)
			}
		}
	}
}
