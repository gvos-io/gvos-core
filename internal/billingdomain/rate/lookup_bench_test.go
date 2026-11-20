package rate

import (
	"testing"

	"github.com/gvos-io/gvos-core/internal/billingdomain/money"
)

func BenchmarkLookupFast(b *testing.B) {
	var rs []Rate
	for i := 0; i < 100000; i++ {
		rs = append(rs, Rate{Prefix: prefixFor(i), Price: money.MustFromYuan(0.01), InitDur: 60, StepDur: 6})
	}
	tbl := NewTable(rs)
	b.ResetTimer()
	b.ReportAllocs()
	var hit bool
	for i := 0; i < b.N; i++ {
		if tbl.Lookup("8613800138000", 1, 600) != nil {
			hit = true
		}
	}
	_ = hit
}
