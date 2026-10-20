package money

import (
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"math/bits"
	"sort"
)

// Money is an immutable amount in integer minor units of one currency.
// The zero value has no currency; build values with New, Zero, Parse or
// ParseRound.
type Money struct {
	minor int64
	spec  Spec
}

// New returns minor units of spec (New(189900, MustISO("MXN")) = 1899.00 MXN).
func New(minor int64, spec Spec) Money { return Money{minor: minor, spec: spec} }

// Zero returns 0 in spec.
func Zero(spec Spec) Money { return Money{spec: spec} }

// Parse parses a plain decimal string ("1899.00", "-0.5") in spec. It returns
// ErrPrecision if the value cannot be represented with spec.MinorUnits
// decimals (trailing zeros beyond them are accepted: "10.0" JPY = 10),
// ErrOverflow if it does not fit in int64 minor units and ErrInvalid for
// malformed input.
func Parse(s string, spec Spec) (Money, error) {
	if err := spec.Validate(); err != nil {
		return Money{}, err
	}
	d, err := ParseDecimal(s)
	if err != nil {
		return Money{}, err
	}
	return FromDecimal(d, spec)
}

// ParseRound is Parse that rounds extra decimals with mode instead of
// returning ErrPrecision.
func ParseRound(s string, spec Spec, mode RoundingMode) (Money, error) {
	if err := spec.Validate(); err != nil {
		return Money{}, err
	}
	d, err := ParseDecimal(s)
	if err != nil {
		return Money{}, err
	}
	return FromDecimalRound(d, spec, mode)
}

// FromDecimal converts an exact decimal; see Parse for the errors.
func FromDecimal(d Decimal, spec Spec) (Money, error) {
	if err := spec.Validate(); err != nil {
		return Money{}, err
	}
	r, err := d.Round(int32(spec.MinorUnits), TowardZero)
	if err != nil {
		return Money{}, err
	}
	if r.Cmp(d) != 0 {
		return Money{}, fmt.Errorf("%w: %s has more than %d decimals for %s", ErrPrecision, truncate(d.String()), spec.MinorUnits, spec.Code)
	}
	return fromBig(r.c(), spec)
}

// FromDecimalRound converts d rounding to spec.MinorUnits with mode.
func FromDecimalRound(d Decimal, spec Spec, mode RoundingMode) (Money, error) {
	if err := spec.Validate(); err != nil {
		return Money{}, err
	}
	r, err := d.Round(int32(spec.MinorUnits), mode)
	if err != nil {
		return Money{}, err
	}
	return fromBig(r.c(), spec)
}

func fromBig(minor *big.Int, spec Spec) (Money, error) {
	if !minor.IsInt64() {
		return Money{}, fmt.Errorf("%w: %s minor units of %s", ErrOverflow, truncate(minor.String()), spec.Code)
	}
	return Money{minor: minor.Int64(), spec: spec}, nil
}

// Minor returns the amount in minor units.
func (m Money) Minor() int64 { return m.minor }

// Spec returns the currency spec.
func (m Money) Spec() Spec { return m.spec }

// Currency returns the ISO 4217 code.
func (m Money) Currency() string { return m.spec.Code }

// Amount returns the amount as an exact Decimal with MinorUnits digits.
func (m Money) Amount() Decimal {
	return Decimal{coef: big.NewInt(m.minor), scale: int32(m.spec.MinorUnits)}
}

// WithSpec swaps the cash/scope policy of m (e.g. an organization override)
// keeping the amount. The code and minor units must match.
func (m Money) WithSpec(spec Spec) (Money, error) {
	if err := spec.Validate(); err != nil {
		return Money{}, err
	}
	if !sameCurrency(m.spec, spec) {
		return Money{}, mismatch(m.spec, spec)
	}
	return Money{minor: m.minor, spec: spec}, nil
}

// Sign returns -1, 0 or +1.
func (m Money) Sign() int {
	switch {
	case m.minor < 0:
		return -1
	case m.minor > 0:
		return 1
	}
	return 0
}

// IsZero reports whether the amount is 0.
func (m Money) IsZero() bool { return m.minor == 0 }

func sameCurrency(a, b Spec) bool { return a.Code == b.Code && a.MinorUnits == b.MinorUnits }

func mismatch(a, b Spec) error {
	return fmt.Errorf("%w: %s(%d) vs %s(%d)", ErrCurrencyMismatch, a.Code, a.MinorUnits, b.Code, b.MinorUnits)
}

// Add returns m + o. Both must share currency; overflow is checked.
func (m Money) Add(o Money) (Money, error) {
	if !sameCurrency(m.spec, o.spec) {
		return Money{}, mismatch(m.spec, o.spec)
	}
	s, ok := add64(m.minor, o.minor)
	if !ok {
		return Money{}, fmt.Errorf("%w: %s + %s", ErrOverflow, m, o)
	}
	return Money{minor: s, spec: m.spec}, nil
}

// Sub returns m - o. Both must share currency; overflow is checked.
func (m Money) Sub(o Money) (Money, error) {
	if !sameCurrency(m.spec, o.spec) {
		return Money{}, mismatch(m.spec, o.spec)
	}
	s, ok := sub64(m.minor, o.minor)
	if !ok {
		return Money{}, fmt.Errorf("%w: %s - %s", ErrOverflow, m, o)
	}
	return Money{minor: s, spec: m.spec}, nil
}

// Neg returns -m (ErrOverflow only for the most negative int64).
func (m Money) Neg() (Money, error) {
	if m.minor == math.MinInt64 {
		return Money{}, fmt.Errorf("%w: -(%s)", ErrOverflow, m)
	}
	return Money{minor: -m.minor, spec: m.spec}, nil
}

