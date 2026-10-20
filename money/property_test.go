package money

import (
	"errors"
	"math"
	"math/rand"
	"reflect"
	"strings"
	"testing"
	"testing/quick"
)

var propSpecs = []Spec{MustISO("MXN"), MustISO("JPY"), MustISO("KWD"), MustISO("CLF"), MustISO("CHF"), MustISO("SEK")}

// arb is a random Money with a bounded amount so sums stay in range.
type arb struct{ M Money }

func (arb) Generate(r *rand.Rand, _ int) reflect.Value {
	spec := propSpecs[r.Intn(len(propSpecs))]
	var minor int64
	switch r.Intn(4) {
	case 0:
		minor = r.Int63n(1000) - 500
	case 1:
		minor = r.Int63n(1e12) - 5e11
	default:
		minor = r.Int63n(1<<60) - 1<<59
	}
	return reflect.ValueOf(arb{New(minor, spec)})
}

type arbWeights []int64

func (arbWeights) Generate(r *rand.Rand, _ int) reflect.Value {
	n := 1 + r.Intn(12)
	w := make(arbWeights, n)
	for i := range w {
		switch r.Intn(4) {
		case 0:
			w[i] = 0
		case 1:
			w[i] = r.Int63n(math.MaxInt64 / 16)
		default:
			w[i] = r.Int63n(100)
		}
	}
	w[r.Intn(n)]++ // never all zero
	return reflect.ValueOf(w)
}

type arbMode RoundingMode

func (arbMode) Generate(r *rand.Rand, _ int) reflect.Value {
	return reflect.ValueOf(arbMode(r.Intn(len(modeNames))))
}

var quickCfg = &quick.Config{MaxCount: 3000, Rand: rand.New(rand.NewSource(20261019))}

func decimals(s string) int {
	if i := strings.IndexByte(s, '.'); i >= 0 {
		return len(s) - i - 1
	}
	return 0
}

func TestPropAllocateSumsToTotal(t *testing.T) {
	f := func(a arb, w arbWeights) bool {
		parts, err := a.M.Allocate(w)
		if err != nil {
			t.Logf("allocate %s %v: %v", a.M, w, err)
			return false
		}
		sum := Zero(a.M.Spec())
		for i, p := range parts {
			if w[i] == 0 && !p.IsZero() {
				return false
			}
			if p.Sign()*a.M.Sign() < 0 {
				return false
			}
			if sum, err = sum.Add(p); err != nil {
				return false
			}
		}
		return sum == a.M
	}
	if err := quick.Check(f, quickCfg); err != nil {
		t.Fatal(err)
	}
	// Extremes.
	for _, m := range []int64{math.MinInt64, math.MaxInt64, -1, 0, 1} {
		parts, err := New(m, MustISO("MXN")).Allocate([]int64{math.MaxInt64, 1, 3})
		if err != nil {
			t.Fatal(err)
		}
		var s int64
		for _, p := range parts {
			s += p.Minor() // wraps exactly like the total for MinInt64
		}
		if s != m {
			t.Fatalf("allocate %d: parts sum %d", m, s)
		}
	}
}

func TestPropParseStringRoundTrip(t *testing.T) {
	f := func(a arb) bool {
		s := a.M.String()
		if decimals(s) != a.M.Spec().MinorUnits {
			return false
		}
		p, err := Parse(s, a.M.Spec())
		return err == nil && p == a.M
	}
	if err := quick.Check(f, quickCfg); err != nil {
		t.Fatal(err)
	}
	for _, m := range []int64{math.MinInt64, math.MaxInt64} {
		x := New(m, MustISO("KWD"))
		if p, err := Parse(x.String(), x.Spec()); err != nil || p != x {
			t.Fatalf("round trip %s: %v %v", x, p, err)
		}
	}
}

func TestPropAddCommutativeAssociative(t *testing.T) {
	f := func(a, b, c arb) bool {
		spec := a.M.Spec()
		x, y, z := a.M, New(b.M.Minor(), spec), New(c.M.Minor(), spec)
		xy, err1 := x.Add(y)
		yx, err2 := y.Add(x)
		if (err1 == nil) != (err2 == nil) || xy != yx {
			return false
		}
		if err1 != nil {
			return errors.Is(err1, ErrOverflow)
		}
		l, errL := xy.Add(z)
		yz, errYZ := y.Add(z)
		if errL != nil || errYZ != nil {
			return true // overflow on an intermediate sum is allowed
		}
		r, errR := x.Add(yz)
		return errR == nil && l == r
	}
	if err := quick.Check(f, quickCfg); err != nil {
		t.Fatal(err)
	}
}

