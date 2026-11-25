package navigation

import (
	"encoding/json"
	"math/rand"
	"os"
	"strings"
	"testing"

	"github.com/asteby/metacore-kernel/manifest"
	v3 "github.com/asteby/metacore-kernel/manifest/v3"
)

// TestBuildFor_LegacyGolden pins the v1 merger on a legacy fleet: the golden
// file was generated BEFORE the anchor fields existed, so any drift here means
// a manifest without the new fields no longer produces the same navigation.
func TestBuildFor_LegacyGolden(t *testing.T) {
	want, err := os.ReadFile("testdata/legacy_buildfor.golden.json")
	if err != nil {
		t.Fatal(err)
	}
	got, _ := json.MarshalIndent(BuildFor(legacyCore(), legacyContribs(), nil), "", "  ")
	if string(append(got, '\n')) != string(want) {
		t.Fatalf("legacy navigation changed:\n%s", got)
	}
}

func legacyAddons() []Addon {
	var out []Addon
	for _, c := range legacyContribs() {
		out = append(out, Addon{Key: c.AddonKey, Groups: c.Groups})
	}
	return out
}

// TestResolve_LegacyKeepsEveryEntry: with manifests that use none of the new
// fields, the v2 resolver keeps every entry of v1 exactly once, in the section
// its target maps to (RFC §7 table), in the v1 order, with no dedupe across
// addons.
func TestResolve_LegacyKeepsEveryEntry(t *testing.T) {
	tree := Resolve(Input{Addons: legacyAddons()})
	if len(tree.Dedupe) != 0 {
		t.Fatalf("legacy groups must not be deduped across addons: %+v", tree.Dedupe)
	}
	type placed struct{ section, owner string }
	got := map[string][]string{} // section -> "owner:title" in order
	for _, s := range tree.Sections {
		for _, e := range s.Entries {
			if e.Kind != EntryGroup {
				t.Fatalf("legacy fleet produced an anchor: %+v", e)
			}
			for _, it := range e.Items {
				got[s.Key] = append(got[s.Key], e.Owner+":"+it.Title)
			}
		}
	}
	want := map[string][]string{}
	for _, c := range legacyContribs() {
		for _, g := range c.Groups {
			sec, _ := v3.NavSectionForTarget(g.Target)
			for _, it := range g.Items {
				want[sec] = append(want[sec], c.AddonKey+":"+it.Title)
			}
		}
	}
	// v1 iterates addons by key; so does v2 inside a section (legacy groups
	// all have order 1000).
	for sec := range want {
		if strings.Join(got[sec], "|") != strings.Join(sortedByAddon(want[sec]), "|") {
			t.Errorf("section %s:\n got %v\nwant %v", sec, got[sec], sortedByAddon(want[sec]))
		}
	}
	if len(got) != len(want) {
		t.Fatalf("sections = %v, want %v", keysOf(got), keysOf(want))
	}
	// Every legacy item of v1 is the same Item (url, filter, owner…) in v2.
	v1 := map[string]Item{}
	for _, g := range BuildFor(nil, legacyContribs(), nil) {
		for _, it := range g.Items {
			v1[it.Owner+"|"+it.Title+"|"+it.URL] = it
		}
	}
	for _, s := range tree.Sections {
		for _, e := range s.Entries {
			for _, it := range e.Items {
				a, _ := json.Marshal(it.Item)
				b, _ := json.Marshal(v1[it.Owner+"|"+it.Title+"|"+it.URL])
				if string(a) != string(b) {
					t.Errorf("item differs from v1:\n v2 %s\n v1 %s", a, b)
				}
			}
		}
	}
	unknown := 0
	for _, w := range tree.Warnings {
		if strings.Contains(w, v3.RuleNavTargetUnknown) {
			unknown++
		}
	}
	if unknown != 1 { // mail: sidebar.tickets
		t.Fatalf("want 1 nav_target_unknown warning, got %v", tree.Warnings)
	}
}

