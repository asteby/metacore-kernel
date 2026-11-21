package v3

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type fakeCat struct {
	roles     map[string]string
	contracts map[string][]string
}

func (c fakeCat) RoleOf(k string) (string, bool) { r, ok := c.roles[k]; return r, ok }
func (c fakeCat) WritableTables(k string) ([]string, bool) {
	m, ok := c.contracts[k]
	return m, ok
}

func TestRoles_DeclaredRoleViolationsAreErrors(t *testing.T) {
	cases := map[string]struct {
		m    map[string]interface{}
		want string
	}{
		"contracts on a foundation":   {roleDoc("acme_items", "foundation", func(m map[string]interface{}) { asContract(m) }), "requires metadata.role \"contract\""},
		"contract without contracts":  {roleDoc("acme_core", "contract", nil), "requires at least one contracts"},
		"implements on an app":        {roleDoc("acme_desk", "app", asProvider), "requires metadata.role \"provider\" or \"extension\""},
		"provider without implements": {roleDoc("acme_pay", "provider", nil), "requires at least one implements"},
		"extension without extends":   {roleDoc("acme_pay_term", "extension", asProvider), "requires extends_addon"},
		"extends on a provider": {roleDoc("acme_pay", "provider", func(m map[string]interface{}) {
			asProvider(m)
			m["extends_addon"] = mm("key", "acme_core", "version", "^1")
		}), "extends_addon requires metadata.role \"extension\""},
		"extends not in requires": {roleDoc("acme_pay_term", "extension", func(m map[string]interface{}) {
			asProvider(m)
			m["extends_addon"] = mm("key", "acme_other", "version", "^1")
		}), "must also be a"},
		"extends bad range": {roleDoc("acme_pay_term", "extension", func(m map[string]interface{}) {
			asProvider(m)
			m["extends_addon"] = mm("key", "acme_core", "version", "not-a-range")
		}), "extends_addon.version"},
		"preset with models": {roleDoc("acme_preset", "preset", func(m map[string]interface{}) { m["models"] = ownModel() }), "configuration only"},
		"replaces itself": {roleDoc("acme_pay", "provider", func(m map[string]interface{}) {
			asProvider(m)
			m["replaces"] = []interface{}{"acme_pay"}
		}), "cannot be the addon itself"},
		"R-D provider navigation": {roleDoc("acme_pay", "provider", func(m map[string]interface{}) {
			asProvider(m)
			m["contributions"] = mm("navigation", []interface{}{mm("title", "Pay", "items", []interface{}{})})
		}), "R-D"},
		"R-C capability outside contract": {roleDoc("acme_pay", "provider", func(m map[string]interface{}) {
			asProvider(m)
			m["provides_capabilities"] = []interface{}{mm("key", "other.thing.send", "handler", mm("type", "wasm", "function", "do_send"))}
		}), "R-C"},
		"contract bad version": {roleDoc("acme_core", "contract", func(m map[string]interface{}) {
			asContract(m)
			m["contracts"].([]interface{})[0].(map[string]interface{})["version"] = "one"
		}), "is not semver"},
		"contract duplicated": {roleDoc("acme_core", "contract", func(m map[string]interface{}) {
			asContract(m)
			c := m["contracts"].([]interface{})
			m["contracts"] = append(c, c[0])
		}), "duplicate contract key"},
		"contract states foreign model": {roleDoc("acme_core", "contract", func(m map[string]interface{}) {
			asContract(m)
			m["contracts"].([]interface{})[0].(map[string]interface{})["states"] = mm("model", "Nope", "field", "stage")
		}), "is not a model of this addon"},
		"contract writable foreign model": {roleDoc("acme_core", "contract", func(m map[string]interface{}) {
			asContract(m)
			m["contracts"].([]interface{})[0].(map[string]interface{})["writable_models"] = []interface{}{"Nope"}
		}), "writable_models"},
		"contract health unknown op": {roleDoc("acme_core", "contract", func(m map[string]interface{}) {
			asContract(m)
			m["contracts"].([]interface{})[0].(map[string]interface{})["health"] = mm("operation", "ghost")
		}), "health.operation"},
		"contract idempotent query": {roleDoc("acme_core", "contract", func(m map[string]interface{}) {
			asContract(m)
			ops := m["contracts"].([]interface{})[0].(map[string]interface{})["operations"].([]interface{})
			ops[1].(map[string]interface{})["idempotent"] = true
		}), "idempotent by nature"},
		"implements bad range": {roleDoc("acme_pay", "provider", func(m map[string]interface{}) {
			asProvider(m)
			m["implements"].([]interface{})[0].(map[string]interface{})["version"] = "nope"
		}), "implements[0]"},
		"implements twice": {roleDoc("acme_pay", "provider", func(m map[string]interface{}) {
			asProvider(m)
			im := m["implements"].([]interface{})
			m["implements"] = append(im, im[0])
		}), "implemented twice"},
	}
	for name, c := range cases {
		for _, strict := range []bool{true, false} {
			_, err := validateRole(t, c.m, Options{Strict: strict})
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("%s (strict=%v): want error containing %q, got %v", name, strict, c.want, err)
			}
		}
	}
}

