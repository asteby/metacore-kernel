package v3

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// injectBase is the shipping UI example plus a sales_order_id foreign key on
// Shipment: the contributor of the DISENO-EXTENSIBILIDAD §B.1 examples.
func injectBase(t *testing.T) map[string]any {
	t.Helper()
	m := uiExample(t)
	ship := obj(arr(m["models"])[0])
	ship["columns"] = append(arr(ship["columns"]), map[string]any{"name": "sales_order_id", "type": "uuid"})
	return m
}

func withInject(t *testing.T, in ...any) map[string]any {
	t.Helper()
	m := injectBase(t)
	c := obj(m["contributions"])
	c["inject"] = in
	return m
}

func shipmentPanel() map[string]any {
	return map[string]any{
		"key": "shipment_panel", "kind": "panel", "target": "@commerce.order", "slot": "record.tabs",
		"order": 300, "permission": "shipping.shipment.read",
		"condition": map[string]any{"addon_installed": "shipping"},
		"panel": map[string]any{
			"key": "shipments", "kind": "relation", "title": "shipping.panel.title",
			"relation": map[string]any{"via": "Shipment.sales_order_id", "columns": []any{"status", "carrier", "tracking_number"}},
		},
	}
}

func shippingStatusColumn() map[string]any {
	return map[string]any{
		"key": "shipping_status", "kind": "column", "target": "@commerce.order", "slot": "list.columns",
		"column": map[string]any{
			"label": "shipping.col.status", "from": "Shipment", "foreign_key": "sales_order_id",
			"pick": "latest", "field": "status", "order_by": "updated_at", "display": "badge",
		},
	}
}

func routeMapPanel() map[string]any {
	return map[string]any{
		"key": "route_map", "kind": "panel", "target": "shipping.Shipment", "slot": "record.panels",
		"panel": map[string]any{
			"key": "route", "kind": "map", "title": "shipping.panel.route",
			"map": map[string]any{"point": map[string]any{"lat": "dest_lat", "lng": "dest_lng"}},
		},
	}
}

func buyLabelAction() map[string]any {
	return map[string]any{
		"key": "buy_label", "target_model": "@commerce.order", "placement": "row", "priority": "secondary",
		"handler": map[string]any{"type": "wasm", "function": "buy_label_for_order"},
	}
}

func TestInject_AcceptsDesignExamples(t *testing.T) {
	m := withInject(t, shipmentPanel(), shippingStatusColumn(), routeMapPanel())
	obj(m["contributions"])["actions"] = []any{buyLabelAction()}
	mf, err := Parse(mustJSON(t, m))
	if err != nil {
		t.Fatalf("expected the §B.1 examples to validate: %v", err)
	}
	if got := len(mf.Contributions.Inject); got != 3 {
		t.Fatalf("inject decoded %d entries, want 3", got)
	}
	if mf.Contributions.Actions[0].TargetModel != "@commerce.order" {
		t.Fatalf("target_model = %q", mf.Contributions.Actions[0].TargetModel)
	}
	if o := mf.Contributions.Inject[2].Order; o != nil {
		t.Fatalf("absent order must decode as nil, got %d", *o)
	}
}

func TestInject_AllKindsAndSealedPrimaryFrom(t *testing.T) {
	m := withInject(t,
		shippingStatusColumn(),
		map[string]any{"key": "shipping_filter", "kind": "filter", "target": "@commerce.order", "filter": map[string]any{"column": "shipping_status"}},
		map[string]any{"key": "ship_badge", "kind": "badge", "target": "@commerce.order",
			"badge": map[string]any{"from": "Shipment", "foreign_key": "sales_order_id", "field": "status", "values": map[string]any{"delivered": "success", "failed": "danger"}}},
		map[string]any{"key": "open_ships", "kind": "column", "target": "customers.SalesOrder",
			"column": map[string]any{"label": "l", "from": "Shipment", "foreign_key": "sales_order_id", "pick": "agg", "aggregate": "count"}},
		map[string]any{"key": "tab_one", "kind": "tab", "target": "customers.SalesOrder", "panel": map[string]any{
			"key": "stats", "kind": "stats", "title": "t", "stats": map[string]any{"items": []any{map[string]any{"field": "total"}}}}},
		map[string]any{"key": "ship_widget", "kind": "widget", "slot": "dashboard.widgets",
			"widget": map[string]any{"key": "by_carrier", "title": "t", "kind": "bar", "query": map[string]any{"model": "Shipment", "group_by": "carrier"}}},
	)
	m["extension_points"] = map[string]any{"sealed": []any{"ShipmentEvent"}, "primary_from": []any{"carriers"}}
	if _, err := Parse(mustJSON(t, m)); err != nil {
		t.Fatalf("expected valid: %v", err)
	}
}

func TestInject_OwnerBand(t *testing.T) {
	p := routeMapPanel()
	p["order"] = 50
	if _, err := Parse(mustJSON(t, withInject(t, p))); err != nil {
		t.Fatalf("owner order 50 must be valid: %v", err)
	}
	p["order"] = 250
	expectInjectErr(t, withInject(t, p), "0-199 band")
}

