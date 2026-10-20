package money

import (
	"fmt"
	"math/big"
	"strings"
)

// MaxDecimalLen bounds the length of the strings ParseDecimal accepts, so a
// hostile input cannot make it allocate arbitrarily large numbers.
const MaxDecimalLen = 256

// maxScale bounds the scale passed to Quo and Round.
const maxScale = 64

// Decimal is an exact, immutable decimal number: coef × 10^-scale with
// scale >= 0. The zero value is 0. It is used for factors (tax rates,
// quantities, exchange rates), not for amounts: amounts are [Money].
type Decimal struct {
	coef  *big.Int
	scale int32
}

// NewDecimal returns coef × 10^-scale. A negative scale multiplies coef.
func NewDecimal(coef int64, scale int32) Decimal {
	c := big.NewInt(coef)
	if scale < 0 {
		c.Mul(c, pow10(int64(-scale)))
		scale = 0
	}
	return Decimal{coef: c, scale: scale}
}

// ParseDecimal parses a plain decimal string: an optional single sign,
// at least one digit, and optionally a dot followed by at least one digit
// ("-12.50", "+3", "0.155"). Exponents ("1e3"), thousands separators
// ("1,000"), whitespace, NaN/Inf, double signs and bare dots (".5", "5.") are
// rejected with ErrInvalid.
func ParseDecimal(s string) (Decimal, error) {
	if len(s) == 0 || len(s) > MaxDecimalLen {
		return Decimal{}, fmt.Errorf("%w: decimal %q: empty or too long", ErrInvalid, truncate(s))
	}
	i := 0
	neg := false
	if s[0] == '+' || s[0] == '-' {
		neg = s[0] == '-'
		i = 1
	}
	intStart := i
	for i < len(s) && isDigit(s[i]) {
		i++
	}
	intDigits := s[intStart:i]
	fracDigits := ""
	if i < len(s) && s[i] == '.' {
		i++
		fracStart := i
		for i < len(s) && isDigit(s[i]) {
			i++
		}
		fracDigits = s[fracStart:i]
		if fracDigits == "" {
			return Decimal{}, fmt.Errorf("%w: decimal %q: no digits after the dot", ErrInvalid, truncate(s))
		}
	}
	if intDigits == "" || i != len(s) {
		return Decimal{}, fmt.Errorf("%w: decimal %q: not a plain decimal", ErrInvalid, truncate(s))
	}
	c, ok := new(big.Int).SetString(intDigits+fracDigits, 10)
	if !ok {
		return Decimal{}, fmt.Errorf("%w: decimal %q", ErrInvalid, truncate(s))
	}
	if neg {
		c.Neg(c)
	}
	return Decimal{coef: c, scale: int32(len(fracDigits))}, nil
}

// MustDecimal is ParseDecimal that panics; for constants and tests.
func MustDecimal(s string) Decimal {
	d, err := ParseDecimal(s)
	if err != nil {
		panic(err)
	}
	return d
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

func truncate(s string) string {
	if len(s) > 32 {
		return s[:32] + "…"
	}
	return s
}

func (d Decimal) c() *big.Int {
	if d.coef == nil {
		return new(big.Int)
	}
	return d.coef
}

// Coef returns a copy of the unscaled coefficient.
func (d Decimal) Coef() *big.Int { return new(big.Int).Set(d.c()) }

// Scale returns the number of decimal digits.
func (d Decimal) Scale() int32 { return d.scale }

// Sign returns -1, 0 or +1.
func (d Decimal) Sign() int { return d.c().Sign() }

// IsZero reports whether d == 0.
func (d Decimal) IsZero() bool { return d.Sign() == 0 }

// Neg returns -d.
func (d Decimal) Neg() Decimal {
	return Decimal{coef: new(big.Int).Neg(d.c()), scale: d.scale}
}

// Cmp compares numerically: -1 if d < o, 0 if equal, +1 if d > o.
func (d Decimal) Cmp(o Decimal) int {
	a, b := d.c(), o.c()
	switch {
	case d.scale < o.scale:
		a = new(big.Int).Mul(a, pow10(int64(o.scale-d.scale)))
	case d.scale > o.scale:
		b = new(big.Int).Mul(b, pow10(int64(d.scale-o.scale)))
	}
	return a.Cmp(b)
}

// Mul returns the exact product d × o (scale = d.scale + o.scale).
func (d Decimal) Mul(o Decimal) Decimal {
	return Decimal{coef: new(big.Int).Mul(d.c(), o.c()), scale: d.scale + o.scale}
}

// Quo returns d / o rounded to scale digits with mode. Division by zero, a
// negative or excessive scale or an unknown mode return ErrInvalid.
func (d Decimal) Quo(o Decimal, scale int32, mode RoundingMode) (Decimal, error) {
	if o.IsZero() {
		return Decimal{}, fmt.Errorf("%w: division by zero", ErrInvalid)
	}
	if scale < 0 || scale > maxScale {
		return Decimal{}, fmt.Errorf("%w: scale %d out of range", ErrInvalid, scale)
	}
	if err := checkMode(mode); err != nil {
		return Decimal{}, err
	}
	// d/o = (dc/10^ds) / (oc/10^os); result coef at scale s:
	// round(dc × 10^(os+s) / (oc × 10^ds)).
	num := new(big.Int).Mul(d.c(), pow10(int64(o.scale)+int64(scale)))
	den := new(big.Int).Mul(o.c(), pow10(int64(d.scale)))
	return Decimal{coef: divRound(num, den, mode), scale: scale}, nil
}

// Round returns d with exactly scale digits, rounding with mode when digits
// are dropped.
func (d Decimal) Round(scale int32, mode RoundingMode) (Decimal, error) {
	if scale < 0 || scale > maxScale {
		return Decimal{}, fmt.Errorf("%w: scale %d out of range", ErrInvalid, scale)
	}
	if err := checkMode(mode); err != nil {
		return Decimal{}, err
	}
	if scale >= d.scale {
		return Decimal{coef: new(big.Int).Mul(d.c(), pow10(int64(scale-d.scale))), scale: scale}, nil
	}
	return Decimal{coef: divRound(d.c(), pow10(int64(d.scale-scale)), mode), scale: scale}, nil
}

// String returns the plain decimal representation with exactly Scale digits
// after the dot ("17.2345", "-0.50", "3"). Zero is never signed.
func (d Decimal) String() string {
	return formatScaled(d.c(), int(d.scale))
}

func formatScaled(coef *big.Int, scale int) string {
	digits := new(big.Int).Abs(coef).String()
	if scale > 0 {
		if len(digits) <= scale {
			digits = strings.Repeat("0", scale-len(digits)+1) + digits
		}
		digits = digits[:len(digits)-scale] + "." + digits[len(digits)-scale:]
	}
	if coef.Sign() < 0 {
		return "-" + digits
	}
	return digits
}

// MarshalText implements encoding.TextMarshaler (JSON string).
func (d Decimal) MarshalText() ([]byte, error) { return []byte(d.String()), nil }

// UnmarshalText implements encoding.TextUnmarshaler.
func (d *Decimal) UnmarshalText(b []byte) error {
	v, err := ParseDecimal(string(b))
	if err != nil {
		return err
	}
	*d = v
	return nil
}
