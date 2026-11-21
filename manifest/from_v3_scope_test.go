package manifest

import (
	"testing"

	"github.com/asteby/metacore-kernel/manifest/v3"
)

// Model.scope (RFC 0006) reaches the host-side ModelDefinition; an undeclared model projects none.
func TestFromV3ModelScope(t *testing.T) {
	m := &v3.Manifest{Models: []v3.Model{
		{Key: "Order", Table: "orders", Scope: v3.ScopeMerchant, Columns: []v3.Column{{Name: "id", Type: "uuid"}}},
		{Key: "Product", Table: "products", Scope: v3.ScopeMarket, Columns: []v3.Column{{Name: "id", Type: "uuid"}, {Name: "markets", Type: "jsonb"}}},
		{Key: "Plain", Table: "plains", Columns: []v3.Column{{Name: "id", Type: "uuid"}}},
	}}
	defs := FromV3(m).ModelDefinitions
	if defs[0].Scope != "merchant" || defs[1].Scope != "market" {
		t.Fatalf("scope not carried: %q, %q", defs[0].Scope, defs[1].Scope)
	}
	if defs[2].Scope != "" {
		t.Fatalf("an undeclared model must project no scope: %q", defs[2].Scope)
	}
}
