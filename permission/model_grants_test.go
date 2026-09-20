package permission

import (
	"testing"

	v3 "github.com/asteby/metacore-kernel/manifest/v3"
)

func posGrants() *ModelGrants {
	g := NewModelGrants()
	g.Register("pos", []v3.PermissionDef{
		{Key: "pos.sale.read", Models: []v3.PermissionModel{{Model: "customers.SalesOrder", Actions: []string{"index", "show"}}}},
		{Key: "refund_claims.settle", Models: []v3.PermissionModel{{Model: "RefundClaim", Actions: []string{"settle"}}}},
		{Key: "pos.unmapped.read"},
	})
	return g
}

func TestModelGrantsListsButDoesNotCreate(t *testing.T) {
	g := posGrants()
	held := []string{"pos.sale.read"}
	if _, ok := g.Allows(held, []string{"index"}, "SalesOrder", "sales_orders"); !ok {
		t.Fatal("pos.sale.read must list SalesOrder (cross-addon, via table alias)")
	}
	if _, ok := g.Allows(held, []string{"create"}, "SalesOrder"); ok {
		t.Fatal("pos.sale.read must not create SalesOrder")
	}
}

func TestModelGrantsDoNotLeakToUnmappedModels(t *testing.T) {
	g := posGrants()
	held := []string{"pos.sale.read", "pos.unmapped.read"}
	for _, m := range []string{"Customer", "PriceType", "POSSession"} {
		if _, ok := g.Allows(held, []string{"index"}, m); ok {
			t.Fatalf("capability leaked onto unmapped model %s", m)
		}
	}
	if _, ok := g.Allows([]string{"other.sale.read"}, []string{"index"}, "SalesOrder"); ok {
		t.Fatal("an undeclared capability must not resolve")
	}
}

func TestModelGrantsTwoSegmentCustomAction(t *testing.T) {
	g := posGrants()
	if _, ok := g.Allows([]string{"refund_claims.settle"}, []string{"action.settle"}, "RefundClaim"); !ok {
		t.Fatal("two-segment capability with custom action must resolve")
	}
}

func TestModelGrantsReplaceAndUnregister(t *testing.T) {
	g := posGrants()
	g.Register("pos", nil)
	if _, ok := g.Allows([]string{"pos.sale.read"}, []string{"index"}, "SalesOrder"); ok {
		t.Fatal("re-register with no models must drop the mapping")
	}
}
