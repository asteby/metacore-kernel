// Package orgconfig gives native (compiled Go) handlers read access to the
// organization config (currency, tax, locale, timezone) through the SAME
// provider that feeds the wasm `ctx_get` scope `org_config`. Money and fiscal
// settings are org config, never a hardcoded regional default: when nothing is
// configured the fields are empty and the caller decides.
package orgconfig

import (
	"context"
	"errors"

	"github.com/google/uuid"
)

// ErrUnavailable is returned when the host did not wire an org-config provider.
var ErrUnavailable = errors.New("orgconfig: provider not wired by the host")

// ErrNoOrg is returned when the caller has no bound organization.
var ErrNoOrg = errors.New("orgconfig: no organization bound")

// Config is the closed, allow-listed slice of the organization (no secrets, no
// billing). Pointers distinguish "unset" from a real zero (tax_rate 0 = exempt).
type Config struct {
	CurrencyCode string   `json:"currency_code"`
	TaxRate      *float64 `json:"tax_rate"`
	TaxIncluded  *bool    `json:"tax_included"`
	Locale       string   `json:"locale"`
	Timezone     string   `json:"timezone"`
}

// Provider resolves the config of exactly the given org. The org always comes
// from the caller's bound context, never from request input.
type Provider func(ctx context.Context, orgID uuid.UUID) (Config, error)

type ctxKey struct{}

// WithProvider returns a context carrying p; the host calls this once per
// request/event/action (typically with wasm.OrgConfigProvider(ctxProviderFn)).
func WithProvider(ctx context.Context, p Provider) context.Context {
	return context.WithValue(ctx, ctxKey{}, p)
}

// Resolve returns the config of orgID using the provider found in ctx.
// Errors: ErrNoOrg (orgID is uuid.Nil), ErrUnavailable (no provider).
func Resolve(ctx context.Context, orgID uuid.UUID) (Config, error) {
	if orgID == uuid.Nil {
		return Config{}, ErrNoOrg
	}
	p, _ := ctx.Value(ctxKey{}).(Provider)
	if p == nil {
		return Config{}, ErrUnavailable
	}
	return p(ctx, orgID)
}
