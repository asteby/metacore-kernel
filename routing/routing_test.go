package routing

import (
	"testing"

	"github.com/asteby/metacore-kernel/manifest"
)

// fulfillmentContribs is the scenario the primitive was built for: inventory
// owns the default (move stock directly), a warehouse addon overrides it with
// allocation when installed, and workshop claims service lines.
func fulfillmentContribs() []Contribution {
	return []Contribution{
		{
			AddonKey: "inventory",
			Routes: []manifest.RouteDef{
				{Domain: "fulfillment", Handler: "stock_direct"},
				{Domain: "fulfillment", Match: map[string]string{"product_type": "storable"}, Handler: "stock_direct", Priority: 10},
			},
		},
		{
			AddonKey: "warehouse",
			Routes: []manifest.RouteDef{
				{
					Domain:    "fulfillment",
					Match:     map[string]string{"product_type": "storable"},
					Handler:   "stock_allocate",
					Priority:  100,
					Condition: &manifest.ConditionDef{AddonInstalled: "warehouse"},
				},
			},
		},
		{
			AddonKey: "workshop",
			Routes: []manifest.RouteDef{
				{
					Domain:    "fulfillment",
					Match:     map[string]string{"product_type": "service"},
					Handler:   "service_workorder",
					Priority:  100,
					Condition: &manifest.ConditionDef{AddonInstalled: "workshop"},
				},
			},
		},
	}
}

func installedSet(keys ...string) InstalledFn {
	set := map[string]struct{}{}
	for _, k := range keys {
		set[k] = struct{}{}
	}
	return func(key string) bool {
		_, ok := set[key]
		return ok
	}
}

// TestResolveFulfillment walks the three org shapes the router has to tell
// apart: bare inventory, inventory + warehouse, inventory + workshop.
func TestResolveFulfillment(t *testing.T) {
	cases := []struct {
		name        string
		installed   InstalledFn
		productType string
		want        string
		wantAddon   string
	}{
		{"storable, no warehouse", installedSet("inventory"), "storable", "stock_direct", "inventory"},
		{"storable, warehouse installed", installedSet("inventory", "warehouse"), "storable", "stock_allocate", "warehouse"},
		{"service, workshop installed", installedSet("inventory", "workshop"), "service", "service_workorder", "workshop"},
		// A service line in an org with no workshop falls back to the
		// catch-all rather than resolving to a handler nobody provides.
		{"service, no workshop", installedSet("inventory"), "service", "stock_direct", "inventory"},
		// An unknown product type is not an error: the catch-all owns it.
		{"unknown type", installedSet("inventory"), "kit", "stock_direct", "inventory"},
	}
	for _, tc := range cases {
		table := Build(fulfillmentContribs(), tc.installed)
		got, addon, ok := table.Resolve("fulfillment", map[string]string{"product_type": tc.productType})
		if !ok {
			t.Errorf("%s: no route resolved", tc.name)
			continue
		}
		if got != tc.want || addon != tc.wantAddon {
			t.Errorf("%s: resolved %q from %q, want %q from %q", tc.name, got, addon, tc.want, tc.wantAddon)
		}
	}
}

// TestResolveUnknownDomain asserts the resolver reports "no route" instead of
// inventing a default the kernel does not own.
func TestResolveUnknownDomain(t *testing.T) {
	table := Build(fulfillmentContribs(), installedSet("inventory"))
	if _, _, ok := table.Resolve("pricing", map[string]string{"product_type": "storable"}); ok {
		t.Error("resolved a handler for a domain with no routes")
	}
	var nilTable *Table
	if _, _, ok := nilTable.Resolve("fulfillment", nil); ok {
		t.Error("nil table resolved a handler")
	}
}

// TestSpecificityBreaksPriorityTies asserts a catch-all never shadows a
// specific rule declared at the same priority.
func TestSpecificityBreaksPriorityTies(t *testing.T) {
	contribs := []Contribution{{
		AddonKey: "a",
		Routes: []manifest.RouteDef{
			{Domain: "d", Handler: "fallback"},
			{Domain: "d", Match: map[string]string{"k": "v"}, Handler: "specific"},
		},
	}}
	table := Build(contribs, nil)
	if got, _, _ := table.Resolve("d", map[string]string{"k": "v"}); got != "specific" {
		t.Errorf("resolved %q, want specific", got)
	}
	if got, _, _ := table.Resolve("d", map[string]string{"k": "other"}); got != "fallback" {
		t.Errorf("resolved %q, want fallback", got)
	}
}

// TestResolveIsDeterministic asserts the winner does not depend on the order
// addons happen to be listed in: two equally specific, equally prioritized
// routes are broken by addon key.
func TestResolveIsDeterministic(t *testing.T) {
	forward := []Contribution{
		{AddonKey: "zeta", Routes: []manifest.RouteDef{{Domain: "d", Handler: "from_zeta"}}},
		{AddonKey: "alpha", Routes: []manifest.RouteDef{{Domain: "d", Handler: "from_alpha"}}},
	}
	reversed := []Contribution{forward[1], forward[0]}
	for _, contribs := range [][]Contribution{forward, reversed} {
		if got, _, _ := Build(contribs, nil).Resolve("d", nil); got != "from_alpha" {
			t.Errorf("resolved %q, want from_alpha regardless of contribution order", got)
		}
	}
}

// TestBuildDropsUnsatisfiedRoutes covers the installation gate and the
// permissive nil-resolver default (an unaware host keeps every route).
func TestBuildDropsUnsatisfiedRoutes(t *testing.T) {
	got := Build(fulfillmentContribs(), installedSet("inventory")).Handlers("fulfillment")
	for _, h := range got {
		if h == "stock_allocate" || h == "service_workorder" {
			t.Errorf("gated handler %q survived for an org without its addon: %v", h, got)
		}
	}
	all := Build(fulfillmentContribs(), nil).Handlers("fulfillment")
	if len(all) != 3 {
		t.Errorf("nil resolver dropped routes: %v", all)
	}
	// Handlers is precedence-ordered and de-duplicated: stock_direct appears
	// twice in the contributions but once here, after the prioritized ones.
	full := Build(fulfillmentContribs(), installedSet("inventory", "warehouse", "workshop")).Handlers("fulfillment")
	if len(full) != 3 || full[len(full)-1] != "stock_direct" {
		t.Errorf("Handlers = %v, want 3 entries with the catch-all last", full)
	}
}
