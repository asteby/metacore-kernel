package v3

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func mm(kv ...interface{}) map[string]interface{} {
	out := map[string]interface{}{}
	for i := 0; i+1 < len(kv); i += 2 {
		out[kv[i].(string)] = kv[i+1]
	}
	return out
}

// roleDoc builds a valid manifest for addon `key` with the given role ("" =
// undeclared) and lets the caller mutate it.
func roleDoc(key, role string, mut func(m map[string]interface{})) map[string]interface{} {
	m := baseValid()
	md := m["metadata"].(map[string]interface{})
	md["key"] = key
	if role != "" {
		md["role"] = role
	}
	m["capabilities"] = []interface{}{mm("kind", "db:write", "target", "addon_"+key+".*")}
	if mut != nil {
		mut(m)
	}
	return m
}

func ownModel() []interface{} {
	return []interface{}{mm("key", "Doc", "table", "docs", "columns", []interface{}{
		mm("name", "id", "type", "uuid"), mm("name", "organization_id", "type", "uuid", "not_null", true),
	})}
}

func contractBlock() []interface{} {
	return []interface{}{mm(
		"key", "acme.document", "version", "1.0.0",
		"operations", []interface{}{
			mm("key", "issue", "kind", "action", "idempotent", true, "failover", "never"),
			mm("key", "status", "kind", "query", "failover", "allowed"),
		},
		"events", []interface{}{mm("name", "acme.document.settled", "version", 1)},
		"states", mm("model", "Doc", "field", "stage", "indeterminate", "pending"),
		"writable_models", []interface{}{"Doc"},
		"provider_settings", []interface{}{mm("key", "environment", "type", "enum", "options", []interface{}{"sandbox", "production"}, "default", "sandbox", "confirm_typed_on", "production")},
		"selection", mm("cardinality", "one", "scopes", []interface{}{"org", "branch"}),
		"health", mm("operation", "status", "every", "5m"),
		"conformance", "conformance/acme.document@1",
	)}
}

func asContract(m map[string]interface{}) {
	m["models"] = ownModel()
	m["contracts"] = contractBlock()
}

func asProvider(m map[string]interface{}) {
	m["implements"] = []interface{}{mm("contract", "acme.document", "version", "^1.0", "countries", []interface{}{"ZZ"}, "operations", []interface{}{"issue", "status"})}
	m["compatibility"].(map[string]interface{})["requires"] = []interface{}{
		mm("key", "kernel", "version", ">=3.0.0 <4.0.0"), mm("key", "acme_core", "version", "^1"),
	}
}

func validateRole(t *testing.T, m map[string]interface{}, opts Options) ([]string, error) {
	t.Helper()
	return ValidateWithOptions(mustJSON(t, m), opts)
}

func TestRoles_ValidManifests(t *testing.T) {
	cases := map[string]map[string]interface{}{
		"contract":     roleDoc("acme_core", "contract", asContract),
		"provider":     roleDoc("acme_pay", "provider", asProvider),
		"foundation":   roleDoc("acme_items", "foundation", nil),
		"vertical":     roleDoc("acme_trade", "vertical", nil),
		"app":          roleDoc("acme_desk", "app", nil),
		"localization": roleDoc("l10n_zz", "localization", nil),
		"extension": roleDoc("acme_pay_term", "extension", func(m map[string]interface{}) {
			asProvider(m)
			m["extends_addon"] = mm("key", "acme_core", "version", "^1")
		}),
		"replaces + deprecated": roleDoc("acme_pay", "provider", func(m map[string]interface{}) {
			asProvider(m)
			m["replaces"] = []interface{}{"acme_legacy"}
			m["metadata"].(map[string]interface{})["deprecated"] = mm("since", "2.0.0", "replacement", "acme_pay2", "sunset", "2027-12-31")
		}),
	}
	for name, m := range cases {
		warns, err := validateRole(t, m, Options{Strict: true})
		if err != nil {
			t.Errorf("%s: want valid, got %v", name, err)
		}
		if len(warns) != 0 {
			t.Errorf("%s: declared role must not warn, got %v", name, warns)
		}
	}
}

func TestRoles_InvalidRoleValueRejected(t *testing.T) {
	m := roleDoc("acme_items", "wizard", nil)
	for _, strict := range []bool{true, false} {
		if _, err := validateRole(t, m, Options{Strict: strict}); err == nil || !strings.Contains(err.Error(), "role") {
			t.Fatalf("strict=%v: invalid role must be rejected, got %v", strict, err)
		}
	}
}

