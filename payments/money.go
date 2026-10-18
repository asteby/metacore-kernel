package payments

import (
	"errors"
	"fmt"
	"math/big"
	"strings"

	"github.com/asteby/metacore-kernel/capability"
)

// ErrInvalidAmount is wrapped by every money parsing error.
var ErrInvalidAmount = errors.New("payments: invalid amount")

// Amount is an exact decimal parsed from the wire form ("1499.00"). The zero
// value is 0.
type Amount struct{ r big.Rat }

// ParseAmount parses a decimal string (capability.ValidDecimal: no exponent,
// no thousands separator, no float).
func ParseAmount(s string) (Amount, error) {
	var a Amount
	if !capability.ValidDecimal(s) {
		return a, fmt.Errorf("%w: %q is not a decimal string", ErrInvalidAmount, s)
	}
	if _, ok := a.r.SetString(s); !ok {
		return a, fmt.Errorf("%w: %q", ErrInvalidAmount, s)
	}
	return a, nil
}

// MustAmount parses s or panics (constants, tests).
func MustAmount(s string) Amount {
	a, err := ParseAmount(s)
	if err != nil {
		panic(err)
	}
	return a
}

// Cmp compares a and b (-1, 0, +1).
func (a Amount) Cmp(b Amount) int { return a.r.Cmp(&b.r) }

// Sign is -1, 0 or +1.
func (a Amount) Sign() int { return a.r.Sign() }

// Add returns a + b.
func (a Amount) Add(b Amount) Amount {
	var out Amount
	out.r.Add(&a.r, &b.r)
	return out
}

// Sub returns a - b.
func (a Amount) Sub(b Amount) Amount {
	var out Amount
	out.r.Sub(&a.r, &b.r)
	return out
}

// String renders the amount with at least two decimals and no trailing zeros
// beyond them ("10.00", "0.0825"), the wire form.
func (a Amount) String() string {
	s := a.r.FloatString(8)
	if i := strings.IndexByte(s, '.'); i >= 0 {
		s = strings.TrimRight(s, "0")
		if len(s)-i-1 < 2 {
			s += strings.Repeat("0", 2-(len(s)-i-1))
		}
	}
	if s == "-0.00" {
		s = "0.00"
	}
	return s
}

// Sum adds decimal strings; the first invalid one is an error.
func Sum(values ...string) (Amount, error) {
	var total Amount
	for _, v := range values {
		a, err := ParseAmount(v)
		if err != nil {
			return Amount{}, err
		}
		total = total.Add(a)
	}
	return total, nil
}
