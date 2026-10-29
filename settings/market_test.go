package settings_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/asteby/metacore-kernel/settings"
)

func TestMarketScopePrecedenceAndSources(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.write(t, settings.ScopeOrg, "", settings.Values{"free_over": 500, "mode": "pro"}, nil)
	f.write(t, settings.ScopeMarket, "us", settings.Values{"free_over": 100}, nil)

	eff, _, err := f.rs.Resolve(ctx, f.org, "acme", settings.Scope{MarketID: "us"})
	if err != nil {
		t.Fatal(err)
	}
	if eff.Values["free_over"] != float64(100) || eff.Sources["free_over"] != settings.SourceMarket {
		t.Fatalf("market override: %v %v", eff.Values["free_over"], eff.Sources["free_over"])
	}
	// Other market, and no market: org value (current behaviour).
	for _, sc := range []settings.Scope{{MarketID: "mx"}, {}} {
		eff, _, _ = f.rs.Resolve(ctx, f.org, "acme", sc)
		if eff.Values["free_over"] != float64(500) || eff.Sources["free_over"] != settings.SourceOrg {
			t.Fatalf("scope %+v: %v %v", sc, eff.Values["free_over"], eff.Sources["free_over"])
		}
	}
	// A market override of a non-market-scoped key is never read.
	if eff.Values["mode"] != "pro" {
		t.Fatalf("mode: %v", eff.Values["mode"])
	}
	// Clearing the override restores the org value.
	f.write(t, settings.ScopeMarket, "us", settings.Values{"free_over": nil}, nil)
	eff, _, _ = f.rs.Resolve(ctx, f.org, "acme", settings.Scope{MarketID: "us"})
	if eff.Values["free_over"] != float64(500) {
		t.Fatalf("after clear: %v", eff.Values["free_over"])
	}
}

func TestMarketWriteRules(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if _, err := f.st.Write(ctx, f.org, "acme", settings.ScopeMarket, "us", settings.Values{"mode": "pro"}, nil, uuid.Nil); !errors.Is(err, settings.ErrNotMarketScoped) {
		t.Fatalf("non market-scoped key: %v", err)
	}
	if _, err := f.st.Write(ctx, f.org, "acme", settings.ScopeBranch, "b1", settings.Values{"free_over": 1}, nil, uuid.Nil); !errors.Is(err, settings.ErrNotBranchScoped) {
		t.Fatalf("market key in branch layer: %v", err)
	}
	if _, err := f.st.Write(ctx, f.org, "acme", settings.ScopeMarket, "", settings.Values{"free_over": 1}, nil, uuid.Nil); !errors.Is(err, settings.ErrInvalidScope) {
		t.Fatalf("market needs id: %v", err)
	}
}

func TestWasmResolverMarket(t *testing.T) {
	f := newFixture(t)
	f.write(t, settings.ScopeOrg, "", settings.Values{"free_over": 500}, nil)
	f.write(t, settings.ScopeMarket, "us", settings.Values{"free_over": 100}, nil)
	w := settings.WasmResolver{R: f.rs}
	ctx := context.Background()
	got, err := w.ResolveSettingsInMarket(ctx, f.org, "acme", "", "us", map[string]string{})
	if err != nil || got["free_over"] != "100" {
		t.Fatalf("market: %v %v", got, err)
	}
	// Legacy entry point = no market.
	got, _ = w.ResolveSettings(ctx, f.org, "acme", "", map[string]string{})
	if got["free_over"] != "500" {
		t.Fatalf("legacy: %v", got)
	}
}
