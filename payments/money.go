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

// MaxScale is the most decimals an Amount accepts. Money is quantized to its
// currency's minor units (CheckQuantized); the extra room is for rates and
// intermediate values, and it keeps every sum and difference exact.
const MaxScale = 6

// Amount is an exact decimal parsed from the wire form ("1499.00"). The zero
// value is 0. Every Amount has at most MaxScale decimals, so String never
// loses precision.
//
// Amount is this package's internal decimal arithmetic, NOT the platform's
// money type: records keep money as a decimal string next to its ISO 4217
// currency, and that wire form is the contract. When the kernel's Money
// type lands, Amount becomes a thin adapter over it (or an alias); the
// records and the function signatures that take strings do not change.
type Amount struct{ r big.Rat }

// ParseAmount parses a decimal string (capability.ValidDecimal: no exponent,
// no thousands separator, no float) with at most MaxScale decimals.
func ParseAmount(s string) (Amount, error) {
	var a Amount
	if !capability.ValidDecimal(s) {
		return a, fmt.Errorf("%w: %q is not a decimal string", ErrInvalidAmount, s)
	}
	if i := strings.IndexByte(s, '.'); i >= 0 && len(s)-i-1 > MaxScale {
		return a, fmt.Errorf("%w: %q has more than %d decimals", ErrInvalidAmount, s, MaxScale)
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

// Mul returns a × b (exact; quantize before storing).
func (a Amount) Mul(b Amount) Amount {
	var out Amount
	out.r.Mul(&a.r, &b.r)
	return out
}

// Max returns the larger of a and b.
func Max(a, b Amount) Amount {
	if a.Cmp(b) >= 0 {
		return a
	}
	return b
}

// String renders the amount exactly, with at least two decimals and no
// trailing zeros beyond them ("10.00", "0.0825"): the wire form.
func (a Amount) String() string {
	s := a.r.FloatString(MaxScale)
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

// scale is the number of significant decimals of a.
func (a Amount) scale() int {
	s := a.r.FloatString(MaxScale)
	i := strings.IndexByte(s, '.')
	if i < 0 {
		return 0
	}
	return len(strings.TrimRight(s[i+1:], "0"))
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

// minorUnits are the ISO 4217 currencies whose minor unit is not 2.
// Provisional: when the kernel's currency catalog (Money) lands, MinorUnits
// reads it and this table goes away. MinorUnits is the only seam.
var minorUnits = map[string]int{
	"BIF": 0, "CLP": 0, "DJF": 0, "GNF": 0, "ISK": 0, "JPY": 0, "KMF": 0, "KRW": 0, "PYG": 0,
	"RWF": 0, "UGX": 0, "UYI": 0, "VND": 0, "VUV": 0, "XAF": 0, "XOF": 0, "XPF": 0,
	"BHD": 3, "IQD": 3, "JOD": 3, "KWD": 3, "LYD": 3, "OMR": 3, "TND": 3,
	"CLF": 4, "UYW": 4,
}

// MinorUnits is the number of decimals of a currency (ISO 4217; 2 when the
// currency is not in the exception table).
func MinorUnits(currency string) int {
	if n, ok := minorUnits[strings.ToUpper(strings.TrimSpace(currency))]; ok {
		return n
	}
	return 2
}

// ErrNotQuantized: an amount has more decimals than its currency.
var ErrNotQuantized = errors.New("payments: amount has more decimals than its currency")

// CheckQuantized verifies a has no more decimals than currency's minor units
// ("10.005" MXN is not money).
func CheckQuantized(a Amount, currency string) error {
	if a.scale() > MinorUnits(currency) {
		return fmt.Errorf("%w: %s %s (max %d decimals)", ErrNotQuantized, a, currency, MinorUnits(currency))
	}
	return nil
}

// ParseMoney parses s and checks it is quantized to currency.
func ParseMoney(s, currency string) (Amount, error) {
	a, err := ParseAmount(s)
	if err != nil {
		return a, err
	}
	if !capability.ValidCurrency(currency) {
		return a, fmt.Errorf("%w: currency %q is not ISO 4217", ErrInvalidAmount, currency)
	}
	return a, CheckQuantized(a, currency)
}

// MinorUnit is the smallest amount of a currency (0.01 MXN, 1 JPY).
func MinorUnit(currency string) Amount {
	n := MinorUnits(currency)
	if n == 0 {
		return MustAmount("1")
	}
	return MustAmount("0." + strings.Repeat("0", n-1) + "1")
}