func TestRoles_SchemaRejectsMalformedBlocks(t *testing.T) {
	cases := map[string]map[string]interface{}{
		"contract key without family": roleDoc("acme_core", "contract", func(m map[string]interface{}) {
			asContract(m)
			m["contracts"].([]interface{})[0].(map[string]interface{})["key"] = "document"
		}),
		"operation kind": roleDoc("acme_core", "contract", func(m map[string]interface{}) {
			asContract(m)
			m["contracts"].([]interface{})[0].(map[string]interface{})["operations"].([]interface{})[0].(map[string]interface{})["kind"] = "mutation"
		}),
		"failover": roleDoc("acme_core", "contract", func(m map[string]interface{}) {
			asContract(m)
			m["contracts"].([]interface{})[0].(map[string]interface{})["operations"].([]interface{})[0].(map[string]interface{})["failover"] = "sometimes"
		}),
		"selection scope": roleDoc("acme_core", "contract", func(m map[string]interface{}) {
			asContract(m)
			m["contracts"].([]interface{})[0].(map[string]interface{})["selection"] = mm("scopes", []interface{}{"planet"})
		}),
		"unknown contract prop": roleDoc("acme_core", "contract", func(m map[string]interface{}) {
			asContract(m)
			m["contracts"].([]interface{})[0].(map[string]interface{})["wat"] = 1
		}),
		"implements without version": roleDoc("acme_pay", "provider", func(m map[string]interface{}) {
			asProvider(m)
			delete(m["implements"].([]interface{})[0].(map[string]interface{}), "version")
		}),
		"country code": roleDoc("acme_pay", "provider", func(m map[string]interface{}) {
			asProvider(m)
			m["implements"].([]interface{})[0].(map[string]interface{})["countries"] = []interface{}{"zzz"}
		}),
		"extends without key": roleDoc("acme_pay_term", "extension", func(m map[string]interface{}) {
			asProvider(m)
			m["extends_addon"] = mm("version", "^1")
		}),
		"replaces duplicated": roleDoc("acme_pay", "provider", func(m map[string]interface{}) {
			asProvider(m)
			m["replaces"] = []interface{}{"a_b", "a_b"}
		}),
		"deprecated unknown prop": roleDoc("acme_items", "foundation", func(m map[string]interface{}) {
			m["metadata"].(map[string]interface{})["deprecated"] = mm("when", "now")
		}),
		"health every": roleDoc("acme_core", "contract", func(m map[string]interface{}) {
			asContract(m)
			m["contracts"].([]interface{})[0].(map[string]interface{})["health"] = mm("operation", "status", "every", "often")
		}),
	}
	for name, m := range cases {
		if _, err := validateRole(t, m, Options{Strict: true}); err == nil {
			t.Errorf("%s: schema must reject it", name)
		}
	}
}

func TestRoles_NoRoleNoSignalStaysSilent(t *testing.T) {
	warns, err := validateRole(t, roleDoc("acme_items", "", nil), Options{Strict: true})
	if err != nil || len(warns) != 0 {
		t.Fatalf("plain manifest must stay silent: warns=%v err=%v", warns, err)
	}
}

func TestRoles_ProvidesCapabilitiesStillRead(t *testing.T) {
	m := roleDoc("acme_pay", "", func(m map[string]interface{}) {
		m["provides_capabilities"] = []interface{}{mm("key", "messaging.thing.send", "handler", mm("type", "wasm", "function", "do_send"))}
	})
	warns, err := validateRole(t, m, Options{Strict: true})
	if err != nil {
		t.Fatalf("provides_capabilities must keep validating: %v", err)
	}
	if !strings.Contains(strings.Join(warns, "\n"), `inferred "provider"`) {
		t.Errorf("provides_capabilities implies provider, got %v", warns)
	}
}

func TestRoles_InferRole(t *testing.T) {
	cases := []struct {
		f    RoleFacts
		want string
	}{
		{RoleFacts{Preset: true}, RolePreset},
		{RoleFacts{Contracts: []Contract{{Key: "a.b"}}}, RoleContract},
		{RoleFacts{ExtendsAddon: &ExtendsAddon{Key: "x"}, Implements: []Implements{{Contract: "a.b"}}}, RoleExtension},
		{RoleFacts{Implements: []Implements{{Contract: "a.b"}}}, RoleProvider},
		{RoleFacts{ProvidedCapabilities: []string{"a.b.c"}}, RoleProvider},
		{RoleFacts{}, ""},
	}
	for i, c := range cases {
		if got := InferRole(c.f); got != c.want {
			t.Errorf("case %d: InferRole = %q, want %q", i, got, c.want)
		}
	}
}

