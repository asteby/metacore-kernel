package manifest_test

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/asteby/metacore-kernel/manifest"
	v3 "github.com/asteby/metacore-kernel/manifest/v3"
)

// shippingUI parses the repo's UI example (docs/spec/v3/examples) through the
// real chain: v3.Parse (schema + cross-field) → FromV3 (host carrier).
func shippingUI(t *testing.T) manifest.Manifest {
	t.Helper()
	raw, err := os.ReadFile("../docs/spec/v3/examples/addon-ui-example.json")
	if err != nil {
		t.Fatal(err)
	}
	m, err := v3.Parse(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return manifest.FromV3(m)
}

func TestFromV3_ProjectsUISurfaces(t *testing.T) {
	m := shippingUI(t)
	if err := m.Validate(manifest.APIVersion); err != nil {
		t.Fatalf("projected manifest must pass the legacy validator: %v", err)
	}
	if m.Frontend == nil || m.Frontend.SDK != "^4.0.0" || len(m.Frontend.Pages) != 1 {
		t.Fatalf("frontend sdk/pages not projected: %+v", m.Frontend)
	}
	p := m.Frontend.Page("/control-tower/")
	if p == nil || p.Expose != "./pages/control-tower" || p.EffectiveLayout(m.Frontend.Layout) != v3.PageLayoutShell {
		t.Fatalf("page lookup: %+v", p)
	}
	if m.Frontend.Page("nope") != nil {
		t.Fatal("unknown path must be nil")
	}
	if !reflect.DeepEqual(m.HostCapabilities, []string{"ui.maps"}) {
		t.Fatalf("compatibility.host_capabilities not projected: %v", m.HostCapabilities)
	}
	if v := m.NavView("Shipment", "map"); v == nil || v.Map == nil || v.Map.Point.Lat != "dest_lat" || !v.Map.Cluster {
		t.Fatalf("NavView(Shipment, map) = %+v", v)
	}
	if m.NavView("Shipment", "timeline") != nil {
		t.Fatal("no timeline view on Shipment")
	}
	var detail *v3.ModelDetail
	for _, d := range m.ModelDefinitions {
		if d.ModelKey == "Shipment" {
			detail = d.Detail
		}
	}
	if detail == nil || len(detail.Panels) != 4 || detail.Panels[1].Timeline.Source.Capability != "commerce.shipping.track" {
		t.Fatalf("detail panels not projected: %+v", detail)
	}
}

func TestRequiredHostCapabilities_ImpliedByUI(t *testing.T) {
	m := shippingUI(t)
	want := []string{
		"ui.detail_panels",
		"ui.embedded_pages",
		"ui.maps",
		"ui.pages",
		"ui.views.calendar",
		"ui.views.chart",
		"ui.views.map",
		"ui.views.split",
		"ui.views.timeline",
	}
	if got := m.RequiredHostCapabilities(); !reflect.DeepEqual(got, want) {
		t.Fatalf("RequiredHostCapabilities =\n%v\nwant\n%v", got, want)
	}

	// An immersive-only page needs ui.pages but not ui.embedded_pages.
	m.Frontend.Layout = "immersive"
	m.Frontend.Pages[0].Layout = ""
	for _, c := range m.RequiredHostCapabilities() {
		if c == manifest.HostCapUIEmbeddedPages {
			t.Fatal("immersive page must not imply ui.embedded_pages")
		}
	}
}

func TestEvaluateCompatibility_UICapabilities(t *testing.T) {
	m := shippingUI(t)
	m.Kernel = ""
	full := manifest.HostCapabilityProfile{SDKVersion: "4.2.0", Capabilities: m.RequiredHostCapabilities()}
	if r := m.EvaluateCompatibility(full); !r.Compatible {
		t.Fatalf("host with every capability must be compatible: %v", r.Reasons())
	}

	// A host from before the UI grammar (no ui.* capabilities) refuses the
	// install with one reason per missing capability, naming what needs it.
	old := manifest.HostCapabilityProfile{SDKVersion: "4.2.0", Capabilities: []string{"wasm-runtime"}}
	r := m.EvaluateCompatibility(old)
	if r.Compatible || len(r.Issues) != 9 {
		t.Fatalf("want 9 missing capabilities, got %v", r.Reasons())
	}
	reasons := strings.Join(r.Reasons(), "\n")
	for _, want := range []string{
		`manifest uses navigation view_type "map", which needs host capability "ui.views.map"; this host does not provide it`,
		`manifest uses frontend.pages "control_tower" (layout shell), which needs host capability "ui.embedded_pages"`,
		`manifest uses model Shipment detail panels, which needs host capability "ui.detail_panels"`,
		// ui.maps is declared explicitly: the declared-capability message wins.
		`manifest.host_capabilities: host is missing required capability "ui.maps"`,
	} {
		if !strings.Contains(reasons, want) {
			t.Errorf("missing reason %q in:\n%s", want, reasons)
		}
	}
	for _, iss := range r.Issues {
		if iss.Kind != manifest.IssueMissingCapability || iss.Field != "host_capabilities" || iss.Capability == "" {
			t.Fatalf("unexpected issue %+v", iss)
		}
	}

	if err := m.CheckHostCapabilities([]string{"ui.maps"}); err == nil || !strings.Contains(err.Error(), "ui.views.map") {
		t.Fatalf("CheckHostCapabilities must list the missing ones, got %v", err)
	}
	if err := m.CheckHostCapabilities(full.Capabilities); err != nil {
		t.Fatalf("CheckHostCapabilities with everything: %v", err)
	}
}

func TestEvaluateCompatibility_FrontendSDK(t *testing.T) {
	m := &manifest.Manifest{Frontend: &manifest.FrontendSpec{Format: "federation", SDK: "^4.0.0"}}
	cases := []struct {
		name     string
		host     string
		kind     manifest.CompatibilityIssueKind
		contains string
	}{
		{"satisfied", "4.3.1", "", ""},
		{"too old", "3.9.0", manifest.IssueSDKRange, "needs @asteby/metacore-sdk ^4.0.0; this host exposes 3.9.0"},
		{"not reported fails closed", "", manifest.IssueInvalidRange, "the host does not report its SDK version"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := m.EvaluateCompatibility(manifest.HostCapabilityProfile{SDKVersion: tc.host})
			if tc.kind == "" {
				if !r.Compatible {
					t.Fatalf("want compatible, got %v", r.Reasons())
				}
				return
			}
			if r.Compatible || len(r.Issues) != 1 || r.Issues[0].Kind != tc.kind || r.Issues[0].Field != "frontend.sdk" || !strings.Contains(r.Issues[0].Detail, tc.contains) {
				t.Fatalf("got %+v", r.Issues)
			}
		})
	}
}