func TestRoles_CatalogRules(t *testing.T) {
	cat := fakeCat{
		roles:     map[string]string{"acme_pay": "provider", "acme_core": "contract", "acme_items": "foundation"},
		contracts: map[string][]string{"acme.document": {"doc_records"}},
	}
	// R-A: a non-extension requiring a provider.
	app := roleDoc("acme_desk", "app", func(m map[string]interface{}) {
		m["compatibility"].(map[string]interface{})["requires"] = []interface{}{
			mm("key", "kernel", "version", ">=3.0.0 <4.0.0"), mm("key", "acme_pay", "version", "^1", "optional", true),
		}
	})
	if _, err := validateRole(t, app, Options{Strict: true, RoleCatalog: cat}); err == nil || !strings.Contains(err.Error(), "R-A") {
		t.Fatalf("R-A: want rejection, got %v", err)
	}
	if _, err := validateRole(t, app, Options{Strict: true}); err != nil {
		t.Fatalf("without a catalog R-A is not evaluated: %v", err)
	}
	// ... an extension may require a provider.
	ext := roleDoc("acme_pay_term", "extension", func(m map[string]interface{}) {
		asProvider(m)
		m["compatibility"].(map[string]interface{})["requires"] = []interface{}{
			mm("key", "kernel", "version", ">=3.0.0 <4.0.0"), mm("key", "acme_pay", "version", "^1"),
		}
		m["extends_addon"] = mm("key", "acme_pay", "version", "^1")
	})
	if _, err := validateRole(t, ext, Options{Strict: true, RoleCatalog: cat}); err != nil {
		t.Fatalf("an extension may require a provider: %v", err)
	}

	// R-B: provider writing a foreign table.
	foreign := func(target string) map[string]interface{} {
		return roleDoc("acme_pay", "provider", func(m map[string]interface{}) {
			asProvider(m)
			m["capabilities"] = []interface{}{mm("kind", "db:write", "target", "addon_acme_pay.*"), mm("kind", "db:write", "target", target)}
		})
	}
	if _, err := validateRole(t, foreign("customers"), Options{Strict: true, RoleCatalog: cat}); err == nil || !strings.Contains(err.Error(), "R-B") {
		t.Fatalf("R-B: want rejection, got %v", err)
	}
	if _, err := validateRole(t, foreign("doc_records"), Options{Strict: true, RoleCatalog: cat}); err != nil {
		t.Fatalf("a table of a model the contract opens is allowed: %v", err)
	}
	// The model KEY is not a table: capabilities name tables, so "Doc" does not
	// match the table doc_records (regression: keys and tables were compared).
	if _, err := validateRole(t, foreign("Doc"), Options{Strict: true, RoleCatalog: cat}); err == nil || !strings.Contains(err.Error(), "R-B") {
		t.Fatalf("a model key must not satisfy R-B, got %v", err)
	}
	warns, err := validateRole(t, foreign("customers"), Options{Strict: true})
	if err != nil || !strings.Contains(strings.Join(warns, "\n"), "["+RuleRoleProviderForeignWrite+"]") {
		t.Fatalf("without a catalog the foreign write warns: warns=%v err=%v", warns, err)
	}

	// Unknown implemented contract (catalog-checked).
	unk := roleDoc("acme_pay", "provider", func(m map[string]interface{}) {
		asProvider(m)
		m["implements"].([]interface{})[0].(map[string]interface{})["contract"] = "acme.ghost"
	})
	if _, err := validateRole(t, unk, Options{Strict: true, RoleCatalog: cat}); err == nil || !strings.Contains(err.Error(), "unknown contract") {
		t.Fatalf("want unknown contract, got %v", err)
	}
}

