package money

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"testing"
)

// GoldenSHA256 is the sha256 of testdata/golden.json. The SDK copy
// (@asteby/metacore-lib src/__fixtures__/money.golden.json) pins the same
// hash: change both together.
const GoldenSHA256 = "8394487d835e0ecf7d336af3a66de6a69e7fc13debb32ba59d48fb08af07711c"

type gMoney struct {
	Amount       string `json:"amount"`
	CurrencyCode string `json:"currency_code"`
}

type golden struct {
	Version int `json:"version"`
	ISO     []struct {
		Code               string `json:"code"`
		MinorUnits         int    `json:"minor_units"`
		CashIncrementMinor int64  `json:"cash_increment_minor"`
	} `json:"iso"`
	RoundingModes []string `json:"rounding_modes"`
	Cases         struct {
		ParseDecimal []struct {
			Input  string `json:"input"`
			String string `json:"string"`
			Error  string `json:"error"`
		} `json:"parse_decimal"`
		Parse []struct {
			Name         string `json:"name"`
			CurrencyCode string `json:"currency_code"`
			Input        string `json:"input"`
			Amount       string `json:"amount"`
			Minor        string `json:"minor"`
			Error        string `json:"error"`
		} `json:"parse"`
		ParseRound []struct {
			Name         string `json:"name"`
			CurrencyCode string `json:"currency_code"`
			Input        string `json:"input"`
			Mode         string `json:"mode"`
			Amount       string `json:"amount"`
			Error        string `json:"error"`
		} `json:"parse_round"`
		Add []gBinary `json:"add"`
		Sub []gBinary `json:"sub"`
		Neg []struct {
			Name   string `json:"name"`
			Money  gMoney `json:"money"`
			Amount string `json:"amount"`
			Error  string `json:"error"`
		} `json:"neg"`
		MulDecimal []struct {
			Name   string `json:"name"`
			Money  gMoney `json:"money"`
			Factor string `json:"factor"`
			Mode   string `json:"mode"`
			Amount string `json:"amount"`
			Error  string `json:"error"`
		} `json:"mul_decimal"`
		Allocate []struct {
			Name    string   `json:"name"`
			Money   gMoney   `json:"money"`
			Weights []int64  `json:"weights"`
			Parts   []string `json:"parts"`
			Error   string   `json:"error"`
		} `json:"allocate"`
		RoundCash []struct {
			Name   string `json:"name"`
			Money  gMoney `json:"money"`
			Mode   string `json:"mode"`
			Amount string `json:"amount"`
		} `json:"round_cash"`
		SumRoundCash []struct {
			Name         string   `json:"name"`
			CurrencyCode string   `json:"currency_code"`
			Lines        []string `json:"lines"`
			Mode         string   `json:"mode"`
			Sum          string   `json:"sum"`
			Cash         string   `json:"cash"`
		} `json:"sum_round_cash"`
		DecimalQuo []struct {
			A      string `json:"a"`
			B      string `json:"b"`
			Scale  int32  `json:"scale"`
			Mode   string `json:"mode"`
			Result string `json:"result"`
			Error  string `json:"error"`
		} `json:"decimal_quo"`
		DecimalMul []struct {
			A      string `json:"a"`
			B      string `json:"b"`
			Result string `json:"result"`
		} `json:"decimal_mul"`
		Inverse []struct {
			Value   string `json:"value"`
			Scale   int32  `json:"scale"`
			Inverse string `json:"inverse"`
			Error   string `json:"error"`
		} `json:"inverse"`
		Convert []struct {
			Name  string `json:"name"`
			Money gMoney `json:"money"`
			Rate  struct {
				Base  string `json:"base"`
				Quote string `json:"quote"`
				Value string `json:"value"`
			} `json:"rate"`
			To     string `json:"to"`
			Mode   string `json:"mode"`
			Amount string `json:"amount"`
			Error  string `json:"error"`
		} `json:"convert"`
		JSON []struct {
			Money gMoney `json:"money"`
			JSON  string `json:"json"`
		} `json:"json"`
		JSONDecode []struct {
			JSON         string `json:"json"`
			Amount       string `json:"amount"`
			CurrencyCode string `json:"currency_code"`
			Error        string `json:"error"`
		} `json:"json_decode"`
	} `json:"cases"`
}

type gBinary struct {
	Name   string `json:"name"`
	A      gMoney `json:"a"`
	B      gMoney `json:"b"`
	Amount string `json:"amount"`
	Error  string `json:"error"`
}

func loadGolden(t *testing.T) golden {
	t.Helper()
	raw, err := os.ReadFile("testdata/golden.json")
	if err != nil {
		t.Fatal(err)
	}
	var g golden
	if err := json.Unmarshal(raw, &g); err != nil {
		t.Fatal(err)
	}
	if g.Version != 1 {
		t.Fatalf("golden version %d, this test understands 1", g.Version)
	}
	return g
}