// Manifests without UI surfaces imply nothing: existing hosts that pass an
// empty capability set keep installing them.
func TestRequiredHostCapabilities_LegacyManifestImpliesNothing(t *testing.T) {
	m := &manifest.Manifest{
		Key: "pos", Name: "POS", Version: "1.0.0",
		Frontend: &manifest.FrontendSpec{Format: "federation", Layout: "immersive", Expose: "./register"},
		Navigation: []manifest.NavGroup{{Title: "g", Items: []manifest.NavItem{
			{Title: "a", Model: "Order", ViewType: "kanban", GroupBy: "status"},
			{Title: "b", URL: "addon://pos/terminal"},
			{Title: "c", Model: "Order", ViewType: "standard"}, // legacy free-form value, untouched
		}}},
	}
	if got := m.RequiredHostCapabilities(); len(got) != 0 {
		t.Fatalf("want none, got %v", got)
	}
	if r := m.EvaluateCompatibility(manifest.HostCapabilityProfile{}); !r.Compatible {
		t.Fatalf("legacy manifest must stay compatible: %v", r.Reasons())
	}
	if err := m.Validate(manifest.APIVersion); err != nil {
		t.Fatalf("legacy manifest must stay valid: %v", err)
	}
}

func TestLegacyValidate_UIShape(t *testing.T) {
	cases := []struct {
		name string
		m    manifest.Manifest
		want string
	}{
		{"page needs federation", manifest.Manifest{Frontend: &manifest.FrontendSpec{Format: "script", Pages: []v3.FrontendPage{{Key: "a", Path: "a", Expose: "./pages/a", Title: "t"}}}},
			"manifest.frontend.pages requires frontend.format=federation"},
		{"page layout alias rejected", manifest.Manifest{Frontend: &manifest.FrontendSpec{Format: "federation", Pages: []v3.FrontendPage{{Key: "a", Path: "a", Expose: "./pages/a", Title: "t", Layout: "fullscreen"}}}},
			`manifest.frontend.pages[0].layout "fullscreen" is not one of shell|immersive`},
		{"invalid frontend.sdk", manifest.Manifest{Frontend: &manifest.FrontendSpec{Format: "federation", SDK: ">>4"}},
			`manifest.frontend.sdk ">>4" is not a valid semver range`},
		{"map view without block", manifest.Manifest{Navigation: []manifest.NavGroup{{Title: "g", Items: []manifest.NavItem{{Title: "m", Model: "Shipment", ViewType: "map"}}}}},
			"manifest.navigation[0].items[0] declares view_type=map but no view.map"},
		{"custom panel without frontend", manifest.Manifest{ModelDefinitions: []manifest.ModelDefinition{{ModelKey: "Shipment", TableName: "shipments", Columns: []manifest.ColumnDef{{Name: "status", Type: "string"}}, Detail: &v3.ModelDetail{Panels: []v3.DetailPanel{{Key: "x", Kind: "custom", Title: "t", Expose: "./panels/x"}}}}}},
			"manifest.model_definitions[0].detail.panels[0] is kind=custom and requires a top-level frontend block with format=federation"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.m.Key, tc.m.Name, tc.m.Version = "demo", "Demo", "1.0.0"
			err := tc.m.Validate(manifest.APIVersion)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want error containing %q, got %v", tc.want, err)
			}
		})
	}
}