func sortedByAddon(in []string) []string {
	out := append([]string(nil), in...)
	// stable by addon key (the part before ':'), keeping declaration order
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && addonOf(out[j]) < addonOf(out[j-1]); j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

func addonOf(s string) string { return s[:strings.Index(s, ":")] }

func keysOf[V any](m map[string]V) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

func findEntry(t *testing.T, tree Tree, section, key string) Entry {
	t.Helper()
	for _, s := range tree.Sections {
		if s.Key != section {
			continue
		}
		for _, e := range s.Entries {
			if e.Key == key {
				return e
			}
		}
	}
	t.Fatalf("entry %s/%s not found in %s", section, key, dump(tree))
	return Entry{}
}

func ids(e Entry) string {
	var s []string
	for _, it := range e.Items {
		s = append(s, it.ID+"@"+it.As)
	}
	return strings.Join(s, ",")
}

func dump(tree Tree) string { b, _ := json.MarshalIndent(tree, "", " "); return string(b) }

func TestResolve_AnchoredFleet(t *testing.T) {
	tree := Resolve(Input{Addons: anchoredFleet()})

	order := findEntry(t, tree, "sales", "sales.order")
	if got, want := ids(order), "fiscal_mexico.invoices_tab@tab,customers.pending_credit@filter,channel_orders.orders@view,fiscal_mexico.stamp@action,pos.register@mode"; got != want {
		t.Fatalf("sales.order items:\n got %s\nwant %s", got, want)
	}
	if order.Owner != "customers" || !order.Provisional {
		t.Fatalf("sales.order owner = %+v", order)
	}
	product := findEntry(t, tree, "catalog", "catalog.product")
	if got, want := ids(product), "products.board@view,link_catalog.sync@action,products-tires.tires@filter"; got != want {
		t.Fatalf("catalog.product items:\n got %s\nwant %s", got, want)
	}
	// RFC §4.4: groups whose items all plug in paint no group; the products
	// group only listed the anchor's own resource, deduped away.
	for _, s := range tree.Sections {
		for _, e := range s.Entries {
			if e.Kind == EntryGroup && (e.Owner == "link_catalog" || e.Owner == "pos" || e.Owner == "fiscal_mexico" || e.Owner == "products" || e.Owner == "customers") {
				t.Fatalf("unexpected own group %+v", e)
			}
		}
	}
	wantDedupe := map[string]string{
		"products.list":         "catalog.product",
		"link_catalog.board":    "products.board",
		"storefront_api.orders": "channel_orders.orders",
	}
	if len(tree.Dedupe) != len(wantDedupe) {
		t.Fatalf("dedupe = %+v", tree.Dedupe)
	}
	for _, d := range tree.Dedupe {
		if wantDedupe[d.Loser] != d.Winner {
			t.Fatalf("dedupe %+v, want winner %q", d, wantDedupe[d.Loser])
		}
	}
	// communication.inbox has no owner: view falls back to an own entry,
	// panel hides.
	mail := findEntry(t, tree, "communication", "mail.inbox")
	inbox := findEntry(t, tree, "communication", "link-inbox.inbox")
	if ids(mail) != "mail.messages@" || mail.Items[0].AnchorMissing != "communication.inbox" || ids(inbox) != "link-inbox.messages@" {
		t.Fatalf("fallback entries: mail=%s inbox=%s", ids(mail), ids(inbox))
	}
	caja := findEntry(t, tree, "finance", "caja.finance")
	if ids(caja) != "caja.registers@" {
		t.Fatalf("caja = %s", ids(caja))
	}
	var secs []string
	for _, s := range tree.Sections {
		secs = append(secs, s.Key)
	}
	if strings.Join(secs, ",") != "sales,customers,catalog,finance,communication" {
		t.Fatalf("sections = %v", secs)
	}
	if len(tree.Warnings) != 0 {
		t.Fatalf("warnings = %v", tree.Warnings)
	}
}

func without(addons []Addon, key string) []Addon {
	var out []Addon
	for _, a := range addons {
		if a.Key != key {
			out = append(out, a)
		}
	}
	return out
}

// Without the anchor owner (or without its permission) each plug-in applies
// its fallback: Caja and the channel views become own entries in sales, the
// invoices tab and the stamp action disappear.
func TestResolve_FallbackWhenAnchorAbsent(t *testing.T) {
	check := func(t *testing.T, tree Tree) {
		t.Helper()
		pos := findEntry(t, tree, "sales", "pos.main")
		if ids(pos) != "pos.register@" || pos.Items[0].AnchorMissing != "sales.order" || pos.Items[0].Page != "register" {
			t.Fatalf("pos fallback = %+v", pos)
		}
		findEntry(t, tree, "sales", "channel_orders.sales")
		findEntry(t, tree, "sales", "storefront_api.sales")
		for _, s := range tree.Sections {
			for _, e := range s.Entries {
				if e.Key == "sales.order" || e.Owner == "fiscal_mexico" {
					t.Fatalf("unexpected entry %+v", e)
				}
			}
		}
		// pos (order 5) before the channel groups (order 1000).
		if s := tree.Sections[0]; s.Key != "sales" || s.Entries[0].Key != "pos.main" {
			t.Fatalf("sales order = %s", dump(tree))
		}
	}
	t.Run("owner not installed", func(t *testing.T) { check(t, Resolve(Input{Addons: without(anchoredFleet(), "customers")})) })
	t.Run("anchor not visible", func(t *testing.T) {
		tree := Resolve(Input{Addons: anchoredFleet(), Allowed: func(p string) bool { return p != "salesorder.index" }})
		check(t, tree)
		findEntry(t, tree, "customers", "customers.customer")
	})
}

func TestResolve_RBACPerLeaf(t *testing.T) {
	tree := Resolve(Input{Addons: anchoredFleet(), Allowed: func(p string) bool { return p != "invoice.index" }})
	if got := ids(findEntry(t, tree, "sales", "sales.order")); strings.Contains(got, "invoices_tab") || !strings.Contains(got, "stamp") {
		t.Fatalf("items = %s", got)
	}
}

func TestResolve_DeterministicAcrossLoadOrder(t *testing.T) {
	base, _ := json.Marshal(Resolve(Input{Addons: anchoredFleet()}))
	legacy, _ := json.Marshal(Resolve(Input{Addons: legacyAddons()}))
	r := rand.New(rand.NewSource(7))
	for i := 0; i < 10; i++ {
		a := anchoredFleet()
		r.Shuffle(len(a), func(i, j int) { a[i], a[j] = a[j], a[i] })
		got, _ := json.Marshal(Resolve(Input{Addons: a}))
		if string(got) != string(base) {
			t.Fatalf("run %d differs:\n%s\n%s", i, got, base)
		}
		l := legacyAddons()
		r.Shuffle(len(l), func(i, j int) { l[i], l[j] = l[j], l[i] })
		if got, _ := json.Marshal(Resolve(Input{Addons: l})); string(got) != string(legacy) {
			t.Fatalf("legacy run %d differs", i)
		}
	}
}

func TestResolve_DefinitiveOwnerBeatsProvisional(t *testing.T) {
	fleet := append(anchoredFleet(), Addon{Key: "orders_core", FirstParty: true, Anchors: []v3.NavAnchor{
		{Key: "sales.order", Section: "sales", Title: "nav.sales.orders", Model: "SalesOrder", Accepts: []string{"tab", "view", "action", "filter", "mode"}},
	}})
	tree := Resolve(Input{Addons: fleet})
	e := findEntry(t, tree, "sales", "sales.order")
	if e.Owner != "orders_core" || e.Provisional {
		t.Fatalf("owner = %+v", e)
	}
	// pending_credit (filter) is still accepted; the plugs did not change.
	if !strings.Contains(ids(e), "customers.pending_credit@filter") {
		t.Fatalf("items = %s", ids(e))
	}
	if len(tree.Warnings) != 1 || !strings.Contains(tree.Warnings[0], "replaces provisional") {
		t.Fatalf("warnings = %v", tree.Warnings)
	}
	_, warns := CheckInstalled(fleet)
	if len(warns) != 1 {
		t.Fatalf("CheckInstalled warns = %v", warns)
	}
}

func TestCheckInstalled_Errors(t *testing.T) {
	two := append(anchoredFleet(), Addon{Key: "catalog_core", Anchors: []v3.NavAnchor{
		{Key: "catalog.product", Section: "catalog", Title: "x", Model: "Product", Accepts: []string{"tab"}},
	}})
	errs, _ := CheckInstalled(two)
	if len(errs) != 1 || !strings.Contains(errs[0], "catalog_core, products") {
		t.Fatalf("errs = %v", errs)
	}
	bad := append(anchoredFleet(), Addon{Key: "acme", Groups: []manifest.NavGroup{{Title: "x", Items: []manifest.NavItem{
		{Key: "s", Title: "x", AttachTo: "customers.customer", As: "setting", Page: "p"},
		{Key: "n", Title: "x", AttachTo: "operations.nowhere", As: "tab", Model: "X"},
	}}}})
	errs, _ = CheckInstalled(bad)
	if len(errs) != 1 || !strings.Contains(errs[0], `does not accept as "setting"`) {
		t.Fatalf("errs = %v", errs)
	}
	if errs, warns := CheckInstalled(anchoredFleet()); len(errs) != 0 || len(warns) != 0 {
		t.Fatalf("clean fleet: %v %v", errs, warns)
	}
	// Resolve drops the unaccepted plug-in with a warning instead of failing.
	tree := Resolve(Input{Addons: bad})
	if !strings.Contains(strings.Join(tree.Warnings, "\n"), "does not accept") {
		t.Fatalf("warnings = %v", tree.Warnings)
	}
}

func TestResolve_SettingsAndConditions(t *testing.T) {
	tree := Resolve(Input{
		Addons: []Addon{{Key: "acme", Groups: []manifest.NavGroup{
			{Key: "acme.settings", Section: "settings", Title: "s", Items: []manifest.NavItem{{Key: "general", Title: "g", URL: "/settings/acme/"}}},
			{Key: "acme.cond", Section: "operations", Title: "c", Condition: &manifest.ConditionDef{AddonInstalled: "other"}, Items: []manifest.NavItem{{Title: "x", URL: "/x"}}},
			{Key: "acme.dupes", Section: "operations", Title: "d", Items: []manifest.NavItem{
				{Key: "b", Title: "b", URL: "/m/x?b=2&a=1"}, {Key: "a", Title: "a", URL: "/m/x/?a=1&b=2"},
			}},
		}}},
		Installed: func(string) bool { return false },
	})
	if tree.Settings == nil || tree.Settings.Entries[0].Key != "acme.settings" {
		t.Fatalf("settings = %s", dump(tree))
	}
	if len(tree.Sections) != 1 || len(tree.Sections[0].Entries) != 1 {
		t.Fatalf("condition not applied: %s", dump(tree))
	}
	d := findEntry(t, tree, "operations", "acme.dupes")
	if ids(d) != "acme.a@" || len(tree.Dedupe) != 1 || tree.Dedupe[0].Loser != "acme.b" {
		t.Fatalf("url dedupe: %s %+v", ids(d), tree.Dedupe)
	}
}

// The v1 merger, which knows no anchors, degrades an anchored manifest: a
// hidden plug-in (action, panel, setting, fallback hide) is dropped and a group
// made only of hidden plug-ins disappears.
func TestBuildFor_DropsHiddenAttachments(t *testing.T) {
	var contribs []Contribution
	for _, a := range anchoredFleet() {
		contribs = append(contribs, Contribution{AddonKey: a.Key, Groups: a.Groups})
	}
	var titles []string
	for _, g := range BuildFor(nil, contribs, nil) {
		for _, it := range g.Items {
			titles = append(titles, it.Title)
		}
	}
	got := strings.Join(titles, ",")
	for _, hidden := range []string{"actions.stamp", "nav.fiscal.invoices", "nav.link_catalog.sync", "nav.inbox.assign"} {
		if strings.Contains(got, hidden) {
			t.Fatalf("%s must be hidden in v1: %s", hidden, got)
		}
	}
	for _, kept := range []string{"nav.pos.register", "nav.sales.pending_credit", "nav.tires"} {
		if !strings.Contains(got, kept) {
			t.Fatalf("%s must stay in v1 (fallback section): %s", kept, got)
		}
	}
}
