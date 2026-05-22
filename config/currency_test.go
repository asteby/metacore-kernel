package config_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/asteby/metacore-kernel/config"
)

func TestGetCurrencyOrDefault_NilGetter(t *testing.T) {
	got := config.GetCurrencyOrDefault(context.Background(), nil, uuid.New(), "USD")
	if got != "USD" {
		t.Fatalf("nil getter should return fallback, got %q", got)
	}
}

func TestGetCurrencyOrDefault_GetterResolves(t *testing.T) {
	orgID := uuid.New()
	var seenOrg uuid.UUID
	getter := config.OrgCurrencyGetter(func(_ context.Context, id uuid.UUID) (string, bool) {
		seenOrg = id
		return "MXN", true
	})

	got := config.GetCurrencyOrDefault(context.Background(), getter, orgID, "USD")
	if got != "MXN" {
		t.Fatalf("getter result should win, got %q", got)
	}
	if seenOrg != orgID {
		t.Fatalf("getter received wrong orgID: got %s want %s", seenOrg, orgID)
	}
}

func TestGetCurrencyOrDefault_GetterEmptyString(t *testing.T) {
	getter := config.OrgCurrencyGetter(func(_ context.Context, _ uuid.UUID) (string, bool) {
		return "", true
	})

	got := config.GetCurrencyOrDefault(context.Background(), getter, uuid.New(), "USD")
	if got != "USD" {
		t.Fatalf("empty string from getter should fall back, got %q", got)
	}
}

func TestGetCurrencyOrDefault_GetterReportsMiss(t *testing.T) {
	getter := config.OrgCurrencyGetter(func(_ context.Context, _ uuid.UUID) (string, bool) {
		return "EUR", false // value is present but ok=false → ignored
	})

	got := config.GetCurrencyOrDefault(context.Background(), getter, uuid.New(), "USD")
	if got != "USD" {
		t.Fatalf("ok=false should ignore the value, got %q", got)
	}
}

func TestCurrencyGetterFromContext_Empty(t *testing.T) {
	if g := config.CurrencyGetterFromContext(context.Background()); g != nil {
		t.Fatalf("expected nil getter on bare context, got %T", g)
	}
}

func TestCurrencyGetterFromContext_RoundTrip(t *testing.T) {
	getter := config.OrgCurrencyGetter(func(_ context.Context, _ uuid.UUID) (string, bool) {
		return "GBP", true
	})

	ctx := config.WithCurrencyGetter(context.Background(), getter)
	out := config.CurrencyGetterFromContext(ctx)
	if out == nil {
		t.Fatal("expected getter to be present after WithCurrencyGetter")
	}

	cur, ok := out(ctx, uuid.New())
	if !ok || cur != "GBP" {
		t.Fatalf("round-tripped getter misbehaved: got (%q, %v)", cur, ok)
	}
}

func TestCurrencyGetterFromContext_IsolationFromUnrelatedValue(t *testing.T) {
	// Make sure our private context key cannot be poisoned by an unrelated
	// caller storing a value under a same-named string key.
	ctx := context.WithValue(context.Background(), "currencyGetterKey", "not-a-getter") //nolint:staticcheck // intentional cross-type test
	if g := config.CurrencyGetterFromContext(ctx); g != nil {
		t.Fatalf("foreign key should not leak as getter, got %T", g)
	}
}
