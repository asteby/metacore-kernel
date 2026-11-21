package manifest

import (
	"encoding/json"
	"strings"
	"testing"

	v3 "github.com/asteby/metacore-kernel/manifest/v3"
)

type roleCat struct {
	roles     map[string]string
	contracts map[string][]string
}

func (c roleCat) RoleOf(k string) (string, bool) { r, ok := c.roles[k]; return r, ok }
func (c roleCat) WritableTables(k string) ([]string, bool) {
	m, ok := c.contracts[k]
	return m, ok
}

func TestLegacyValidator_RejectsWhatV3Rejects(t *testing.T) {
	cases := map[string]struct {
		d    map[string]any
		want string
	}{
		"R-D provider navigation": {doc("acme_pay", "provider", func(m map[string]any) {
			provider(m)
			m["contributions"] = rm("navigation", []any{rm("title", "Pay", "items", []any{})})
		}), "R-D"},
		"R-C capability outside contract": {doc("acme_pay", "provider", func(m map[string]any) {
			provider(m)
			m["provides_capabilities"] = []any{rm("key", "other.thing.send", "handler", rm("type", "wasm", "function", "do_send"))}
		}), "R-C"},
		"provider without implements": {doc("acme_pay", "provider", nil), "implements"},
		"extends without role extension": {doc("acme_pay", "provider", func(m map[string]any) {
			provider(m)
			m["extends_addon"] = rm("key", "acme_core", "version", "^1")
		}), "extends_addon requires"},
	}
	for name, c := range cases {
		// v3 side: parse non-strict so the legacy projection can be built from a
		// manifest the v3 validator would reject; assert both reject it.
		raw, _ := json.Marshal(c.d)
		if err := v3.Validate(raw); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: v3 validator: want %q, got %v", name, c.want, err)
		}
		m3 := &v3.Manifest{}
		if err := json.Unmarshal(raw, m3); err != nil {
			t.Fatal(err)
		}
		legacy := FromV3(m3)
		if err := legacy.Validate("3.1.0"); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: legacy validator: want %q, got %v", name, c.want, err)
		}
	}
}

func TestLegacyManifest_CheckRolesWithCatalog(t *testing.T) {
	cat := roleCat{
		roles:     map[string]string{"acme_pay": "provider"},
		contracts: map[string][]string{"acme.document": {"doc_records"}},
	}
	app := legacyFromV3(t, doc("acme_desk", "app", func(m map[string]any) {
		m["compatibility"].(map[string]any)["requires"] = []any{rm("key", "kernel", "version", ">=3.0.0 <4.0.0"), rm("key", "acme_pay", "version", "^1")}
	}))
	if errs, _ := app.CheckRoles(cat); len(errs) != 1 || !strings.Contains(errs[0], "R-A") {
		t.Fatalf("R-A via legacy: got %v", errs)
	}
	if errs, _ := app.CheckRoles(nil); len(errs) != 0 {
		t.Fatalf("no catalog, no R-A: %v", errs)
	}
	prov := legacyFromV3(t, doc("acme_pay", "provider", func(m map[string]any) {
		provider(m)
		m["capabilities"] = []any{rm("kind", "db:write", "target", "addon_acme_pay.*"), rm("kind", "db:write", "target", "customers")}
	}))
	if errs, _ := prov.CheckRoles(cat); len(errs) != 1 || !strings.Contains(errs[0], "R-B") {
		t.Fatalf("R-B via legacy: got %v", errs)
	}
}
