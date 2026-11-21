package dec

import (
	"github.com/asteby/metacore-kernel/money"
)

// AddChecked is Add that reports overflow instead of wrapping silently. Ledger
// code that accumulates balances should prefer it over Add.
func (d D) AddChecked(o D) (D, bool) {
	s := d + o
	if (o > 0 && s < d) || (o < 0 && s > d) {
		return 0, false
	}
	return s, true
}

// FromMoney converts a money.Decimal to D. It fails with ErrRange when the
// value has more than Places significant decimals or does not fit in an int64
// at that scale, so a conversion never rounds silently.
func FromMoney(m money.Decimal) (D, error) {
	return Parse(m.String())
}

// ToMoney converts D to a money.Decimal (exact).
func (d D) ToMoney() money.Decimal {
	return money.MustDecimal(d.Format(Places))
}
