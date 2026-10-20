package money

import (
	"fmt"
	"math/big"
)

// InverseScale is the scale used for derived inverse rates (fx_rates stores
// numeric(28,12)).
const InverseScale int32 = 12

// InverseMode is the rounding mode of derived inverse rates.
const InverseMode = HalfEven

// Rate is an exchange rate: Value units of Quote per 1 unit of Base
// (Base=USD, Quote=MXN, Value=17.2345 means 1 USD = 17.2345 MXN).
type Rate struct {
	ID    string  `json:"id,omitempty"`
	Base  string  `json:"base"`
	Quote string  `json:"quote"`
	Value Decimal `json:"value"`
	Kind  string  `json:"kind,omitempty"` // mid, buy, sell, official, parallel
	// Inverted is true when Value was obtained by inverting a stored rate.
	Inverted bool `json:"inverted,omitempty"`
	// Derived is true when the rate was computed (inverse or cross) instead of
	// read as stored; freeze it with source='derived' if a document uses it.
	Derived bool `json:"derived,omitempty"`
}

func (r Rate) validate() error {
	if r.Base == "" || r.Quote == "" {
		return fmt.Errorf("%w: rate without base or quote", ErrInvalid)
	}
	if r.Value.Sign() <= 0 {
		return fmt.Errorf("%w: rate %s/%s must be > 0, got %s", ErrInvalid, r.Base, r.Quote, r.Value)
	}
	return nil
}

// Inverse returns the Quote→Base rate, 1/Value rounded to scale digits with
// HalfEven (use InverseScale). The result keeps ID and Kind, flips Inverted
// and is marked Derived.
func (r Rate) Inverse(scale int32) (Rate, error) {
	if err := r.validate(); err != nil {
		return Rate{}, err
	}
	v, err := NewDecimal(1, 0).Quo(r.Value, scale, InverseMode)
	if err != nil {
		return Rate{}, err
	}
	if v.Sign() == 0 {
		return Rate{}, fmt.Errorf("%w: inverse of %s rounds to 0 at scale %d", ErrPrecision, r.Value, scale)
	}
	return Rate{
		ID:       r.ID,
		Base:     r.Quote,
		Quote:    r.Base,
		Value:    v,
		Kind:     r.Kind,
		Inverted: !r.Inverted,
		Derived:  true,
	}, nil
}

// Convert converts a (in r.Base) to the currency to (= r.Quote), rounding to
// to.MinorUnits with mode. A currency that does not match the rate returns
// ErrCurrencyMismatch.
func Convert(a Money, r Rate, to Spec, mode RoundingMode) (Money, error) {
	if err := r.validate(); err != nil {
		return Money{}, err
	}
	if err := to.Validate(); err != nil {
		return Money{}, err
	}
	if err := checkMode(mode); err != nil {
		return Money{}, err
	}
	if r.Base != a.spec.Code {
		return Money{}, fmt.Errorf("%w: amount in %s, rate base %s", ErrCurrencyMismatch, a.spec.Code, r.Base)
	}
	if r.Quote != to.Code {
		return Money{}, fmt.Errorf("%w: target %s, rate quote %s", ErrCurrencyMismatch, to.Code, r.Quote)
	}
	// minor_to = minor_a × 10^-muA × value × 10^muTo
	//          = minor_a × coef × 10^muTo / 10^(scale + muA)
	num := new(big.Int).Mul(big.NewInt(a.minor), r.Value.c())
	num.Mul(num, pow10(int64(to.MinorUnits)))
	den := pow10(int64(r.Value.scale) + int64(a.spec.MinorUnits))
	return fromBig(divRound(num, den, mode), to)
}
