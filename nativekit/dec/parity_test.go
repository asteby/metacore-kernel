package dec

import (
	"testing"

	"github.com/asteby/metacore-kernel/money"
)

// Parity of dec.D with the kernel money.Decimal . Safety
// net so both decimals agree on the shared operations: same results for the
// operations modules use, and the bridge is lossless.
func TestParityWithKernelMoneyDecimal(t *testing.T) {
	pairs := [][2]string{{"0.1", "0.2"}, {"19.99", "3"}, {"2.345", "1"}, {"-2.345", "1"}, {"116", "1.16"}, {"2164.999999", "5"}, {"5.01", "5"}, {"0", "5"}, {"123456.789012", "0.000001"}}
	for _, p := range pairs {
		a, b := MustParse(p[0]), MustParse(p[1])
		ka, kb := money.MustDecimal(p[0]), money.MustDecimal(p[1])
		if got, want := a.Add(b).String(), ka.Add(kb).String(); money.MustDecimal(got).Cmp(money.MustDecimal(want)) != 0 {
			t.Errorf("Add(%s,%s): dec=%s kernel=%s", p[0], p[1], got, want)
		}
		if got, want := a.Sub(b).String(), ka.Sub(kb).String(); money.MustDecimal(got).Cmp(money.MustDecimal(want)) != 0 {
			t.Errorf("Sub(%s,%s): dec=%s kernel=%s", p[0], p[1], got, want)
		}
		km, _ := ka.Mul(kb).Round(2, money.HalfAwayFromZero)
		if got := a.Mul(b).Round(2).String(); money.MustDecimal(got).Cmp(km) != 0 {
			t.Errorf("Mul+Round(%s,%s): dec=%s kernel=%s", p[0], p[1], got, km)
		}
		if kc, err := ka.CeilTo(kb); err == nil && b > 0 {
			if got := a.CeilTo(b).String(); money.MustDecimal(got).Cmp(kc) != 0 {
				t.Errorf("CeilTo(%s,%s): dec=%s kernel=%s", p[0], p[1], got, kc)
			}
		}
	}
}
