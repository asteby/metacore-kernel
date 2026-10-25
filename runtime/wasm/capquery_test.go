package wasm

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/asteby/metacore-kernel/capability"
	"github.com/asteby/metacore-kernel/manifest"
	"github.com/asteby/metacore-kernel/security"
	"github.com/google/uuid"
)

type fakeQuerier struct {
	got capability.Query
	res capability.QueryResult
	err error
}

func (f *fakeQuerier) Query(_ context.Context, q capability.Query) (capability.QueryResult, error) {
	f.got = q
	return f.res, f.err
}

func capEnv(t *testing.T, b []byte) (bool, map[string]any) {
	t.Helper()
	var e struct {
		Success bool           `json:"success"`
		Data    map[string]any `json:"data"`
		Error   map[string]any `json:"error"`
	}
	if err := json.Unmarshal(b, &e); err != nil {
		t.Fatal(err)
	}
	if e.Success {
		return true, e.Data
	}
	return false, e.Error
}

func TestCapabilityQueryGate(t *testing.T) {
	org := uuid.New()
	q := &fakeQuerier{res: capability.QueryResult{Capability: "commerce.shipping.rates", Market: "mx"}}
	inv := &invocation{
		addonKey:   "storefront_cart",
		orgID:      org,
		caps:       security.Compile("storefront_cart", []manifest.Capability{{Kind: "capability:query", Target: "commerce.shipping.*"}}),
		capQuerier: q,
	}
	ctx := context.Background()

	ok, d := capEnv(t, executeCapabilityQuery(ctx, inv, []byte(`{"capability":"commerce.shipping.rates","market":"mx","caller":"evil","input":{"a":1}}`)))
	if !ok || d["capability"] != "commerce.shipping.rates" {
		t.Fatalf("allowed query failed: %v", d)
	}
	// Caller is the invoking addon, never what the guest wrote.
	if q.got.Org != org || q.got.Market.Code != "mx" || q.got.Capability != "commerce.shipping.rates" || q.got.Caller != "storefront_cart" {
		t.Fatalf("querier got %+v", q.got)
	}

	ok, e := capEnv(t, executeCapabilityQuery(ctx, inv, []byte(`{"capability":"commerce.payments.session"}`)))
	if ok || e["code"] != "forbidden" {
		t.Fatalf("undeclared capability must be forbidden: %v", e)
	}

	inv.capQuerier = nil
	ok, e = capEnv(t, executeCapabilityQuery(ctx, inv, []byte(`{"capability":"commerce.shipping.rates"}`)))
	if ok || e["code"] != "capability_unavailable" {
		t.Fatalf("no querier: %v", e)
	}

	inv.capQuerier = &fakeQuerier{err: capability.ErrNoProvider}
	ok, e = capEnv(t, executeCapabilityQuery(ctx, inv, []byte(`{"capability":"commerce.shipping.rates"}`)))
	if ok || e["code"] != "no_provider" {
		t.Fatalf("no provider: %v", e)
	}

	deep := context.WithValue(ctx, capQueryDepthKey{}, maxCapQueryDepth)
	ok, e = capEnv(t, executeCapabilityQuery(deep, inv, []byte(`{"capability":"commerce.shipping.rates"}`)))
	if ok || e["code"] != "capability_depth" {
		t.Fatalf("depth: %v", e)
	}
}

type fakeLister struct {
	fakeQuerier
	listing capability.ProviderListing
	err     error
	got     capability.Query
}

func (f *fakeLister) ListProviders(_ context.Context, q capability.Query) (capability.ProviderListing, error) {
	f.got = q
	return f.listing, f.err
}

func TestCapabilityProvidersGateAndCodes(t *testing.T) {
	org := uuid.New()
	l := &fakeLister{listing: capability.ProviderListing{Capability: "commerce.payments.session", Market: "mx", Resolution: "ok",
		Providers: []capability.ProviderView{{Addon: "mp", Status: capability.StatusReady, Serves: true, Reached: true}}}}
	inv := &invocation{
		addonKey:   "storefront_cart",
		orgID:      org,
		caps:       security.Compile("storefront_cart", []manifest.Capability{{Kind: "capability:query", Target: "commerce.payments.*"}}),
		capQuerier: l,
	}
	ctx := context.Background()
	ok, d := capEnv(t, executeCapabilityProviders(ctx, inv, []byte(`{"capability":"commerce.payments.session","market":"mx"}`)))
	if !ok || d["resolution"] != "ok" || l.got.Org != org || l.got.Market.Code != "mx" || l.got.Caller != "storefront_cart" {
		t.Fatalf("listing failed: %v %+v", d, l.got)
	}
	if ok, e := capEnv(t, executeCapabilityProviders(ctx, inv, []byte(`{"capability":"commerce.shipping.rates"}`))); ok || e["code"] != "forbidden" {
		t.Fatalf("undeclared capability must be forbidden: %v", e)
	}
	inv.capQuerier = &fakeQuerier{}
	if ok, e := capEnv(t, executeCapabilityProviders(ctx, inv, []byte(`{"capability":"commerce.payments.session"}`))); ok || e["code"] != "capability_unavailable" {
		t.Fatalf("querier without lister: %v", e)
	}
	inv.capQuerier = nil
	if ok, e := capEnv(t, executeCapabilityProviders(ctx, inv, []byte(`{"capability":"commerce.payments.session"}`))); ok || e["code"] != "capability_unavailable" {
		t.Fatalf("nil querier: %v", e)
	}
}

func TestCapQueryErrorCodeNotConfigured(t *testing.T) {
	if got := capQueryErrorCode(capability.ErrNotConfigured); got != "not_configured" {
		t.Fatalf("got %q", got)
	}
	if got := capQueryErrorCode(capability.ErrAmbiguousProvider); got != "ambiguous_provider" {
		t.Fatalf("got %q", got)
	}
}
