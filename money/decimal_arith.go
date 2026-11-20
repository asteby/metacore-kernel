package money

import (
	"database/sql/driver"
	"fmt"
	"math/big"
	"strconv"
)

// Add returns the exact sum d + o (scale = max of both).
func (d Decimal) Add(o Decimal) Decimal {
	a, b, s := align(d, o)
	return Decimal{coef: new(big.Int).Add(a, b), scale: s}
}

// Sub returns the exact difference d - o (scale = max of both).
func (d Decimal) Sub(o Decimal) Decimal {
	a, b, s := align(d, o)
	return Decimal{coef: new(big.Int).Sub(a, b), scale: s}
}

// CeilTo rounds d UP (toward positive infinity) to the next multiple of m, the
// usual "round the price up to a multiple of 5" rule. m must be > 0.
func (d Decimal) CeilTo(m Decimal) (Decimal, error) {
	if m.Sign() <= 0 {
		return Decimal{}, fmt.Errorf("%w: CeilTo multiple must be > 0", ErrInvalid)
	}
	a, b, s := align(d, m)
	q, r := new(big.Int).DivMod(a, b, new(big.Int)) // Euclidean: r >= 0, so q is the floor for b > 0
	if r.Sign() > 0 {
		q.Add(q, big.NewInt(1))
	}
	return Decimal{coef: q.Mul(q, b), scale: s}, nil
}

func align(d, o Decimal) (a, b *big.Int, scale int32) {
	a, b = d.c(), o.c()
	switch {
	case d.scale < o.scale:
		a = new(big.Int).Mul(a, pow10(int64(o.scale-d.scale)))
		return a, b, o.scale
	case d.scale > o.scale:
		b = new(big.Int).Mul(b, pow10(int64(d.scale-o.scale)))
	}
	return a, b, d.scale
}

// Value implements driver.Valuer: the plain decimal text (Postgres casts it to
// numeric; no float ever touches the value).
func (d Decimal) Value() (driver.Value, error) { return d.String(), nil }

// Scan implements sql.Scanner for numeric columns (text, bytes, ints, floats;
// a float is read through its shortest decimal form, so prefer text drivers).
func (d *Decimal) Scan(src any) error {
	var s string
	switch v := src.(type) {
	case nil:
		*d = Decimal{}
		return nil
	case string:
		s = v
	case []byte:
		s = string(v)
	case int64:
		*d = NewDecimal(v, 0)
		return nil
	case float64:
		s = strconv.FormatFloat(v, 'f', -1, 64)
	default:
		return fmt.Errorf("%w: cannot scan %T into Decimal", ErrInvalid, src)
	}
	p, err := ParseDecimal(s)
	if err != nil {
		return err
	}
	*d = p
	return nil
}
