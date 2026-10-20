package money

import (
	"encoding/json"
	"errors"
	"testing"
)

func TestDefaultModeIsHalfAwayFromZero(t *testing.T) {
	var m RoundingMode
	if m != HalfAwayFromZero || m.String() != "halfAwayFromZero" {
		t.Fatalf("zero RoundingMode = %s", m)
	}
	if _, err := ParseRoundingMode("bankers"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unknown mode: %v", err)
	}
	if _, err := New(1, MustISO("MXN")).MulDecimal(MustDecimal("1"), RoundingMode(99)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("invalid mode accepted: %v", err)
	}
	b, _ := json.Marshal(struct{ M RoundingMode }{HalfEven})
	if string(b) != `{"M":"halfEven"}` {
		t.Fatalf("mode json %s", b)
	}
}

func TestCmpSignAndMismatch(t *testing.T) {
	mxn, usd := MustISO("MXN"), MustISO("USD")
	a, b := New(100, mxn), New(-5, mxn)
	if c, err := a.Cmp(b); err != nil || c != 1 {
		t.Fatalf("cmp %d %v", c, err)
	}
	if c, _ := b.Cmp(a); c != -1 {
		t.Fatal("cmp reversed")
	}
	if a.Sign() != 1 || b.Sign() != -1 || !Zero(mxn).IsZero() || Zero(mxn).Sign() != 0 {
		t.Fatal("sign")
	}
	if _, err := a.Cmp(New(100, usd)); !errors.Is(err, ErrCurrencyMismatch) {
		t.Fatalf("cmp mismatch %v", err)
	}
}

func TestSpecOverrides(t *testing.T) {
	mxn := MustISO("mxn") // case-insensitive
	if mxn.Code != "MXN" || mxn.MinorUnits != 2 || mxn.CashIncrementMinor != 0 || mxn.Scope != ScopeLine {
		t.Fatalf("MXN spec %+v", mxn)
	}
	if _, ok := ISO("XAU"); ok {
		t.Fatal("metal codes have no minor units and must not be listed")
	}
	// An organization rounds MXN cash to 0.50.
	org := mxn
	org.CashIncrementMinor = 50
	m, err := New(1234, mxn).WithSpec(org)
	if err != nil {
		t.Fatal(err)
	}
	if c, _ := m.RoundCash(HalfAwayFromZero); c.String() != "12.50" {
		t.Fatalf("org cash rounding %s", c)
	}
	if _, err := m.WithSpec(MustISO("JPY")); !errors.Is(err, ErrCurrencyMismatch) {
		t.Fatalf("WithSpec other currency: %v", err)
	}
	for _, bad := range []Spec{{}, {Code: "X", MinorUnits: -1}, {Code: "X", MinorUnits: 19}, {Code: "X", CashIncrementMinor: -5}, {Code: "X", Scope: "doc"}} {
		if _, err := Parse("1", bad); !errors.Is(err, ErrInvalid) {
			t.Errorf("spec %+v accepted: %v", bad, err)
		}
	}
}

func TestRateJSON(t *testing.T) {
	r := Rate{ID: "r1", Base: "USD", Quote: "MXN", Value: MustDecimal("17.2345"), Kind: "official"}
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"id":"r1","base":"USD","quote":"MXN","value":"17.2345","kind":"official"}`
	if string(b) != want {
		t.Fatalf("rate json %s", b)
	}
	var back Rate
	if err := json.Unmarshal(b, &back); err != nil || back.Value.Cmp(r.Value) != 0 || back.Base != "USD" {
		t.Fatalf("rate round trip %+v %v", back, err)
	}
}

func TestDecimalZeroValueAndRound(t *testing.T) {
	var d Decimal
	if d.String() != "0" || !d.IsZero() || d.Neg().String() != "0" {
		t.Fatalf("zero decimal %q", d.String())
	}
	if NewDecimal(15, -2).String() != "1500" {
		t.Fatal("negative scale")
	}
	r, err := MustDecimal("2.675").Round(2, HalfEven)
	if err != nil || r.String() != "2.68" {
		t.Fatalf("round %s %v", r, err)
	}
	if _, err := MustDecimal("1").Quo(MustDecimal("3"), -1, HalfEven); !errors.Is(err, ErrInvalid) {
		t.Fatal("negative scale accepted")
	}
}