func TestInject_Rejects(t *testing.T) {
	lowOrder := shipmentPanel()
	lowOrder["order"] = 100
	zeroOrder := shipmentPanel()
	zeroOrder["order"] = 0
	badKind := shipmentPanel()
	badKind["kind"] = "sparkle"
	wrongBody := shipmentPanel()
	wrongBody["column"] = shippingStatusColumn()["column"]
	missingBody := shipmentPanel()
	delete(missingBody, "panel")
	badTarget := shipmentPanel()
	badTarget["target"] = "customers.salesorder"
	bareTarget := shipmentPanel()
	bareTarget["target"] = "SalesOrder"
	badRole := shipmentPanel()
	badRole["target"] = "@Commerce"
	noTarget := shipmentPanel()
	delete(noTarget, "target")
	badSlot := shipmentPanel()
	badSlot["slot"] = "record.footer"
	wrongSlot := shipmentPanel()
	wrongSlot["slot"] = "list.columns"
	badPick := shippingStatusColumn()
	obj(badPick["column"])["pick"] = "newest"
	noOrderBy := shippingStatusColumn()
	delete(obj(noOrderBy["column"]), "order_by")
	foreignFrom := shippingStatusColumn()
	obj(foreignFrom["column"])["from"] = "Invoice"
	badFK := shippingStatusColumn()
	obj(badFK["column"])["foreign_key"] = "nope_id"
	aggNoAgg := shippingStatusColumn()
	obj(aggNoAgg["column"])["pick"] = "agg"
	viaName := shipmentPanel()
	obj(obj(viaName["panel"])["relation"])["name"] = "items"
	viaBad := shipmentPanel()
	obj(obj(viaBad["panel"])["relation"])["via"] = "Shipment"
	dup := shippingStatusColumn()
	badFilter := map[string]any{"key": "f", "kind": "filter", "target": "@commerce.order", "filter": map[string]any{"column": "ghost"}}
	badTone := map[string]any{"key": "b", "kind": "badge", "target": "@commerce.order",
		"badge": map[string]any{"from": "Shipment", "foreign_key": "sales_order_id", "field": "status", "values": map[string]any{"x": "purple"}}}

	cases := []struct {
		name string
		m    map[string]any
		want string
	}{
		{"order below 200", withInject(t, lowOrder), "order 100 is below 200"},
		{"order 0 explicit", withInject(t, zeroOrder), "order 0 is below 200"},
		{"unknown kind", withInject(t, badKind), "/contributions/inject/0/kind"},
		{"body does not match kind", withInject(t, wrongBody), `declares "column" but kind is "panel"`},
		{"missing body", withInject(t, missingBody), `kind "panel" needs the "panel" block`},
		{"concrete target malformed", withInject(t, badTarget), `must be a concrete "<addon>.<Model>"`},
		{"bare target", withInject(t, bareTarget), `must be a concrete "<addon>.<Model>"`},
		{"role target malformed", withInject(t, badRole), `or a role "@<domain>.<entity>"`},
		{"target required", withInject(t, noTarget), "target is required"},
		{"slot outside catalog", withInject(t, badSlot), "/contributions/inject/0/slot"},
		{"slot does not accept kind", withInject(t, wrongSlot), "does not accept kind=panel"},
		{"bad pick", withInject(t, badPick), "/contributions/inject/0/column/pick"},
		{"latest needs order_by", withInject(t, noOrderBy), "order_by is required for pick=latest"},
		{"from not own model", withInject(t, foreignFrom), "not one of this addon's models"},
		{"foreign_key not a column", withInject(t, badFK), `"nope_id" is not a column of model "Shipment"`},
		{"agg needs aggregate", withInject(t, aggNoAgg), "required for pick=agg"},
		{"relation name in inject", withInject(t, viaName), "relation.name is not valid in an inject panel"},
		{"relation via malformed", withInject(t, viaBad), "/panel/relation/via"},
		{"duplicate key", withInject(t, dup, shippingStatusColumn()), "duplicated"},
		{"filter without column inject", withInject(t, badFilter), `"ghost" is not the key of a kind=column`},
		{"badge tone", withInject(t, badTone), "/badge/values/x"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { expectInjectErr(t, c.m, c.want) })
	}
}

func TestInject_ActionTargets(t *testing.T) {
	good := []string{"", "Shipment", "customers.SalesOrder", "@commerce.order"}
	for _, g := range good {
		if msg := ActionTargetProblem(g); msg != "" {
			t.Errorf("target %q must be accepted: %s", g, msg)
		}
	}
	for _, b := range []string{"customers.salesorder", "@commerce", "@Commerce.Order", "a.b.C", "customers."} {
		if ActionTargetProblem(b) == "" {
			t.Errorf("target %q must be rejected", b)
		}
	}
	m := injectBase(t)
	a := buyLabelAction()
	a["target_model"] = "@commerce"
	obj(m["contributions"])["actions"] = []any{a}
	expectInjectErr(t, m, "contributions.actions[0].target_model")
}

