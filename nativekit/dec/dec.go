// Package dec is a comparable fixed-point decimal for native modules: money,
// costs and quantities that live in ledgers and balances. Six decimals in an
// int64, so == and < are exact and no float ever touches a balance.
//
// It complements money.Decimal rather than replacing it: money.Decimal is a
// pointer-backed struct (arbitrary precision, currency rules), so == and < on
// it are silently wrong. D is a plain integer: compare with the operators,
// use it as a map key, scan it from numeric(p,6) columns. Use money.Decimal at
// the edges where currency or wider precision matter and D inside module
// ledgers where exact, comparable arithmetic is the point.
//
// It implements sql.Scanner / driver.Valuer / json so it maps to numeric
// columns and to the "money is always a decimal string" HTTP contract.
package dec

import (
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/bits"
	"strconv"
	"strings"
)

const (
	// Places is the number of stored decimals.
	Places = 6
	scale  = 1_000_000
)

// D is a fixed-point decimal (value * 1e6).
type D int64

// Zero is 0.
const Zero D = 0

// ErrSyntax / ErrRange are returned by Parse.
var (
	ErrSyntax = errors.New("dec: invalid decimal")
	ErrRange  = errors.New("dec: out of range")
)

// FromInt builds a decimal from an integer.
func FromInt(n int64) D { return D(n * scale) }

// Parse reads "123", "-1.5", "0.000001". More than 6 significant decimals is
// an error (the contract rejects silent truncation).
func Parse(s string) (D, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, ErrSyntax
	}
	neg := false
	if s[0] == '-' || s[0] == '+' {
		neg = s[0] == '-'
		s = s[1:]
	}
	ip, fp, _ := strings.Cut(s, ".")
	if ip == "" && fp == "" {
		return 0, ErrSyntax
	}
	if len(fp) > Places {
		// numeric(18,8) exports carry trailing zeros beyond the scale.
		if strings.Trim(fp[Places:], "0") != "" {
			return 0, ErrRange
		}
		fp = fp[:Places]
	}
	for _, r := range ip + fp {
		if r < '0' || r > '9' {
			return 0, ErrSyntax
		}
	}
	if ip == "" {
		ip = "0"
	}
	iv, err := strconv.ParseInt(ip, 10, 64)
	if err != nil || iv > math.MaxInt64/scale-1 {
		return 0, ErrRange
	}
	fp += strings.Repeat("0", Places-len(fp))
	fv, _ := strconv.ParseInt(fp, 10, 64)
	v := D(iv*scale + fv)
	if neg {
		v = -v
	}
	return v, nil
}

// MustParse is Parse for constants and tests.
func MustParse(s string) D {
	v, err := Parse(s)
	if err != nil {
		panic(err)
	}
	return v
}

// String renders with at least 2 and at most 6 decimals ("12.50", "0.333333").
func (d D) String() string { return d.Format(2) }

// Format renders with at least min decimals (trailing zeros beyond min trimmed).
func (d D) Format(min int) string {
	neg := d < 0
	u := uint64(d)
	if neg {
		u = uint64(-d)
	}
	ip, fp := u/scale, u%scale
	f := strings.TrimRight(fmt.Sprintf("%06d", fp), "0")
	for len(f) < min {
		f += "0"
	}
	s := strconv.FormatUint(ip, 10)
	if f != "" {
		s += "." + f
	}
	if neg {
		s = "-" + s
	}
	return s
}

// Float is for display/logging only, never for arithmetic.
func (d D) Float() float64 { return float64(d) / scale }

// Add / Sub / Neg / Abs / Sign / Cmp / IsZero are the obvious operations.
func (d D) Add(o D) D { return d + o }
func (d D) Sub(o D) D { return d - o }
func (d D) Neg() D    { return -d }
func (d D) Abs() D {
	if d < 0 {
		return -d
	}
	return d
}
func (d D) Sign() int {
	switch {
	case d < 0:
		return -1
	case d > 0:
		return 1
	}
	return 0
}
func (d D) Cmp(o D) int  { return (d - o).Sign() }
func (d D) IsZero() bool { return d == 0 }

// Min / Max of two decimals.
func Min(a, b D) D {
	if a < b {
		return a
	}
	return b
}

func Max(a, b D) D {
	if a > b {
		return a
	}
	return b
}

// Mul multiplies rounding half away from zero at 6 decimals.
func (d D) Mul(o D) D { return mulDiv(int64(d), int64(o), scale) }

// Div divides rounding half away from zero at 6 decimals. Division by zero
// returns 0 and false.
func (d D) Div(o D) (D, bool) {
	if o == 0 {
		return 0, false
	}
	return mulDiv(int64(d), scale, int64(o)), true
}

// Pct returns d * p / 100 (p is a percentage such as 16 or 12.5).
func (d D) Pct(p D) D { return mulDiv(int64(d), int64(p), scale*100) }

// Round rounds half away from zero to `places` decimals (0..6).
func (d D) Round(places int) D {
	if places >= Places {
		return d
	}
	if places < 0 {
		places = 0
	}
	step := int64(1)
	for i := 0; i < Places-places; i++ {
		step *= 10
	}
	return mulDiv(int64(d), 1, step) * D(step)
}

// CeilTo rounds UP to the next multiple of m (m > 0). A common price rule
// "round up to a multiple of 5" is CeilTo(FromInt(5)).
func (d D) CeilTo(m D) D {
	if m <= 0 {
		return d
	}
	q := int64(d) / int64(m)
	if int64(d)%int64(m) > 0 {
		q++
	}
	return D(q * int64(m))
}

// mulDiv computes round-half-away(a*b/c) with a 128-bit intermediate.
func mulDiv(a, b, c int64) D {
	neg := (a < 0) != (b < 0)
	if c < 0 {
		neg = !neg
		c = -c
	}
	hi, lo := bits.Mul64(abs64(a), abs64(b))
	if hi >= uint64(c) { // quotient overflows uint64: far outside money range
		return D(math.MaxInt64)
	}
	q, r := bits.Div64(hi, lo, uint64(c))
	if r >= uint64(c)-r { // 2r >= c without overflow
		q++
	}
	v := int64(q)
	if neg {
		v = -v
	}
	return D(v)
}

func abs64(x int64) uint64 {
	if x < 0 {
		return uint64(-x)
	}
	return uint64(x)
}

// MarshalJSON emits a decimal string ("12.50"), the HTTP money contract.
func (d D) MarshalJSON() ([]byte, error) { return json.Marshal(d.String()) }

// UnmarshalJSON accepts a decimal string (preferred) or a JSON number.
func (d *D) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	if s == "null" {
		return nil
	}
	v, err := Parse(s)
	if err != nil {
		return err
	}
	*d = v
	return nil
}

// Value implements driver.Valuer (decimal text; Postgres casts to numeric).
func (d D) Value() (driver.Value, error) { return d.Format(Places), nil }

// Scan implements sql.Scanner for numeric columns (text, bytes, int or float).
func (d *D) Scan(src any) error {
	switch v := src.(type) {
	case nil:
		*d = 0
		return nil
	case string:
		return d.scanString(v)
	case []byte:
		return d.scanString(string(v))
	case int64:
		*d = FromInt(v)
		return nil
	case float64:
		*d = D(math.Round(v * scale))
		return nil
	}
	return fmt.Errorf("dec: cannot scan %T", src)
}

func (d *D) scanString(s string) error {
	v, err := Parse(s)
	if err != nil {
		return err
	}
	*d = v
	return nil
}
