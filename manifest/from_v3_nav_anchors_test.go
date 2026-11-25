package manifest_test

import (
	"encoding/json"
	"testing"

	"github.com/asteby/metacore-kernel/manifest"
	v3 "github.com/asteby/metacore-kernel/manifest/v3"
)

func TestFromV3_NavAnchorsAndContractRequires(t *testing.T) {
	order := 10
	m := &v3.Manifest{
		Metadata: v3.Metadata{Key: "customers", Version: "5.0.0"},
		Compatibility: v3.Compatibility{Requires: []v3.Requirement{
			{Key: "kernel", Version: ">=3.0.0 <4.0.0"},
			{Contract: "payments.charge", Version: "^1", Optional: true, Enables: []string{"action:SalesOrder.charge"}},
		}},
		Models: []v3.Model{{Key: "SalesOrder", Table: "sales_orders"}},
		ExtensionPoints: &v3.ExtensionPoints{NavAnchors: []v3.NavAnchor{
			{Key: "sales.order", Section: "sales", Title: "nav.sales.orders", Model: "SalesOrder", Order: &order, Accepts: []string{"tab", "filter"}, Provisional: true},
		}},
		Contributions: &v3.Contributions{Navigation: []v3.NavGroup{{
			Key: "customers.sales", Section: "sales", Order: &order, Title: "nav.sales.orders",
			Items: []v3.NavItem{
				{Key: "pending_credit", Title: "t", AttachTo: "sales.order", As: "filter", Filter: map[string]string{"state": "credit_review"}, Order: &order},
				{Key: "register", Title: "t", AttachTo: "sales.order", As: "mode", Page: "register"},
				{Title: "legacy", Page: "dash"},
			},
		}}},
	}
	out := manifest.FromV3(m)
	if len(out.ContractRequires) != 1 || out.ContractRequires[0].Contract != "payments.charge" || out.ContractRequires[0].Range != "^1" || !out.ContractRequires[0].Optional {
		t.Fatalf("ContractRequires = %+v", out.ContractRequires)
	}
	for _, r := range out.AddonRequires {
		if r.Key == "" {
			t.Fatalf("a contract require leaked into AddonRequires: %+v", out.AddonRequires)
		}
	}
	if len(out.NavAnchors) != 1 || out.NavAnchors[0].Key != "sales.order" || !out.NavAnchors[0].Provisional {
		t.Fatalf("NavAnchors = %+v", out.NavAnchors)
	}
	*m.ExtensionPoints.NavAnchors[0].Order = 99 // projection must not alias
	if *out.NavAnchors[0].Order != 10 {
		t.Fatal("NavAnchors order aliases the v3 manifest")
	}
	g := out.Navigation[0]
	if g.Key != "customers.sales" || g.Section != "sales" || g.Order == nil {
		t.Fatalf("group = %+v", g)
	}
	if it := g.Items[0]; it.Key != "pending_credit" || it.AttachTo != "sales.order" || it.As != "filter" {
		t.Fatalf("item = %+v", it)
	}
	if g.Items[1].Page != "register" {
		t.Fatalf("attached mode item must carry page: %+v", g.Items[1])
	}
	if g.Items[2].Page != "" {
		t.Fatalf("a legacy (unattached) item must not change shape: %+v", g.Items[2])
	}
}

// A manifest that uses none of the new fields serializes its navigation
// exactly as before (no new keys).
func TestFromV3_LegacyNavigationShapeUnchanged(t *testing.T) {
	m := &v3.Manifest{
		Metadata: v3.Metadata{Key: "tickets", Version: "1.0.0"},
		Models:   []v3.Model{{Key: "Ticket", Table: "tickets"}},
		Contributions: &v3.Contributions{Navigation: []v3.NavGroup{{
			Title: "Tickets", Icon: "Ticket", Target: "sidebar.tickets",
			Items: []v3.NavItem{{Title: "Open", Model: "Ticket", Filter: map[string]string{"status": "open"}}, {Title: "Board", Page: "board"}},
		}}},
	}
	b, _ := json.Marshal(manifest.FromV3(m).Navigation)
	want := `[{"title":"Tickets","icon":"Ticket","target":"sidebar.tickets","items":[{"title":"Open","url":"/m/tickets","model":"Ticket","filter":{"status":"open"}},{"title":"Board"}]}]`
	if string(b) != want {
		t.Fatalf("legacy navigation changed:\n got %s\nwant %s", b, want)
	}
}
