package money

import (
	"fmt"
	"sort"
	"strings"
)

// Scope says where cash/tax rounding happens: on each line or on the total.
type Scope string

const (
	// ScopeLine rounds every line, then adds (default).
	ScopeLine Scope = "line"
	// ScopeTotal adds unrounded lines, then rounds once.
	ScopeTotal Scope = "total"
)

// MaxMinorUnits is the largest number of minor units a Spec may declare.
const MaxMinorUnits = 18

// Spec describes a currency for arithmetic: its code, how many minor units
// (decimals) it has, the cash rounding increment in minor units (0 or 1 =
// no cash rounding; CHF = 5, i.e. 0.05) and the rounding scope. Organizations
// may override CashIncrementMinor and Scope; MinorUnits comes from ISO 4217.
type Spec struct {
	Code               string `json:"code"`
	MinorUnits         int    `json:"minor_units"`
	CashIncrementMinor int64  `json:"cash_increment_minor,omitempty"`
	Scope              Scope  `json:"scope,omitempty"`
}

// Validate checks the spec is usable.
func (s Spec) Validate() error {
	if s.Code == "" {
		return fmt.Errorf("%w: empty currency code", ErrInvalid)
	}
	if s.MinorUnits < 0 || s.MinorUnits > MaxMinorUnits {
		return fmt.Errorf("%w: %s minor units %d out of range", ErrInvalid, s.Code, s.MinorUnits)
	}
	if s.CashIncrementMinor < 0 {
		return fmt.Errorf("%w: %s negative cash increment", ErrInvalid, s.Code)
	}
	if s.Scope != "" && s.Scope != ScopeLine && s.Scope != ScopeTotal {
		return fmt.Errorf("%w: %s unknown rounding scope %q", ErrInvalid, s.Code, s.Scope)
	}
	return nil
}

// ISO returns the ISO 4217 spec of an active currency (case-insensitive).
// Fund and metal codes without minor units (XAU, XDR...) are not listed.
func ISO(code string) (Spec, bool) {
	c := strings.ToUpper(strings.TrimSpace(code))
	mu, ok := isoMinorUnits[c]
	if !ok {
		return Spec{}, false
	}
	return Spec{Code: c, MinorUnits: mu, CashIncrementMinor: cashIncrements[c], Scope: ScopeLine}, true
}

// MustISO is ISO that panics on an unknown code; for constants and tests.
func MustISO(code string) Spec {
	s, ok := ISO(code)
	if !ok {
		panic(fmt.Sprintf("money: unknown ISO 4217 currency %q", code))
	}
	return s
}

// ISOCodes returns every code ISO knows, sorted.
func ISOCodes() []string {
	out := make([]string, 0, len(isoMinorUnits))
	for c := range isoMinorUnits {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

// cashIncrements holds the default cash rounding increment, in minor units,
// for currencies whose smallest coin is larger than one minor unit (CLDR
// supplemental currency data). Organizations can override it.
var cashIncrements = map[string]int64{
	"CHF": 5,   // 0.05
	"CAD": 5,   // 0.05 (no pennies since 2013)
	"DKK": 50,  // 0.50
	"SEK": 100, // whole kronor
	"NOK": 100, // whole kroner
	"CZK": 100, // whole koruna
	"HUF": 100, // whole forint
}

// isoMinorUnits is ISO 4217 list one (active codes) with their minor units.
// Keep it identical to the "iso" section of testdata/golden.json and to the
// table in @asteby/metacore-lib (money.ts); TestISOTableMatchesGolden checks it.
var isoMinorUnits = map[string]int{
	"AED": 2, "AFN": 2, "ALL": 2, "AMD": 2, "ANG": 2, "AOA": 2, "ARS": 2, "AUD": 2,
	"AWG": 2, "AZN": 2, "BAM": 2, "BBD": 2, "BDT": 2, "BGN": 2, "BHD": 3, "BIF": 0,
	"BMD": 2, "BND": 2, "BOB": 2, "BOV": 2, "BRL": 2, "BSD": 2, "BTN": 2, "BWP": 2,
	"BYN": 2, "BZD": 2, "CAD": 2, "CDF": 2, "CHE": 2, "CHF": 2, "CHW": 2, "CLF": 4,
	"CLP": 0, "CNY": 2, "COP": 2, "COU": 2, "CRC": 2, "CUC": 2, "CUP": 2, "CVE": 2,
	"CZK": 2, "DJF": 0, "DKK": 2, "DOP": 2, "DZD": 2, "EGP": 2, "ERN": 2, "ETB": 2,
	"EUR": 2, "FJD": 2, "FKP": 2, "GBP": 2, "GEL": 2, "GHS": 2, "GIP": 2, "GMD": 2,
	"GNF": 0, "GTQ": 2, "GYD": 2, "HKD": 2, "HNL": 2, "HTG": 2, "HUF": 2, "IDR": 2,
	"ILS": 2, "INR": 2, "IQD": 3, "IRR": 2, "ISK": 0, "JMD": 2, "JOD": 3, "JPY": 0,
	"KES": 2, "KGS": 2, "KHR": 2, "KMF": 0, "KPW": 2, "KRW": 0, "KWD": 3, "KYD": 2,
	"KZT": 2, "LAK": 2, "LBP": 2, "LKR": 2, "LRD": 2, "LSL": 2, "LYD": 3, "MAD": 2,
	"MDL": 2, "MGA": 2, "MKD": 2, "MMK": 2, "MNT": 2, "MOP": 2, "MRU": 2, "MUR": 2,
	"MVR": 2, "MWK": 2, "MXN": 2, "MXV": 2, "MYR": 2, "MZN": 2, "NAD": 2, "NGN": 2,
	"NIO": 2, "NOK": 2, "NPR": 2, "NZD": 2, "OMR": 3, "PAB": 2, "PEN": 2, "PGK": 2,
	"PHP": 2, "PKR": 2, "PLN": 2, "PYG": 0, "QAR": 2, "RON": 2, "RSD": 2, "RUB": 2,
	"RWF": 0, "SAR": 2, "SBD": 2, "SCR": 2, "SDG": 2, "SEK": 2, "SGD": 2, "SHP": 2,
	"SLE": 2, "SLL": 2, "SOS": 2, "SRD": 2, "SSP": 2, "STN": 2, "SVC": 2, "SYP": 2,
	"SZL": 2, "THB": 2, "TJS": 2, "TMT": 2, "TND": 3, "TOP": 2, "TRY": 2, "TTD": 2,
	"TWD": 2, "TZS": 2, "UAH": 2, "UGX": 0, "USD": 2, "USN": 2, "UYI": 0, "UYU": 2,
	"UYW": 4, "UZS": 2, "VED": 2, "VES": 2, "VND": 0, "VUV": 0, "WST": 2, "XAF": 0,
	"XCD": 2, "XCG": 2, "XOF": 0, "XPF": 0, "YER": 2, "ZAR": 2, "ZMW": 2, "ZWG": 2,
	"ZWL": 2,
}