func TestRoles_InferredViolationsDowngradedToWarnings(t *testing.T) {
	m := roleDoc("acme_pay", "", func(m map[string]interface{}) {
		asProvider(m)
		m["contributions"] = mm("navigation", []interface{}{mm("title", "Pay", "items", []interface{}{})})
	})
	for _, strict := range []bool{true, false} {
		warns, err := validateRole(t, m, Options{Strict: strict})
		if err != nil {
			t.Fatalf("strict=%v: an undeclared role must not error: %v", strict, err)
		}
		joined := strings.Join(warns, "\n")
		if !strings.Contains(joined, "["+RuleRoleMissing+"]") || !strings.Contains(joined, `inferred "provider"`) {
			t.Errorf("strict=%v: want role_missing naming the inferred role, got %v", strict, warns)
		}
		if !strings.Contains(joined, "["+RuleRoleInferred+"] R-D") {
			t.Errorf("strict=%v: want the R-D finding downgraded to a warning, got %v", strict, warns)
		}
	}
}

// A nil catalog (hub/ops that do not pass one) must not panic and must skip
// R-A and the catalog half of R-B.
func TestRoles_NilCatalogNeverPanics(t *testing.T) {
	m := roleDoc("acme_pay", "provider", func(m map[string]interface{}) {
		asProvider(m)
		m["capabilities"] = []interface{}{mm("kind", "db:write", "target", "customers")}
		m["compatibility"].(map[string]interface{})["requires"] = []interface{}{
			mm("key", "kernel", "version", ">=3.0.0 <4.0.0"), mm("key", "acme_core", "version", "^1"), mm("key", "acme_other", "version", "^1"),
		}
	})
	warns, err := validateRole(t, m, Options{Strict: true, RoleCatalog: nil})
	if err != nil {
		t.Fatalf("nil catalog: %v", err)
	}
	if !strings.Contains(strings.Join(warns, "\n"), RuleRoleProviderForeignWrite) {
		t.Fatalf("want the foreign-write warning, got %v", warns)
	}
}

func TestRoles_RuleIDsAreWarnOnlyWindow(t *testing.T) {
	for _, r := range []string{RuleRoleInferred, RuleRoleProviderForeignWrite} {
		if _, ok := warnOnlyWindow[r]; !ok {
			t.Errorf("%s must be warn-only", r)
		}
		found := false
		for _, l := range LegacyTolerated() {
			if l == r {
				found = true
			}
		}
		if !found {
			t.Errorf("%s must be in legacyTolerated", r)
		}
	}
}

// Real addon manifests without a role keep passing the full rule set (no
// errors; nothing is enforced for an undeclared role).
func TestRoles_RealAddonManifestsPassRules(t *testing.T) {
	files, _ := filepath.Glob("testdata/roles/*.manifest.json")
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		m, _, err := ParseWithOptions(raw, Options{Strict: true, RoleCatalog: fakeCat{}})
		if err != nil {
			t.Errorf("%s: %v", f, err)
			continue
		}
		if errs, _ := CheckRoles(m.RoleFacts(), fakeCat{}); len(errs) != 0 {
			t.Errorf("%s: undeclared role must never error: %v", f, errs)
		}
	}
}

// The catalog a host builds from the owner's manifest: model keys resolve to
// TABLES, which is what a provider's db:write target names.
func TestRoles_ContractWritableTablesResolvesModelToTable(t *testing.T) {
	m := mustParseRole(t, roleDoc("acme_core", "contract", asContract))
	got := ContractWritableTables(m)["acme.document"]
	if len(got) != 1 || got[0] != "doc_records" {
		t.Fatalf("ContractWritableTables = %v, want [doc_records] (key Doc, table doc_records)", got)
	}
	// End to end: a provider writing that table passes with the derived catalog.
	cat := fakeCat{contracts: ContractWritableTables(m)}
	prov := roleDoc("acme_pay", "provider", func(d map[string]interface{}) {
		asProvider(d)
		d["capabilities"] = []interface{}{mm("kind", "db:write", "target", "addon_acme_pay.*"), mm("kind", "db:write", "target", "doc_records")}
	})
	if _, err := validateRole(t, prov, Options{Strict: true, RoleCatalog: cat}); err != nil {
		t.Fatalf("derived catalog must allow the opened table: %v", err)
	}
}

