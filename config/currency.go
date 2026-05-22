// Package config exposes thin, transport-agnostic helpers that addons use to
// read per-organization configuration (currency, locale, fiscal identifiers,
// etc.) without hardcoding values like "MXN".
//
// Apps (Ops, Pitsline, doctores.lat) wire the actual implementations — they
// know how to look up the organization row (DB, cache, in-memory tenant
// registry). Addons (inventory, customers, billing-sat, accounting-lite) only
// depend on the helper interfaces declared here.
package config

import (
	"context"

	"github.com/google/uuid"
)

// OrgCurrencyGetter resolves the ISO-4217 currency code for an organization
// at request time. Apps register a concrete implementation; addons consume
// it via GetCurrencyOrDefault so they never hardcode a default.
//
// Returns ("MXN", true), ("USD", true), …, or ("", false) when the org is
// unknown or unconfigured. Implementations MUST NOT panic — return ("", false)
// on lookup errors and let the caller decide on a fallback.
type OrgCurrencyGetter func(ctx context.Context, orgID uuid.UUID) (string, bool)

// GetCurrencyOrDefault returns the organization's currency or the provided
// fallback when the getter is nil or could not resolve a value.
//
//	getter := config.CurrencyGetterFromContext(ctx)
//	cur := config.GetCurrencyOrDefault(ctx, getter, orgID, "USD")
func GetCurrencyOrDefault(ctx context.Context, getter OrgCurrencyGetter, orgID uuid.UUID, fallback string) string {
	if getter == nil {
		return fallback
	}
	if cur, ok := getter(ctx, orgID); ok && cur != "" {
		return cur
	}
	return fallback
}

// --- Context plumbing -------------------------------------------------------

type ctxKey int

const (
	currencyGetterKey ctxKey = iota
)

// WithCurrencyGetter stores the getter on the context so callers deep in the
// stack (e.g. GORM BeforeCreate hooks on addon models) can reach it without
// having to thread an extra parameter through every function signature.
func WithCurrencyGetter(ctx context.Context, g OrgCurrencyGetter) context.Context {
	return context.WithValue(ctx, currencyGetterKey, g)
}

// CurrencyGetterFromContext extracts the previously-wired getter or nil when
// none has been installed. Callers should pass the result to
// GetCurrencyOrDefault, which already handles the nil case.
func CurrencyGetterFromContext(ctx context.Context) OrgCurrencyGetter {
	if v, ok := ctx.Value(currencyGetterKey).(OrgCurrencyGetter); ok {
		return v
	}
	return nil
}
