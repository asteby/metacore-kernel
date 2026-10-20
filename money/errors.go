package money

import "errors"

// Sentinel errors. Functions wrap them with context; test with errors.Is.
var (
	// ErrCurrencyMismatch is returned when two amounts (or an amount and a
	// rate) do not share the same currency.
	ErrCurrencyMismatch = errors.New("money: currency mismatch")
	// ErrOverflow is returned when a result does not fit in int64 minor units.
	ErrOverflow = errors.New("money: overflow")
	// ErrPrecision is returned when an input has more decimals than the
	// currency allows and would have to be rounded.
	ErrPrecision = errors.New("money: precision loss")
	// ErrInvalid is returned for malformed input: bad decimal strings, unknown
	// currencies, invalid specs, weights, rates or rounding modes.
	ErrInvalid = errors.New("money: invalid")
)