func mustParseRole(t *testing.T, d map[string]interface{}) *Manifest {
	t.Helper()
	m, _, err := ParseWithOptions(mustJSON(t, d), Options{})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// A provider's own namespace is "addon_<key>.", never "addon_<key>_": a
// provider "acme_pay" must not write the namespace of "acme_pay_term".
func TestRoles_OwnNamespaceIsDotFormOnly(t *testing.T) {
	cat := fakeCat{contracts: map[string][]string{"acme.document": {}}}
	d := roleDoc("acme_pay", "provider", func(m map[string]interface{}) {
		asProvider(m)
		m["capabilities"] = []interface{}{mm("kind", "db:write", "target", "addon_acme_pay_term.*")}
	})
	if _, err := validateRole(t, d, Options{Strict: true, RoleCatalog: cat}); err == nil || !strings.Contains(err.Error(), "R-B") {
		t.Fatalf("addon_acme_pay_term.* must not count as acme_pay's namespace, got %v", err)
	}
}

type typedNilCat struct{}

func (*typedNilCat) RoleOf(string) (string, bool)           { panic("typed nil catalog consulted") }
func (*typedNilCat) WritableTables(string) ([]string, bool) { panic("typed nil catalog consulted") }

// A typed nil pointer in the interface means "no catalog" and must not panic.
func TestRoles_TypedNilCatalogIsNoCatalog(t *testing.T) {
	var c *typedNilCat
	d := roleDoc("acme_desk", "app", func(m map[string]interface{}) {
		m["compatibility"].(map[string]interface{})["requires"] = []interface{}{
			mm("key", "kernel", "version", ">=3.0.0 <4.0.0"), mm("key", "acme_pay", "version", "^1"),
		}
	})
	if _, err := validateRole(t, d, Options{Strict: true, RoleCatalog: c}); err != nil {
		t.Fatalf("typed nil catalog: %v", err)
	}
	prov := roleDoc("acme_pay", "provider", asProvider)
	if _, err := validateRole(t, prov, Options{Strict: true, RoleCatalog: c}); err != nil {
		t.Fatalf("typed nil catalog on a provider: %v", err)
	}
}

// Real db:write targets come schema-qualified; the comparison is on the bare
// table, so "public.x" and "addon_<owner>.x" match the table a contract opens
// (regression: an exact comparison rejected them as foreign writes).
func TestRoles_QualifiedTargetsMatchBareTables(t *testing.T) {
	cat := fakeCat{contracts: map[string][]string{"acme.document": {"doc_records"}}}
	for _, target := range []string{"public.doc_records", "addon_acme_core.doc_records", "doc_records"} {
		d := roleDoc("acme_pay", "provider", func(m map[string]interface{}) {
			asProvider(m)
			m["capabilities"] = []interface{}{mm("kind", "db:write", "target", "addon_acme_pay.*"), mm("kind", "db:write", "target", target)}
		})
		if _, err := validateRole(t, d, Options{Strict: true, RoleCatalog: cat}); err != nil {
			t.Errorf("target %q must match the opened table: %v", target, err)
		}
	}
	// Still rejected: a different table, qualified or not.
	for _, target := range []string{"public.sales_orders", "addon_acme_core.other_table"} {
		d := roleDoc("acme_pay", "provider", func(m map[string]interface{}) {
			asProvider(m)
			m["capabilities"] = []interface{}{mm("kind", "db:write", "target", target)}
		})
		if _, err := validateRole(t, d, Options{Strict: true, RoleCatalog: cat}); err == nil || !strings.Contains(err.Error(), "R-B") {
			t.Errorf("target %q must still violate R-B, got %v", target, err)
		}
	}
	// The provider's own tables also match when qualified with public.
	own := roleDoc("acme_pay", "provider", func(m map[string]interface{}) {
		asProvider(m)
		m["models"] = []interface{}{mm("key", "Pay", "table", "pay_rows", "columns", []interface{}{
			mm("name", "id", "type", "uuid"), mm("name", "organization_id", "type", "uuid", "not_null", true),
		})}
		m["capabilities"] = []interface{}{mm("kind", "db:write", "target", "public.pay_rows")}
	})
	if _, err := validateRole(t, own, Options{Strict: true, RoleCatalog: cat}); err != nil {
		t.Errorf("public.<own table> must count as own: %v", err)
	}
}

// The Go validator repeats the schema's contract key shape (the legacy
// validator never sees the schema).
func TestRoles_ContractKeyShapeEnforcedInGo(t *testing.T) {
	f := RoleFacts{Key: "acme_core", Role: RoleContract, OwnModelKeys: []string{"Doc"},
		Contracts: []Contract{{Key: "nodot", Version: "1.0.0"}, {Key: "Bad.Key", Version: "1.0.0"}, {Key: "acme.ok", Version: "1.0.0"}}}
	errs, _ := CheckRoles(f, nil)
	joined := strings.Join(errs, "\n")
	if !strings.Contains(joined, "(nodot).key") || !strings.Contains(joined, "(Bad.Key).key") || strings.Contains(joined, "(acme.ok).key") {
		t.Fatalf("key shape errors wrong: %v", errs)
	}
}