func TestLite_PagesSlimAndViewsDropped(t *testing.T) {
	m := shippingUI(t)
	lite := manifest.Lite(m)

	if lite.Frontend == nil || len(lite.Frontend.Pages) != 1 {
		t.Fatalf("pages must ride in the lite manifest: %+v", lite.Frontend)
	}
	p := lite.Frontend.Pages[0]
	if p.Key != "control_tower" || p.Path != "control-tower" || p.Expose != "./pages/control-tower" || p.Layout != "shell" || p.Title == "" || p.Permission == "" {
		t.Fatalf("routing fields lost: %+v", p)
	}
	if p.Requires != nil || p.Shares != nil || p.BudgetKB != 0 || p.Breadcrumbs != nil {
		t.Fatalf("heavy page fields must be dropped: %+v", p)
	}
	b, _ := json.Marshal(p)
	if len(b) > 300 {
		t.Fatalf("lite page is %d bytes (budget 300): %s", len(b), b)
	}

	for _, it := range lite.Navigation[0].Items {
		if it.View != nil {
			t.Fatalf("lite nav must not carry view blocks: %+v", it)
		}
	}
	if lite.Navigation[0].Items[1].ViewType != "map" {
		t.Fatal("lite nav keeps view_type")
	}
	// Lite is a copy: the full manifest keeps its views and page details.
	if m.Navigation[0].Items[1].View == nil || m.Frontend.Pages[0].Shares == nil {
		t.Fatal("Lite mutated its input")
	}
}
