package v3

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// uiExample loads the repo's UI example manifest (shipping) as a mutable map.
func uiExample(t *testing.T) map[string]any {
	t.Helper()
	b, err := os.ReadFile("../../docs/spec/v3/examples/addon-ui-example.json")
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func obj(v any) map[string]any { return v.(map[string]any) }
func arr(v any) []any          { return v.([]any) }

func navItems(m map[string]any) []any {
	return arr(obj(arr(obj(m["contributions"])["navigation"])[0])["items"])
}

// navItem returns the nav entry with that view_type.
func navItem(t *testing.T, m map[string]any, viewType string) map[string]any {
	t.Helper()
	for _, it := range navItems(m) {
		if obj(it)["view_type"] == viewType {
			return obj(it)
		}
	}
	t.Fatalf("no nav item with view_type %q", viewType)
	return nil
}

func panels(m map[string]any) []any {
	return arr(obj(obj(arr(m["models"])[0])["detail"])["panels"])
}

func panel(t *testing.T, m map[string]any, key string) map[string]any {
	t.Helper()
	for _, p := range panels(m) {
		if obj(p)["key"] == key {
			return obj(p)
		}
	}
	t.Fatalf("no panel %q", key)
	return nil
}

func page0(m map[string]any) map[string]any {
	return obj(arr(obj(m["frontend"])["pages"])[0])
}

func TestUIExampleIsValid(t *testing.T) {
	if err := Validate(mustJSON(t, uiExample(t))); err != nil {
		t.Fatalf("UI example must validate: %v", err)
	}
}

func TestUIValidation(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(t *testing.T, m map[string]any)
		want   string // substring of the error; "" = valid
	}{
		// ---- nav views
		{"map without view.map", func(t *testing.T, m map[string]any) {
			delete(navItem(t, m, "map"), "view")
		}, "declares view_type=map but no view.map"},
		{"view block not matching view_type", func(t *testing.T, m map[string]any) {
			cal := navItem(t, m, "calendar")
			obj(cal["view"])["map"] = map[string]any{"geometry": "dest_postal_code"}
		}, `view.map is declared but view_type is "calendar"; only view.calendar is read`},
		{"view on a table entry", func(t *testing.T, m map[string]any) {
			it := navItem(t, m, "kanban")
			it["view"] = map[string]any{"split": map[string]any{}}
		}, `view is only read for view_type map|timeline|calendar|chart|split (view_type is "kanban")`},
		{"declarative view without model", func(t *testing.T, m map[string]any) {
			delete(navItem(t, m, "calendar"), "model")
		}, "declares view_type=calendar but no model"},
		{"unknown view_type", func(t *testing.T, m map[string]any) {
			navItem(t, m, "calendar")["view_type"] = "gantt"
		}, "view_type"},
		{"map point and geometry", func(t *testing.T, m map[string]any) {
			obj(obj(navItem(t, m, "map")["view"])["map"])["geometry"] = "dest_postal_code"
		}, "declares both point and geometry; use one"},
		{"map without coordinates", func(t *testing.T, m map[string]any) {
			delete(obj(obj(navItem(t, m, "map")["view"])["map"]), "point")
		}, "needs point {lat, lng} or geometry"},
		{"map unknown column", func(t *testing.T, m map[string]any) {
			mv := obj(obj(navItem(t, m, "map")["view"])["map"])
			obj(mv["point"])["lat"] = "latitude"
		}, `view.map.point.lat "latitude" is not a column of model "Shipment"`},
		{"map popup unknown column", func(t *testing.T, m map[string]any) {
			mv := obj(obj(navItem(t, m, "map")["view"])["map"])
			mv["popup"] = []any{"tracking_number", "weight"}
		}, `view.map.popup[1] "weight" is not a column of model "Shipment"`},
		{"map route on a nav entry", func(t *testing.T, m map[string]any) {
			mv := obj(obj(navItem(t, m, "map")["view"])["map"])
			mv["route"] = map[string]any{"relation": "events", "point": map[string]any{"lat": "lat", "lng": "lng"}, "order_by": "at"}
		}, "route is only valid in a detail panel"},
		{"map zones layer without geometry", func(t *testing.T, m map[string]any) {
			mv := obj(obj(navItem(t, m, "map")["view"])["map"])
			mv["layers"] = []any{map[string]any{"kind": "zones", "model": "shipping_flat.Zone"}}
		}, "is kind=zones and needs geometry"},
		{"map popup over the limit", func(t *testing.T, m map[string]any) {
			mv := obj(obj(navItem(t, m, "map")["view"])["map"])
			mv["popup"] = []any{"a", "b", "c", "d", "e", "f", "g", "h", "i"}
		}, "popup"},
		{"kernel-owned columns are accepted", func(t *testing.T, m map[string]any) {
			cv := obj(obj(navItem(t, m, "calendar")["view"])["calendar"])
			cv["start_field"] = "created_at"
		}, ""},
		{"timeline nav with source", func(t *testing.T, m map[string]any) {
			tv := obj(obj(navItem(t, m, "timeline")["view"])["timeline"])
			tv["source"] = map[string]any{"relation": "events"}
		}, "source is only valid in a detail panel"},
		{"chart over another model", func(t *testing.T, m map[string]any) {
			cv := obj(obj(navItem(t, m, "chart")["view"])["chart"])
			obj(cv["query"])["model"] = "ShipmentEvent"
		}, `query.model "ShipmentEvent" must be the nav entry's model "Shipment"`},
		{"chart reuses the widget query rules", func(t *testing.T, m map[string]any) {
			cv := obj(obj(navItem(t, m, "chart")["view"])["chart"])
			delete(obj(cv["query"]), "group_by")
		}, "query.group_by is required for kind=bar"},
		{"split column unknown", func(t *testing.T, m map[string]any) {
			sv := obj(obj(navItem(t, m, "split")["view"])["split"])
			sv["columns"] = []any{"weight"}
		}, `view.split.columns[0] "weight" is not a column of model "Shipment"`},

		// ---- detail panels
		{"timeline panel without source", func(t *testing.T, m map[string]any) {
			delete(obj(panel(t, m, "tracking")["timeline"]), "source")
		}, "source is required in a detail panel"},
		{"timeline capability items not an array", func(t *testing.T, m map[string]any) {
			src := obj(obj(panel(t, m, "tracking")["timeline"])["source"])
			src["items"] = "status"
		}, `items "status" is not an array of objects in the output of "commerce.shipping.track"`},
		{"timeline field missing from the contract items", func(t *testing.T, m map[string]any) {
			obj(panel(t, m, "tracking")["timeline"])["title_field"] = "summary"
		}, `title_field "summary" is not a field of commerce.shipping.track events[]`},
		{"timeline input from payload", func(t *testing.T, m map[string]any) {
			src := obj(obj(panel(t, m, "tracking")["timeline"])["source"])
			obj(src["input"])["carrier"] = "payload.carrier"
		}, "a timeline reads the record; use record.<column> or const:<literal>"},
		{"timeline input missing a required contract field", func(t *testing.T, m map[string]any) {
			src := obj(obj(panel(t, m, "tracking")["timeline"])["source"])
			delete(obj(src["input"]), "tracking_number")
		}, `input must map required field "tracking_number" of capability "commerce.shipping.track"`},
		{"timeline input key outside the contract", func(t *testing.T, m map[string]any) {
			src := obj(obj(panel(t, m, "tracking")["timeline"])["source"])
			obj(src["input"])["foo"] = "record.carrier"
		}, `input key "foo" is not a field of capability "commerce.shipping.track"`},
		{"timeline input from an unknown column", func(t *testing.T, m map[string]any) {
			src := obj(obj(panel(t, m, "tracking")["timeline"])["source"])
			obj(src["input"])["carrier"] = "record.courier"
		}, `"courier" is not a column of model "Shipment"`},
		{"timeline over an action contract", func(t *testing.T, m map[string]any) {
			src := obj(obj(panel(t, m, "tracking")["timeline"])["source"])
			src["capability"] = "messaging.whatsapp.send"
			src["input"] = map[string]any{"to": "record.carrier"}
		}, "is an action contract; a timeline needs a query contract"},
		{"timeline from a relation checks child columns", func(t *testing.T, m map[string]any) {
			tv := obj(panel(t, m, "tracking")["timeline"])
			tv["source"] = map[string]any{"relation": "events"}
			tv["title_field"] = "headline"
		}, `title_field "headline" is not a column of model "ShipmentEvent"`},
		{"relation panel unknown relation", func(t *testing.T, m map[string]any) {
			obj(panel(t, m, "history")["relation"])["name"] = "scans"
		}, `relation.name "scans" is not a relation of model "Shipment"`},
		{"relation panel unknown child column", func(t *testing.T, m map[string]any) {
			obj(panel(t, m, "history")["relation"])["columns"] = []any{"weight"}
		}, `"weight" is not a column of model "ShipmentEvent"`},
		{"panel block not matching kind", func(t *testing.T, m map[string]any) {
			panel(t, m, "figures")["map"] = map[string]any{"geometry": "dest_postal_code"}
		}, `declares map but kind is "stats"`},
		{"panel kind without its block", func(t *testing.T, m map[string]any) {
			delete(panel(t, m, "route"), "map")
		}, "is kind=map and needs map"},
		{"duplicate panel key", func(t *testing.T, m map[string]any) {
			panel(t, m, "figures")["key"] = "route"
		}, `key "route" is already used by`},
		{"stats unknown field", func(t *testing.T, m map[string]any) {
			items := arr(obj(panel(t, m, "figures")["stats"])["items"])
			obj(items[0])["field"] = "weight"
		}, `stats.items[0].field "weight" is not a column of model "Shipment"`},
		{"custom panel needs a federation frontend", func(t *testing.T, m map[string]any) {
			delete(m, "frontend")
			navItems(m)[len(navItems(m))-1] = map[string]any{"title": "x", "url": "/x"}
			panel(t, m, "figures")["kind"] = "custom"
			delete(panel(t, m, "figures"), "stats")
			panel(t, m, "figures")["expose"] = "./panels/figures"
		}, "is kind=custom and requires a top-level frontend block with format=federation"},
		{"map panel route only", func(t *testing.T, m map[string]any) {
			delete(obj(panel(t, m, "route")["map"]), "point")
		}, ""},
		{"map panel route unknown child column", func(t *testing.T, m map[string]any) {
			r := obj(obj(panel(t, m, "route")["map"])["route"])
			r["order_by"] = "scanned_at"
		}, `route.order_by "scanned_at" is not a column of model "ShipmentEvent"`},
		{"chart panel without relation", func(t *testing.T, m map[string]any) {
			p := panel(t, m, "figures")
			delete(p, "stats")
			p["kind"] = "chart"
			p["chart"] = map[string]any{"kind": "bar", "query": map[string]any{"model": "ShipmentEvent", "group_by": "status"}}
		}, "relation is required in a detail panel"},
		{"chart panel query on the wrong model", func(t *testing.T, m map[string]any) {
			p := panel(t, m, "figures")
			delete(p, "stats")
			p["kind"] = "chart"
			p["chart"] = map[string]any{"kind": "bar", "relation": "events", "query": map[string]any{"model": "Shipment", "group_by": "status"}}
		}, `query.model "Shipment" must be "ShipmentEvent", the child model of relation "events"`},

		// ---- frontend pages
		{"pages need federation", func(t *testing.T, m map[string]any) {
			obj(m["frontend"])["format"] = "script"
		}, "frontend.pages requires frontend.format=federation"},
		{"page reuses the register expose", func(t *testing.T, m map[string]any) {
			page0(m)["expose"] = "./register"
		}, "reuses frontend.expose"},
		{"duplicate page path", func(t *testing.T, m map[string]any) {
			pages := obj(m["frontend"])["pages"].([]any)
			dup := map[string]any{"key": "other", "path": "control-tower", "expose": "./pages/other", "title": "x"}
			obj(m["frontend"])["pages"] = append(pages, dup)
		}, `frontend.pages[1].path "control-tower" is already used by frontend.pages[0]`},
		{"wire keeps shell|immersive (no alias)", func(t *testing.T, m map[string]any) {
			page0(m)["layout"] = "embedded"
		}, "layout"},
		{"page path with leading slash", func(t *testing.T, m map[string]any) {
			page0(m)["path"] = "/control-tower"
		}, "path"},
		{"unknown share", func(t *testing.T, m map[string]any) {
			page0(m)["shares"] = []any{"leaflet"}
		}, "shares"},
		{"budget over the limit", func(t *testing.T, m map[string]any) {
			page0(m)["budget_kb"] = 400
		}, "budget_kb"},
		{"invalid frontend.sdk", func(t *testing.T, m map[string]any) {
			obj(m["frontend"])["sdk"] = "banana"
		}, `frontend.sdk "banana" is not a valid semver range`},
		{"immersive page without layout inherits", func(t *testing.T, m map[string]any) {
			delete(page0(m), "layout")
			obj(m["frontend"])["layout"] = "immersive"
		}, ""},
		{"host capability names are validated", func(t *testing.T, m map[string]any) {
			obj(m["compatibility"])["host_capabilities"] = []any{"UI Maps"}
		}, "host_capabilities"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := uiExample(t)
			tc.mutate(t, m)
			err := Validate(mustJSON(t, m))
			if tc.want == "" {
				if err != nil {
					t.Fatalf("expected valid, got %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected an error containing %q, got nil", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("expected error containing %q, got:\n%v", tc.want, err)
			}
		})
	}
}

// The Go rules behind the schema enums give the clear messages the legacy
// carrier (no JSON schema) surfaces.
func TestValidateFrontendUIMessages(t *testing.T) {
	pages := []FrontendPage{
		{Key: "a", Path: "a", Expose: "./pages/a", Title: "t", Layout: "embedded", Shares: []string{"leaflet", "recharts", "recharts"}, BudgetKB: 400},
		{Key: "a", Path: "/b", Expose: "pages/b", Title: ""},
	}
	got := strings.Join(ValidateFrontendUI("federation", "./register", "", pages), "\n")
	for _, want := range []string{
		`frontend.pages[0].layout "embedded" is not one of shell|immersive (shell = embedded in the host chrome, immersive = full screen)`,
		`frontend.pages[0].shares[0] "leaflet" is not a host-provided share (want one of @asteby/metacore-charts|@asteby/metacore-maps|maplibre-gl|recharts)`,
		`frontend.pages[0].shares[2] "recharts" is duplicated`,
		`frontend.pages[0].budget_kb 400 is out of range: 1..150 KB gzip`,
		`frontend.pages[1].key "a" is already used by frontend.pages[0]`,
		`frontend.pages[1].path "/b" must be lowercase kebab-case segments without a leading slash`,
		`frontend.pages[1].expose "pages/b" must be a federation module name`,
		`frontend.pages[1].title is required`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if errs := ValidateFrontendUI("script", "", "", nil); len(errs) != 0 {
		t.Fatalf("no pages, no errors: %v", errs)
	}
}

func TestFrontendPageEffectiveLayout(t *testing.T) {
	cases := []struct{ page, addon, want string }{
		{"", "", PageLayoutShell},
		{"", PageLayoutImmersive, PageLayoutImmersive},
		{PageLayoutShell, PageLayoutImmersive, PageLayoutShell},
		{PageLayoutImmersive, "", PageLayoutImmersive},
	}
	for _, c := range cases {
		if got := (FrontendPage{Layout: c.page}).EffectiveLayout(c.addon); got != c.want {
			t.Errorf("page %q addon %q: got %q want %q", c.page, c.addon, got, c.want)
		}
	}
}

// Manifests published before the UI grammar keep validating: the base fixture
// of this package has no frontend pages, views or panels, and a kanban/table
// nav entry without view is untouched.
func TestUIGrammarIsAdditive(t *testing.T) {
	m := baseValid()
	m["frontend"] = map[string]any{"entry": "/x/remoteEntry.js", "format": "federation", "expose": "./register", "layout": "immersive"}
	m["contributions"] = map[string]any{"navigation": []any{map[string]any{
		"title": "g",
		"items": []any{
			map[string]any{"title": "a", "url": "addon://inventory/pos"},
			map[string]any{"title": "b", "model": "Product", "view_type": "table"},
		},
	}}}
	if err := Validate(mustJSON(t, m)); err != nil {
		t.Fatalf("pre-existing shape must stay valid: %v", err)
	}
}