func TestPropConvertRoundTrip(t *testing.T) {
	// Holds when the forward conversion does not lose information, i.e. the
	// target is at least as fine-grained as the source: rate ≥ 1 between
	// currencies with the same minor units (USD→MXN, EUR→VES...).
	usd, mxn := MustISO("USD"), MustISO("MXN")
	r := rand.New(rand.NewSource(17))
	for i := 0; i < 5000; i++ {
		rate := Rate{Base: "USD", Quote: "MXN", Value: NewDecimal(1_000_000+r.Int63n(100_000_000), 6)}
		inv, err := rate.Inverse(InverseScale)
		if err != nil {
			t.Fatal(err)
		}
		m := New(r.Int63n(2e9)-1e9, usd) // up to ±10M USD: inverse error stays < 0.1 cent
		md := arbMode(r.Intn(len(modeNames)))
		there, err := Convert(m, rate, mxn, RoundingMode(md))
		if err != nil {
			t.Fatal(err)
		}
		back, err := Convert(there, inv, usd, HalfAwayFromZero)
		if err != nil {
			t.Fatal(err)
		}
		d, _ := back.Sub(m)
		if d.Minor() > 1 || d.Minor() < -1 {
			t.Fatalf("%s USD @%s (%s) → %s MXN → %s USD (inverse %s)", m, rate.Value, RoundingMode(md), there, back, inv.Value)
		}
	}
}

func TestPropRoundCashIdempotent(t *testing.T) {
	f := func(a arb, md arbMode) bool {
		once, err := a.M.RoundCash(RoundingMode(md))
		if err != nil {
			return errors.Is(err, ErrOverflow)
		}
		twice, err := once.RoundCash(RoundingMode(md))
		if err != nil || twice != once {
			return false
		}
		inc := a.M.Spec().CashIncrementMinor
		return inc <= 1 || once.Minor()%inc == 0
	}
	if err := quick.Check(f, quickCfg); err != nil {
		t.Fatal(err)
	}
}

func TestPropNoExtraDecimals(t *testing.T) {
	f := func(a, b arb, md arbMode, num int64, scale uint8, w arbWeights) bool {
		mode := RoundingMode(md)
		spec := a.M.Spec()
		var out []Money
		push := func(m Money, err error) {
			if err == nil {
				out = append(out, m)
			}
		}
		push(a.M.Add(New(b.M.Minor(), spec)))
		push(a.M.Sub(New(b.M.Minor(), spec)))
		push(a.M.Neg())
		push(a.M.MulDecimal(NewDecimal(num%1_000_000_000, int32(scale%14)), mode))
		push(a.M.RoundCash(mode))
		push(ParseRound(NewDecimal(num, int32(scale%20)).String(), spec, mode))
		push(Convert(a.M, Rate{Base: spec.Code, Quote: "KWD", Value: NewDecimal(1+abs64(num%1e9), int32(scale%13))}, MustISO("KWD"), mode))
		if parts, err := a.M.Allocate(w); err == nil {
			out = append(out, parts...)
		}
		for _, m := range out {
			if decimals(m.String()) != m.Spec().MinorUnits {
				return false
			}
		}
		return len(out) > 0
	}
	if err := quick.Check(f, quickCfg); err != nil {
		t.Fatal(err)
	}
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

func FuzzParseDecimal(f *testing.F) {
	for _, s := range []string{"0", "-12.50", "+3", "1e3", "1,000", "NaN", "--1", ".5", "5.", "",
		"92233720368547758.08", strings.Repeat("9", 300), "-0.00", "1.2.3", "\x00", "٣"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		d, err := ParseDecimal(s)
		if err != nil {
			if !errors.Is(err, ErrInvalid) {
				t.Fatalf("ParseDecimal(%q): untyped error %v", s, err)
			}
			return
		}
		// Canonical form re-parses to the same value.
		back, err := ParseDecimal(d.String())
		if err != nil || back.Cmp(d) != 0 || back.Scale() != d.Scale() {
			t.Fatalf("ParseDecimal(%q) = %s, re-parse %s %v", s, d, back, err)
		}
		for _, spec := range propSpecs {
			m, err := ParseRound(s, spec, HalfEven)
			if err != nil {
				if !errors.Is(err, ErrOverflow) {
					t.Fatalf("ParseRound(%q): %v", s, err)
				}
				continue
			}
			if decimals(m.String()) != spec.MinorUnits {
				t.Fatalf("ParseRound(%q) = %s", s, m)
			}
			if _, err := Parse(s, spec); err != nil && !errors.Is(err, ErrPrecision) && !errors.Is(err, ErrOverflow) {
				t.Fatalf("Parse(%q): %v", s, err)
			}
		}
	})
}