// An undeclared role never errors: the role is inferred and named in the
// role_missing warning (the enforcing rules arrive in role_validate.go).
func TestRoles_InferredRoleWarnsNeverErrors(t *testing.T) {
	m := roleDoc("acme_pay", "", asProvider)
	for _, strict := range []bool{true, false} {
		warns, err := validateRole(t, m, Options{Strict: strict})
		if err != nil {
			t.Fatalf("strict=%v: an undeclared role must not error: %v", strict, err)
		}
		if !strings.Contains(strings.Join(warns, "\n"), "["+RuleRoleMissing+"]") || !strings.Contains(strings.Join(warns, "\n"), `inferred "provider"`) {
			t.Errorf("strict=%v: want role_missing naming the inferred role, got %v", strict, warns)
		}
	}
}

// The role rule id is inside the compatibility window and warning-only.
func TestRoles_MissingRuleIsWarnOnlyWindow(t *testing.T) {
	if _, ok := warnOnlyWindow[RuleRoleMissing]; !ok {
		t.Error("role_missing must be warn-only")
	}
	found := false
	for _, l := range LegacyTolerated() {
		if l == RuleRoleMissing {
			found = true
		}
	}
	if !found {
		t.Error("role_missing must be in legacyTolerated")
	}
}

// Real addon manifests (no metadata.role, copied from the addons monorepo)
// validate exactly as before: no error and no role warning, in strict and
// tolerant mode. mercadopago has provides_capabilities, so it only gains the
// inference warning.
func TestRoles_RealAddonManifestsUnchanged(t *testing.T) {
	files, _ := filepath.Glob("testdata/roles/*.manifest.json")
	if len(files) < 3 {
		t.Fatalf("want the real-manifest fixtures, found %d", len(files))
	}
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, strict := range []bool{true, false} {
			warns, err := ValidateWithOptions(raw, Options{Strict: strict})
			if err != nil {
				t.Errorf("%s (strict=%v): %v", f, strict, err)
				continue
			}
			for _, w := range warns {
				if strings.Contains(w, "role") && !strings.Contains(f, "mercadopago") {
					t.Errorf("%s (strict=%v): unexpected role warning %q", f, strict, w)
				}
			}
		}
	}
}

// The frozen shape of the contract surface: deprecated{since, replacement,
// sunset} on addon, contract and operation; failover before_effect;
// implements[].scope; Family() derived from the key.
func TestRoles_FrozenContractShape(t *testing.T) {
	ok := roleDoc("acme_core", "contract", func(m map[string]interface{}) {
		asContract(m)
		c := m["contracts"].([]interface{})[0].(map[string]interface{})
		c["deprecated"] = mm("since", "1.2.0", "replacement", "acme.document2", "sunset", "2027-06-30")
		ops := c["operations"].([]interface{})
		ops[0].(map[string]interface{})["failover"] = "before_effect"
		ops[1].(map[string]interface{})["deprecated"] = mm("since", "1.1.0", "replacement", "lookup")
		m["metadata"].(map[string]interface{})["deprecated"] = mm("since", "2.0.0", "replacement", "acme_core2", "sunset", "2028-01-01")
	})
	if _, err := validateRole(t, ok, Options{Strict: true}); err != nil {
		t.Fatalf("frozen shape must validate: %v", err)
	}
	prov := roleDoc("acme_pay", "provider", func(m map[string]interface{}) {
		asProvider(m)
		m["implements"].([]interface{})[0].(map[string]interface{})["scope"] = []interface{}{"org", "branch"}
	})
	if _, err := validateRole(t, prov, Options{Strict: true}); err != nil {
		t.Fatalf("implements[].scope must validate: %v", err)
	}

	bad := map[string]map[string]interface{}{
		"old replaced_by name": roleDoc("acme_items", "foundation", func(m map[string]interface{}) {
			m["metadata"].(map[string]interface{})["deprecated"] = mm("replaced_by", "x")
		}),
		"sunset not a date": roleDoc("acme_items", "foundation", func(m map[string]interface{}) {
			m["metadata"].(map[string]interface{})["deprecated"] = mm("sunset", "soon")
		}),
		"operation deprecated unknown prop": roleDoc("acme_core", "contract", func(m map[string]interface{}) {
			asContract(m)
			m["contracts"].([]interface{})[0].(map[string]interface{})["operations"].([]interface{})[0].(map[string]interface{})["deprecated"] = mm("message", "x")
		}),
		"implements scope enum": roleDoc("acme_pay", "provider", func(m map[string]interface{}) {
			asProvider(m)
			m["implements"].([]interface{})[0].(map[string]interface{})["scope"] = []interface{}{"planet"}
		}),
	}
	for name, m := range bad {
		if _, err := validateRole(t, m, Options{Strict: true}); err == nil {
			t.Errorf("%s: schema must reject it", name)
		}
	}
	if got := (Contract{Key: "acme.document"}).Family(); got != "acme" {
		t.Errorf("Family() = %q", got)
	}
	if got := (Contract{Key: "single"}).Family(); got != "single" {
		t.Errorf("Family() of a key without dot = %q", got)
	}
}
