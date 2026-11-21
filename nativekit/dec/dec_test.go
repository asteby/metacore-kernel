package dec

import (
	"encoding/json"
	"testing"

	"github.com/asteby/metacore-kernel/money"
)

func TestParseFormat(t *testing.T) {
	cases := map[string]string{"12": "12.00", "12.5": "12.50", "-0.000001": "-0.000001", ".5": "0.50", "1.230000": "1.23"}
	for in, want := range cases {
		d, err := Parse(in)
		if err != nil || d.String() != want {
			t.Fatalf("%s -> %v %v want %s", in, d, err, want)
		}
	}
	for _, bad := range []string{"", "a", "1.1234567", "1,5", "--1"} {
		if _, err := Parse(bad); err == nil {
			t.Fatalf("%q should fail", bad)
		}
	}
}

func TestMulDivRound(t *testing.T) {
	if got := MustParse("0.1").Add(MustParse("0.2")); got != MustParse("0.3") {
		t.Fatalf("0.1+0.2 = %s", got)
	}
	if got := MustParse("19.99").Mul(FromInt(3)); got.String() != "59.97" {
		t.Fatal(got)
	}
	if got, _ := MustParse("116").Div(MustParse("1.16")); got.String() != "100.00" {
		t.Fatal(got)
	}
	if got := MustParse("2.345").Round(2); got.String() != "2.35" {
		t.Fatal(got)
	}
	if got := MustParse("-2.345").Round(2); got.String() != "-2.35" {
		t.Fatal(got)
	}
	if got := MustParse("200").Pct(MustParse("16")); got.String() != "32.00" {
		t.Fatal(got)
	}
	if _, ok := FromInt(1).Div(0); ok {
		t.Fatal("div by zero must report false")
	}
}

func TestCeilTo(t *testing.T) {
	five := FromInt(5)
	for in, want := range map[string]string{"1.00": "5.00", "5.00": "5.00", "5.01": "10.00", "0": "0.00", "2165.00": "2165.00", "2164.999999": "2165.00"} {
		if got := MustParse(in).CeilTo(five); got.String() != want {
			t.Fatalf("ceil5(%s)=%s want %s", in, got, want)
		}
	}
}

func TestJSONAndScan(t *testing.T) {
	b, _ := json.Marshal(struct{ P D }{MustParse("9.9")})
	if string(b) != `{"P":"9.90"}` {
		t.Fatal(string(b))
	}
	var x struct{ P D }
	if err := json.Unmarshal([]byte(`{"P":"10.25"}`), &x); err != nil || x.P.String() != "10.25" {
		t.Fatal(err, x)
	}
	var d D
	if err := d.Scan([]byte("3.1400")); err != nil || d.String() != "3.14" {
		t.Fatal(err, d)
	}
	if err := d.Scan(float64(1.5)); err != nil || d.String() != "1.50" {
		t.Fatal(err, d)
	}
}

func TestAddChecked(t *testing.T) {
	if v, ok := FromInt(1).AddChecked(FromInt(2)); !ok || v != FromInt(3) {
		t.Fatal(v, ok)
	}
	if _, ok := D(1<<62 + 1<<61).AddChecked(D(1 << 62)); ok {
		t.Fatal("overflow must be reported")
	}
	if _, ok := D(-(1 << 62) - (1 << 61)).AddChecked(D(-(1 << 62))); ok {
		t.Fatal("negative overflow must be reported")
	}
}

func TestMoneyBridge(t *testing.T) {
	d := MustParse("123.456789")
	m := d.ToMoney()
	back, err := FromMoney(m)
	if err != nil || back != d {
		t.Fatal(back, err)
	}
	if _, err := FromMoney(money.MustDecimal("0.0000001")); err == nil {
		t.Fatal("sub-micro precision must not convert silently")
	}
}

func TestComparable(t *testing.T) {
	m := map[D]int{MustParse("1.5"): 1}
	if m[MustParse("1.50")] != 1 || !(MustParse("1.5") < MustParse("1.6")) {
		t.Fatal("D must be usable with == < and as a map key")
	}
}