func TestGoldenHash(t *testing.T) {
	raw, err := os.ReadFile("testdata/golden.json")
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	if got := hex.EncodeToString(sum[:]); got != GoldenSHA256 {
		t.Fatalf("golden.json sha256 = %s, pinned %s: update GoldenSHA256 here AND copy the file to "+
			"@asteby/metacore-lib (src/__fixtures__/money.golden.json, KERNEL_GOLDEN_SHA256)", got, GoldenSHA256)
	}
}

// errCode maps an error to the golden error code ("" for nil).
func errCode(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, ErrCurrencyMismatch):
		return "currency_mismatch"
	case errors.Is(err, ErrOverflow):
		return "overflow"
	case errors.Is(err, ErrPrecision):
		return "precision"
	case errors.Is(err, ErrInvalid):
		return "invalid"
	}
	return "unexpected: " + err.Error()
}

func gm(t *testing.T, v gMoney) Money {
	t.Helper()
	m, err := Parse(v.Amount, MustISO(v.CurrencyCode))
	if err != nil {
		t.Fatalf("golden money %+v: %v", v, err)
	}
	return m
}

func mode(t *testing.T, s string) RoundingMode {
	t.Helper()
	m, err := ParseRoundingMode(s)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// check compares a result against an expected amount or error code.
func check(t *testing.T, name string, got Money, err error, wantAmount, wantErr string) {
	t.Helper()
	if wantErr != "" {
		if c := errCode(err); c != wantErr {
			t.Errorf("%s: error %q (%v), want %q (result %s)", name, c, err, wantErr, got)
		}
		return
	}
	if err != nil {
		t.Errorf("%s: unexpected error %v", name, err)
		return
	}
	if got.String() != wantAmount {
		t.Errorf("%s: got %s, want %s", name, got, wantAmount)
	}
}

func TestISOTableMatchesGolden(t *testing.T) {
	g := loadGolden(t)
	codes := make([]string, 0, len(g.ISO))
	for _, e := range g.ISO {
		codes = append(codes, e.Code)
		s, ok := ISO(e.Code)
		if !ok {
			t.Errorf("ISO(%s) missing", e.Code)
			continue
		}
		if s.MinorUnits != e.MinorUnits || s.CashIncrementMinor != e.CashIncrementMinor {
			t.Errorf("ISO(%s) = %d/%d, golden %d/%d", e.Code, s.MinorUnits, s.CashIncrementMinor, e.MinorUnits, e.CashIncrementMinor)
		}
	}
	if !reflect.DeepEqual(codes, ISOCodes()) {
		t.Errorf("ISO codes differ from golden:\n go     %v\n golden %v", ISOCodes(), codes)
	}
	var names []string
	for m := HalfAwayFromZero; m.valid(); m++ {
		names = append(names, m.String())
	}
	if !reflect.DeepEqual(names, g.RoundingModes) {
		t.Errorf("rounding modes %v, golden %v", names, g.RoundingModes)
	}
}

func TestGolden(t *testing.T) {
	g := loadGolden(t)
	c := g.Cases
	n := 0

	for _, tc := range c.ParseDecimal {
		n++
		d, err := ParseDecimal(tc.Input)
		if tc.Error != "" {
			if errCode(err) != tc.Error {
				t.Errorf("ParseDecimal(%q) = %v, %v; want %s", tc.Input, d, err, tc.Error)
			}
			continue
		}
		if err != nil || d.String() != tc.String {
			t.Errorf("ParseDecimal(%q) = %v, %v; want %s", tc.Input, d, err, tc.String)
		}
	}
	for _, tc := range c.Parse {
		n++
		m, err := Parse(tc.Input, MustISO(tc.CurrencyCode))
		check(t, "parse "+tc.Name, m, err, tc.Amount, tc.Error)
		if tc.Error == "" && err == nil {
			if got := NewDecimal(m.Minor(), 0).String(); got != tc.Minor {
				t.Errorf("parse %s: minor %s, want %s", tc.Name, got, tc.Minor)
			}
		}
	}
	for _, tc := range c.ParseRound {
		n++
		m, err := ParseRound(tc.Input, MustISO(tc.CurrencyCode), mode(t, tc.Mode))
		check(t, "parse_round "+tc.Name, m, err, tc.Amount, tc.Error)
	}
	for _, tc := range c.Add {
		n++
		m, err := gm(t, tc.A).Add(gm(t, tc.B))
		check(t, "add "+tc.Name, m, err, tc.Amount, tc.Error)
	}
	for _, tc := range c.Sub {
		n++
		m, err := gm(t, tc.A).Sub(gm(t, tc.B))
		check(t, "sub "+tc.Name, m, err, tc.Amount, tc.Error)
	}
	for _, tc := range c.Neg {
		n++
		m, err := gm(t, tc.Money).Neg()
		check(t, "neg "+tc.Name, m, err, tc.Amount, tc.Error)
	}
	for _, tc := range c.MulDecimal {
		n++
		m, err := gm(t, tc.Money).MulDecimal(MustDecimal(tc.Factor), mode(t, tc.Mode))
		check(t, "mul_decimal "+tc.Name, m, err, tc.Amount, tc.Error)
	}
	for _, tc := range c.Allocate {
		n++
		parts, err := gm(t, tc.Money).Allocate(tc.Weights)
		if tc.Error != "" {
			if errCode(err) != tc.Error {
				t.Errorf("allocate %s: %v, want %s", tc.Name, err, tc.Error)
			}
			continue
		}
		if err != nil {
			t.Errorf("allocate %s: %v", tc.Name, err)
			continue
		}
		got := make([]string, len(parts))
		for i, p := range parts {
			got[i] = p.String()
		}
		if !reflect.DeepEqual(got, tc.Parts) {
			t.Errorf("allocate %s: %v, want %v", tc.Name, got, tc.Parts)
		}
	}
	for _, tc := range c.RoundCash {
		n++
		m, err := gm(t, tc.Money).RoundCash(mode(t, tc.Mode))
		check(t, "round_cash "+tc.Name, m, err, tc.Amount, "")
	}
	for _, tc := range c.SumRoundCash {
		n++
		spec := MustISO(tc.CurrencyCode)
		sum := Zero(spec)
		for _, l := range tc.Lines {
			var err error
			if sum, err = sum.Add(gm(t, gMoney{l, tc.CurrencyCode})); err != nil {
				t.Fatal(err)
			}
		}
		check(t, "sum "+tc.Name, sum, nil, tc.Sum, "")
		cash, err := sum.RoundCash(mode(t, tc.Mode))
		check(t, "sum_round_cash "+tc.Name, cash, err, tc.Cash, "")
	}
	for _, tc := range c.DecimalQuo {
		n++
		d, err := MustDecimal(tc.A).Quo(MustDecimal(tc.B), tc.Scale, mode(t, tc.Mode))
		if tc.Error != "" {
			if errCode(err) != tc.Error {
				t.Errorf("quo %s/%s: %v, want %s", tc.A, tc.B, err, tc.Error)
			}
			continue
		}
		if err != nil || d.String() != tc.Result {
			t.Errorf("quo %s/%s scale %d %s = %s, %v; want %s", tc.A, tc.B, tc.Scale, tc.Mode, d, err, tc.Result)
		}
	}
	for _, tc := range c.DecimalMul {
		n++
		if got := MustDecimal(tc.A).Mul(MustDecimal(tc.B)).String(); got != tc.Result {
			t.Errorf("mul %s×%s = %s, want %s", tc.A, tc.B, got, tc.Result)
		}
	}
	for _, tc := range c.Inverse {
		n++
		r := Rate{Base: "USD", Quote: "MXN", Value: MustDecimal(tc.Value), ID: "r1", Kind: "mid"}
		inv, err := r.Inverse(tc.Scale)
		if tc.Error != "" {
			if errCode(err) != tc.Error {
				t.Errorf("inverse %s: %v, want %s", tc.Value, err, tc.Error)
			}
			continue
		}
		if err != nil || inv.Value.String() != tc.Inverse {
			t.Errorf("inverse %s = %s, %v; want %s", tc.Value, inv.Value, err, tc.Inverse)
			continue
		}
		if inv.Base != "MXN" || inv.Quote != "USD" || !inv.Inverted || !inv.Derived || inv.ID != "r1" || inv.Kind != "mid" {
			t.Errorf("inverse metadata %+v", inv)
		}
	}
	for _, tc := range c.Convert {
		n++
		r := Rate{Base: tc.Rate.Base, Quote: tc.Rate.Quote, Value: MustDecimal(tc.Rate.Value)}
		m, err := Convert(gm(t, tc.Money), r, MustISO(tc.To), mode(t, tc.Mode))
		check(t, "convert "+tc.Name, m, err, tc.Amount, tc.Error)
		if err == nil && m.Currency() != tc.To {
			t.Errorf("convert %s: currency %s, want %s", tc.Name, m.Currency(), tc.To)
		}
	}
	for _, tc := range c.JSON {
		n++
		b, err := json.Marshal(gm(t, tc.Money))
		if err != nil || string(b) != tc.JSON {
			t.Errorf("json %+v = %s, %v; want %s", tc.Money, b, err, tc.JSON)
		}
		var back Money
		if err := json.Unmarshal(b, &back); err != nil || back != gm(t, tc.Money) {
			t.Errorf("json round trip %s = %v, %v", b, back, err)
		}
	}
	for _, tc := range c.JSONDecode {
		n++
		var m Money
		err := json.Unmarshal([]byte(tc.JSON), &m)
		check(t, "json_decode "+tc.JSON, m, err, tc.Amount, tc.Error)
		if err == nil && m.Currency() != tc.CurrencyCode {
			t.Errorf("json_decode %s: currency %s", tc.JSON, m.Currency())
		}
	}
	if n < 300 {
		t.Fatalf("only %d golden vectors ran", n)
	}
	t.Logf("%d golden vectors", n)
}
