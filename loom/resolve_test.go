package loom

import (
	"encoding/json"
	"testing"

	v3 "github.com/asteby/metacore-kernel/manifest/v3"
)

func mf(t *testing.T, raw string) *v3.Manifest {
	t.Helper()
	var m v3.Manifest
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatal(err)
	}
	return &m
}

const fiscal = `{
  "metadata": { "key": "fiscal_mexico" },
  "frontend": { "entry": "/addons/fiscal_mexico/remoteEntry.js", "container": "fiscal_mexico", "integrity": "sha384-manifest" },
  "ui": {
    "exposes": [{ "id": "fiscal_mexico.csf-import", "version": "1.0.0", "expose": "./parts/csf-import", "self_review": true, "needs": { "records": { "find": ["customers.Customer"] } } }],
    "contributions": [
      { "id": "fiscal_mexico.csf-in-customer", "part": "fiscal_mexico.csf-import", "seam": "record.dialog.header",
        "requires": "^1", "order": 10,
        "when": { "model": ["Customer"], "mode": ["create"], "permission": ["customer.create"],
                  "setting": { "key": "fiscal_mexico.csf_reader", "equals": true },
                  "condition": { "addon_installed": "customers" } },
        "reads": ["values"], "writes": ["tax_id"] },
      { "id": "fiscal_mexico.stamp-hint", "part": "fiscal_mexico.csf-import", "seam": "record.form.section",
        "when": { "condition": { "connector_connected": "pac" } } },
      { "id": "fiscal_mexico.flagged", "part": "fiscal_mexico.csf-import", "seam": "record.form.section", "order": -1,
        "when": { "flag": "beta" } }
    ]
  },
  "contributions": { "slots": [{ "slot_kind": "record.panel", "entry": "./Panel", "permission": "invoice.read", "order": 5 }] }
}`

const other = `{
  "metadata": { "key": "pos" },
  "frontend": { "entry": "/addons/pos/remoteEntry.js" },
  "ui": { "contributions": [
    { "id": "pos.uses-csf", "part": "fiscal_mexico.csf-import", "seam": "record.dialog.header", "order": 10 },
    { "id": "pos.ghost", "part": "pos.missing", "seam": "record.dialog.header" }
  ] }
}`

func subject(perms ...string) Subject {
	set := map[string]bool{}
	for _, p := range perms {
		set[p] = true
	}
	return Subject{
		Can:                func(p string) bool { return set[p] },
		Setting:            func(k string) (any, bool) { return true, k == "fiscal_mexico.csf_reader" },
		Flag:               func(string) bool { return false },
		AddonInstalled:     func(k string) bool { return k == "customers" },
		ConnectorConnected: func(string, string) bool { return false },
	}
}

func ids(gs []Graft) []string {
	out := []string{}
	for _, g := range gs {
		out = append(out, g.ID)
	}
	return out
}

func reasons(ss []Skip) map[string]string {
	out := map[string]string{}
	for _, s := range ss {
		out[s.ID] = s.Reason
	}
	return out
}

