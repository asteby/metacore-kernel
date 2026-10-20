package money

import (
	"fmt"
	"math/big"
)

// RoundingMode decides what happens to the discarded digits of a result.
// The zero value is HalfAwayFromZero, the platform default.
type RoundingMode uint8

const (
	// HalfAwayFromZero rounds to nearest, ties away from zero (math.Round):
	// 1.005 → 1.01, -0.5 → -1. Platform default.
	HalfAwayFromZero RoundingMode = iota
	// HalfEven rounds to nearest, ties to the even neighbour (banker's):
	// 2.5 → 2, 3.5 → 4.
	HalfEven
	// TowardZero truncates.
	TowardZero
	// AwayFromZero rounds any discarded non-zero digit up in magnitude.
	AwayFromZero
	// Floor rounds toward negative infinity.
	Floor
	// Ceiling rounds toward positive infinity.
	Ceiling
)

var modeNames = [...]string{
	HalfAwayFromZero: "halfAwayFromZero",
	HalfEven:         "halfEven",
	TowardZero:       "towardZero",
	AwayFromZero:     "awayFromZero",
	Floor:            "floor",
	Ceiling:          "ceil",
}

// String returns the wire name of the mode, shared with the TypeScript SDK
// ("halfAwayFromZero", "halfEven", "towardZero", "awayFromZero", "floor",
// "ceil").
func (m RoundingMode) String() string {
	if m.valid() {
		return modeNames[m]
	}
	return fmt.Sprintf("RoundingMode(%d)", uint8(m))
}

func (m RoundingMode) valid() bool { return int(m) < len(modeNames) }

// ParseRoundingMode parses a wire name returned by RoundingMode.String.
func ParseRoundingMode(s string) (RoundingMode, error) {
	for i, n := range modeNames {
		if n == s {
			return RoundingMode(i), nil
		}
	}
	return 0, fmt.Errorf("%w: unknown rounding mode %q", ErrInvalid, s)
}

// MarshalText implements encoding.TextMarshaler.
func (m RoundingMode) MarshalText() ([]byte, error) {
	if !m.valid() {
		return nil, fmt.Errorf("%w: unknown rounding mode %d", ErrInvalid, uint8(m))
	}
	return []byte(modeNames[m]), nil
}

// UnmarshalText implements encoding.TextUnmarshaler.
func (m *RoundingMode) UnmarshalText(b []byte) error {
	v, err := ParseRoundingMode(string(b))
	if err != nil {
		return err
	}
	*m = v
	return nil
}

func checkMode(m RoundingMode) error {
	if !m.valid() {
		return fmt.Errorf("%w: unknown rounding mode %d", ErrInvalid, uint8(m))
	}
	return nil
}

// divRound returns num/den rounded to an integer with mode. den must be
// non-zero and mode valid.
func divRound(num, den *big.Int, mode RoundingMode) *big.Int {
	q, r := new(big.Int).QuoRem(num, den, new(big.Int))
	if r.Sign() == 0 {
		return q
	}
	sign := num.Sign() * den.Sign()
	var up bool
	switch mode {
	case TowardZero:
		up = false
	case AwayFromZero:
		up = true
	case Floor:
		up = sign < 0
	case Ceiling:
		up = sign > 0
	default: // HalfAwayFromZero, HalfEven
		r2 := new(big.Int).Abs(r)
		r2.Lsh(r2, 1)
		c := r2.Cmp(new(big.Int).Abs(den))
		up = c > 0 || (c == 0 && (mode == HalfAwayFromZero || q.Bit(0) == 1))
	}
	if up {
		q.Add(q, big.NewInt(int64(sign)))
	}
	return q
}

var pow10Cache = func() [40]*big.Int {
	var c [40]*big.Int
	c[0] = big.NewInt(1)
	ten := big.NewInt(10)
	for i := 1; i < len(c); i++ {
		c[i] = new(big.Int).Mul(c[i-1], ten)
	}
	return c
}()

// pow10 returns 10^n as a fresh big.Int (n >= 0).
func pow10(n int64) *big.Int {
	if n < int64(len(pow10Cache)) {
		return new(big.Int).Set(pow10Cache[n])
	}
	return new(big.Int).Exp(big.NewInt(10), big.NewInt(n), nil)
}
