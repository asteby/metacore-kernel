package wasm

import (
	"context"
	"testing"

	"github.com/asteby/metacore-kernel/connectors"
	"github.com/asteby/metacore-kernel/manifest"
	"github.com/asteby/metacore-kernel/security"
	"github.com/google/uuid"
)

type connHostStore map[uuid.UUID]map[string]string

func (s connHostStore) Get(_ context.Context, org uuid.UUID, key string) (map[string]string, bool, error) {
	if key != "woocommerce" {
		return nil, false, nil
	}
	c, ok := s[org]
	return c, ok, nil
}

// QA-EXP-ROLES / EXP-ROL-16: el host de http:fetch sale del store_url de la
// org que invoca, nunca del de otra org.
func TestConnectorFetchHostsPerOrg(t *testing.T) {
	orgA, orgB := uuid.New(), uuid.New()
	store := connHostStore{
		orgA: {"store_url": "https://tienda-a.example.com"},
		orgB: {"store_url": "tienda-b.example.org"},
	}
	caps := security.Compile("integration_woocommerce", []manifest.Capability{
		{Kind: "http:fetch", Target: "connector:woocommerce.store_url"},
		{Kind: "connector:read", Target: "woocommerce"},
	})
	res := connectors.NewResolver(store)
	ctx := context.Background()

	invA := &invocation{caps: caps, orgID: orgA, connectors: res}
	hostsA := connectorFetchHosts(ctx, invA)
	if err := caps.CanFetchHosts("https://tienda-a.example.com/wp-json/wc/v3/orders", hostsA); err != nil {
		t.Fatalf("org A → su tienda: %v", err)
	}
	if err := caps.CanFetchHosts("https://tienda-b.example.org/wp-json/wc/v3/orders", hostsA); err == nil {
		t.Fatal("org A no debe poder llamar a la tienda de org B")
	}

	// Sin connector:read el grant no resuelve.
	noRead := security.Compile("x", []manifest.Capability{{Kind: "http:fetch", Target: "connector:woocommerce.store_url"}})
	if h := connectorFetchHosts(ctx, &invocation{caps: noRead, orgID: orgA, connectors: res}); len(h) != 0 {
		t.Fatalf("sin connector:read: hosts=%v", h)
	}
	// Org sin conector configurado / sin org / sin resolver → nada.
	if h := connectorFetchHosts(ctx, &invocation{caps: caps, orgID: uuid.New(), connectors: res}); len(h) != 0 {
		t.Fatalf("org sin conector: %v", h)
	}
	if h := connectorFetchHosts(ctx, &invocation{caps: caps, connectors: res}); len(h) != 0 {
		t.Fatalf("sin org: %v", h)
	}
	if h := connectorFetchHosts(ctx, &invocation{caps: caps, orgID: orgA}); len(h) != 0 {
		t.Fatalf("sin resolver: %v", h)
	}
}