func TestResolve_FiltersOrdersAndExplains(t *testing.T) {
	in := []Installed{{Manifest: mf(t, fiscal), Integrity: "sha384-pinned"}, {Manifest: mf(t, other)}}
	r := Resolve(in, subject("customer.create", "invoice.read"), PresetUI{}, Options{})

	want := []string{"fiscal_mexico.csf-in-customer", "pos.uses-csf", "fiscal_mexico.stamp-hint", "fiscal_mexico.slot.0"}
	if got := ids(r.Grafts); !equal(got, want) {
		t.Fatalf("grafts = %v, want %v", got, want)
	}
	g := r.Grafts[0]
	if len(g.Part.RecordsFind) != 1 || g.Part.RecordsFind[0] != "customers.Customer" || len(r.Grafts[3].Part.RecordsFind) != 0 {
		t.Fatalf("records_find must travel with the Part only: %#v", g.Part)
	}
	if !g.Part.SelfReview || r.Grafts[3].Part.SelfReview {
		t.Fatalf("self_review must travel with the Part only: %#v / %#v", g.Part, r.Grafts[3].Part)
	}
	if g.Part.Integrity != "sha384-pinned" || g.Part.Container != "fiscal_mexico" || g.Kind != "view" || g.Network != "none" {
		t.Fatalf("part/defaults: %#v", g)
	}
	if g.When == nil || g.When.Model[0] != "Customer" || g.When.Mode[0] != "create" {
		t.Fatalf("client-side clauses must travel: %#v", g.When)
	}
	// A graft of another addon reuses the exposing addon's Part (its bundle, its digest).
	if r.Grafts[1].Part.Addon != "fiscal_mexico" {
		t.Fatalf("foreign part: %#v", r.Grafts[1].Part)
	}
	if !r.Grafts[3].Legacy || r.Grafts[3].Part.Expose != "./Panel" {
		t.Fatalf("legacy slot projection: %#v", r.Grafts[3])
	}
	if r.Grafts[2].Inert != "connector_not_connected:pac" {
		t.Fatalf("unmet connector defaults to disable: %#v", r.Grafts[2])
	}
	if got := reasons(r.Skipped); got["fiscal_mexico.flagged"] != ReasonFlag || got["pos.ghost"] != ReasonPartMissing {
		t.Fatalf("skipped = %v", got)
	}
}

func TestResolve_ServerIsTheGate(t *testing.T) {
	in := []Installed{{Manifest: mf(t, fiscal)}}
	r := Resolve(in, subject(), PresetUI{}, Options{})
	got := reasons(r.Skipped)
	if got["fiscal_mexico.csf-in-customer"] != ReasonPermission || got["fiscal_mexico.slot.0"] != ReasonPermission {
		t.Fatalf("no permission must not be served: %v (served %v)", got, ids(r.Grafts))
	}
	sub := subject("customer.create")
	sub.Setting = func(string) (any, bool) { return false, true }
	r = Resolve(in, sub, PresetUI{}, Options{})
	if reasons(r.Skipped)["fiscal_mexico.csf-in-customer"] != ReasonSetting {
		t.Fatalf("setting off must not be served: %v", r.Skipped)
	}
	sub = subject("customer.create")
	sub.AddonInstalled = func(string) bool { return false }
	r = Resolve(in, sub, PresetUI{}, Options{})
	if reasons(r.Skipped)["fiscal_mexico.csf-in-customer"] != ReasonCondition {
		t.Fatalf("addon_installed unmet hides: %v", r.Skipped)
	}
}

func TestResolve_PresetAndSeamFilter(t *testing.T) {
	in := []Installed{{Manifest: mf(t, fiscal)}, {Manifest: mf(t, other)}}
	p := PresetUI{Disabled: map[string]bool{"pos.uses-csf": true}, Order: map[string]int{"fiscal_mexico.csf-in-customer": 99}}
	r := Resolve(in, subject("customer.create"), p, Options{Seams: []string{"record.dialog.header"}})
	if got := ids(r.Grafts); !equal(got, []string{"fiscal_mexico.csf-in-customer"}) || r.Grafts[0].Order != 99 {
		t.Fatalf("grafts = %v", r.Grafts)
	}
	if reasons(r.Skipped)["pos.uses-csf"] != ReasonPreset {
		t.Fatalf("skipped = %v", r.Skipped)
	}
	for _, s := range r.Skipped {
		if s.Seam != "record.dialog.header" {
			t.Fatalf("other seams must not be explained: %v", s)
		}
	}
}

func TestResolve_EmptyIsArray(t *testing.T) {
	b, _ := json.Marshal(Resolve(nil, Subject{}, PresetUI{}, Options{}))
	if string(b) != `{"grafts":[]}` {
		t.Fatalf("got %s", b)
	}
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
