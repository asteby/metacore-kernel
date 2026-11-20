package money

import "testing"

func TestDecimalAddSubCeilTo(t *testing.T) {
	a, b := MustDecimal("1.25"), MustDecimal("0.005")
	if got := a.Add(b).String(); got != "1.255" {
		t.Fatalf("add %s", got)
	}
	if got := a.Sub(MustDecimal("2")).String(); got != "-0.75" {
		t.Fatalf("sub %s", got)
	}
	five := MustDecimal("5")
	for in, want := range map[string]string{"1": "5", "5": "5", "5.01": "10", "0": "0", "-1": "0", "-5.5": "-5", "2164.999999": "2165"} {
		got, err := MustDecimal(in).CeilTo(five)
		if err != nil || got.Cmp(MustDecimal(want)) != 0 {
			t.Fatalf("ceil5(%s)=%s want %s (%v)", in, got, want, err)
		}
	}
	if _, err := a.CeilTo(MustDecimal("0")); err == nil {
		t.Fatal("multiple must be > 0")
	}
	if got, _ := MustDecimal("10.01").CeilTo(MustDecimal("0.05")); got.String() != "10.05" {
		t.Fatalf("fractional multiple %s", got)
	}
}

func TestDecimalSQL(t *testing.T) {
	var d Decimal
	for _, c := range []struct {
		src  any
		want string
	}{{"12.50", "12.50"}, {[]byte("3.1400"), "3.1400"}, {int64(7), "7"}, {1.5, "1.5"}} {
		if err := d.Scan(c.src); err != nil || d.String() != c.want {
			t.Fatalf("scan %v: %v %s", c.src, err, d)
		}
	}
	if err := d.Scan(nil); err != nil || !d.IsZero() {
		t.Fatal("nil scans to zero")
	}
	if err := d.Scan(struct{}{}); err == nil {
		t.Fatal("unsupported type must fail")
	}
	v, _ := MustDecimal("9.90").Value()
	if v != "9.90" {
		t.Fatalf("value %v", v)
	}
}