// Cmp compares two amounts of the same currency: -1, 0 or +1.
func (m Money) Cmp(o Money) (int, error) {
	if !sameCurrency(m.spec, o.spec) {
		return 0, mismatch(m.spec, o.spec)
	}
	switch {
	case m.minor < o.minor:
		return -1, nil
	case m.minor > o.minor:
		return 1, nil
	}
	return 0, nil
}

// add64 adds with overflow detection (two's complement sum via math/bits).
func add64(a, b int64) (int64, bool) {
	u, _ := bits.Add64(uint64(a), uint64(b), 0)
	s := int64(u)
	// Overflow iff both operands have the same sign and the sum's sign differs.
	return s, !((a >= 0) == (b >= 0) && (s >= 0) != (a >= 0))
}

// sub64 subtracts with overflow detection.
func sub64(a, b int64) (int64, bool) {
	u, _ := bits.Sub64(uint64(a), uint64(b), 0)
	s := int64(u)
	// Overflow iff the operands have different signs and the result's sign
	// differs from a.
	return s, !((a >= 0) != (b >= 0) && (s >= 0) != (a >= 0))
}

// MulDecimal returns m × d rounded to minor units with mode
// (10.000 KWD × 0.155 = 1.550 KWD).
func (m Money) MulDecimal(d Decimal, mode RoundingMode) (Money, error) {
	if err := checkMode(mode); err != nil {
		return Money{}, err
	}
	num := new(big.Int).Mul(big.NewInt(m.minor), d.c())
	return fromBig(divRound(num, pow10(int64(d.scale)), mode), m.spec)
}

// Allocate splits m proportionally to weights using the largest remainder
// method: each part gets floor(m×w/Σw) and the leftover minor units go, one
// each, to the parts with the largest remainders (ties to the earliest
// index). The parts always add up to m exactly; a zero weight gets zero.
// Weights must be non-negative and not all zero.
func (m Money) Allocate(weights []int64) ([]Money, error) {
	if len(weights) == 0 {
		return nil, fmt.Errorf("%w: allocate needs at least one weight", ErrInvalid)
	}
	var total uint64
	for _, w := range weights {
		if w < 0 {
			return nil, fmt.Errorf("%w: negative weight %d", ErrInvalid, w)
		}
		var carry uint64
		total, carry = bits.Add64(total, uint64(w), 0)
		if carry != 0 {
			return nil, fmt.Errorf("%w: sum of weights", ErrOverflow)
		}
	}
	if total == 0 {
		return nil, fmt.Errorf("%w: all weights are zero", ErrInvalid)
	}
	abs := uint64(m.minor)
	if m.minor < 0 {
		abs = -abs // two's complement: also right for math.MinInt64 (2^63)
	}
	quotas := make([]uint64, len(weights))
	rems := make([]uint64, len(weights))
	var assigned uint64
	for i, w := range weights {
		// abs×w is a 128-bit product; abs×w/total <= abs fits in 64 bits, so
		// hi < total and Div64 cannot panic.
		hi, lo := bits.Mul64(abs, uint64(w))
		quotas[i], rems[i] = bits.Div64(hi, lo, total)
		assigned += quotas[i]
	}
	order := make([]int, len(weights))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool { return rems[order[a]] > rems[order[b]] })
	for k := uint64(0); k < abs-assigned; k++ {
		quotas[order[k]]++
	}
	parts := make([]Money, len(weights))
	for i, q := range quotas {
		v := int64(q) // q <= abs <= 2^63; 2^63 wraps to MinInt64, negated below
		if m.minor < 0 {
			v = -v
		}
		parts[i] = Money{minor: v, spec: m.spec}
	}
	return parts, nil
}

// RoundCash rounds m to the spec's cash increment with mode (CHF:
// 12.32 → 12.30, 12.33 → 12.35 with HalfAwayFromZero). Without an increment
// (0 or 1) it returns m unchanged.
func (m Money) RoundCash(mode RoundingMode) (Money, error) {
	if err := checkMode(mode); err != nil {
		return Money{}, err
	}
	inc := m.spec.CashIncrementMinor
	if inc <= 1 {
		return m, nil
	}
	bi := big.NewInt(inc)
	q := divRound(big.NewInt(m.minor), bi, mode)
	return fromBig(q.Mul(q, bi), m.spec)
}

// String returns the canonical amount with exactly MinorUnits decimals and no
// currency or grouping: "1899.00", "-0.50", "1000" (JPY), "1.550" (KWD).
func (m Money) String() string {
	return formatScaled(big.NewInt(m.minor), m.spec.MinorUnits)
}

type moneyJSON struct {
	Amount       string `json:"amount"`
	CurrencyCode string `json:"currency_code"`
}

// MarshalJSON encodes {"amount":"1899.00","currency_code":"MXN"}.
func (m Money) MarshalJSON() ([]byte, error) {
	return json.Marshal(moneyJSON{Amount: m.String(), CurrencyCode: m.spec.Code})
}

// UnmarshalJSON decodes {"amount":"1899.00","currency_code":"MXN"}. The
// currency must be known to ISO and the amount must be a JSON string with no
// more decimals than the currency allows (ErrPrecision).
func (m *Money) UnmarshalJSON(b []byte) error {
	var v moneyJSON
	if err := json.Unmarshal(b, &v); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	spec, ok := ISO(v.CurrencyCode)
	if !ok {
		return fmt.Errorf("%w: unknown currency %q", ErrInvalid, v.CurrencyCode)
	}
	p, err := Parse(v.Amount, spec)
	if err != nil {
		return err
	}
	*m = p
	return nil
}
