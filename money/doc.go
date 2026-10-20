// Package money is an exact, allocation-safe money type for the platform.
//
// A [Money] is an amount in integer minor units (cents, fils, yen...) plus the
// [Spec] of its currency (ISO 4217 code, number of minor units, cash rounding
// increment). It never touches float64: amounts are parsed from decimal
// strings, multiplied by exact [Decimal] factors and rounded with an explicit
// [RoundingMode]. The default mode, [HalfAwayFromZero], reproduces the
// historical math.Round(v*100)/100 used by the addons, without its float
// artefacts (1.005 rounds to 1.01, not 1.00).
//
// Rules:
//
//   - Arithmetic only between the same currency ([ErrCurrencyMismatch]).
//   - Every int64 operation is overflow-checked ([ErrOverflow]).
//   - Parse never drops precision silently ([ErrPrecision]); use [ParseRound]
//     to round on purpose.
//   - [Money.Allocate] splits by largest remainder: the parts always add up to
//     the total, exactly.
//   - Conversion is explicit: [Convert] takes the [Rate] used (QUOTE units per
//     1 BASE unit) and the rounding mode, so documents can store the trio
//     amount / amount in base / rate.
//
// The JSON shape is {"amount":"1899.00","currency_code":"MXN"}, the same
// contract as the Money type of @asteby/storefront and of MoneyValue in
// @asteby/metacore-lib. testdata/golden.json holds the vectors shared with the
// TypeScript implementation (same file, compared by sha256).
//
// The package depends only on the Go standard library on purpose: wasm guests
// can only build with the standard library, so this directory is copied into
// them verbatim (see TestStdlibOnly). Locale formatting lives elsewhere.
package money