func TestInject_ExtensionPointsLists(t *testing.T) {
	m := injectBase(t)
	m["extension_points"] = map[string]any{"sealed": []any{"Ghost"}}
	expectInjectErr(t, m, "extension_points.sealed[0]")
	m["extension_points"] = map[string]any{"primary_from": []any{"Not An Addon"}}
	expectInjectErr(t, m, "/extension_points/primary_from/0")
}

func TestInject_OwnPanelRelationRejectsVia(t *testing.T) {
	m := uiExample(t)
	shipment := obj(arr(m["models"])[0])
	shipment["detail"] = map[string]any{"panels": []any{map[string]any{
		"key": "events", "kind": "relation", "title": "t", "relation": map[string]any{"via": "ShipmentEvent.shipment_id"},
	}}}
	expectInjectErr(t, m, "via is only valid in contributions.inject")
}

func TestDashboard_MapKind(t *testing.T) {
	ok := withDashboard([]interface{}{map[string]interface{}{
		"key": "by_state", "title": "t", "kind": "map",
		"query": map[string]interface{}{"model": "Shipment", "group_by": "dest_state", "aggregate": "count"},
	}}, false)
	if err := Validate(mustJSON(t, ok)); err != nil {
		t.Fatalf("kind map with a geographic group_by must validate: %v", err)
	}
	bad := withDashboard([]interface{}{map[string]interface{}{
		"key": "by_state", "title": "t", "kind": "map",
		"query": map[string]interface{}{"model": "Shipment"},
	}}, false)
	err := Validate(mustJSON(t, bad))
	if err == nil || !strings.Contains(err.Error(), "group_by is required for kind=map") {
		t.Fatalf("map without group_by must be refused, got: %v", err)
	}
}

func expectInjectErr(t *testing.T, m map[string]any, want string) {
	t.Helper()
	_, err := Parse(mustJSON(t, m))
	if err == nil {
		t.Fatalf("expected an error containing %q, got none", want)
	}
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("error %q does not contain %q", err.Error(), want)
	}
}

// The two published copies of the schema must never drift: the embedded one
// (manifest/v3/schema) validates, the docs one is what authors read.
func TestSchemaCopiesAreByteIdentical(t *testing.T) {
	a, err := os.ReadFile("schema/manifest-v3.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile("../../docs/spec/v3/manifest-v3.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a, b) {
		t.Fatal("manifest/v3/schema/manifest-v3.schema.json and docs/spec/v3/manifest-v3.schema.json differ: copy one over the other (cp) so both stay byte-identical")
	}
}

// TestAddonsManifestsStillValidate walks every packages/*/manifest.json of a
// checkout of asteby-hq/addons (ADDONS_DIR, or a sibling ../addons checkout)
// and requires each to keep validating in strict mode: the R2 schema package is
// additive. Skipped when no checkout is available.
func TestAddonsManifestsStillValidate(t *testing.T) {
	dir := os.Getenv("ADDONS_DIR")
	if dir == "" {
		dir = "../../../addons"
	}
	files, _ := filepath.Glob(filepath.Join(dir, "packages", "*", "manifest.json"))
	if len(files) == 0 {
		t.Skipf("no addons checkout at %s (set ADDONS_DIR)", dir)
	}
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if err := Validate(raw); err != nil {
			t.Errorf("%s no longer validates: %v", f, err)
		}
	}
	t.Logf("validated %d addon manifests", len(files))
}

// The schema already refuses unknown enums on the v3 plane; the shared Go rules
// are what the legacy carrier runs, so they carry the same clear messages.
func TestValidateInject_GoMessages(t *testing.T) {
	env := InjectEnv{AddonKey: "shipping", Cols: map[string]map[string]struct{}{"Shipment": {"status": {}, "sales_order_id": {}}}}
	three := 3
	cases := []struct {
		name string
		in   Inject
		want string
	}{
		{"unknown kind", Inject{Key: "a", Kind: "sparkle", Target: "@commerce.order"}, `kind "sparkle" is not one of panel|tab|column|badge|filter|widget`},
		{"slot outside catalog", Inject{Key: "a", Kind: "panel", Target: "@commerce.order", Slot: "record.footer", Panel: &DetailPanel{}}, `slot "record.footer" is not in the slots@1 catalog`},
		{"order below 200", Inject{Key: "a", Kind: "filter", Target: "@commerce.order", Order: &three, Filter: &InjectFilter{Column: "x"}}, "order 3 is below 200"},
		{"bad tone", Inject{Key: "a", Kind: "badge", Target: "@commerce.order", Badge: &InjectBadge{From: "Shipment", ForeignKey: "sales_order_id", Field: "status", Values: map[string]string{"x": "purple"}}}, `"purple" is not one of`},
		{"target format", Inject{Key: "a", Kind: "filter", Target: "commerce.order", Filter: &InjectFilter{Column: "x"}}, `must be a concrete "<addon>.<Model>"`},
	}
	for _, c := range cases {
		errs := ValidateInject(env, []Inject{c.in}, nil)
		if !strings.Contains(strings.Join(errs, "\n"), c.want) {
			t.Errorf("%s: errors %q do not contain %q", c.name, errs, c.want)
		}
	}
}
