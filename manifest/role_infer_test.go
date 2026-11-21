package manifest

import (
	"encoding/json"
	"strings"
	"testing"

	v3 "github.com/asteby/metacore-kernel/manifest/v3"
)

func legacyFromV3(t *testing.T, doc map[string]any) Manifest {
	t.Helper()
	raw, _ := json.Marshal(doc)
	m3, _, err := v3.ParseWithOptions(raw, v3.Options{})
	if err != nil {
		t.Fatalf("v3 parse: %v", err)
	}
	return FromV3(m3)
}

func rm(kv ...any) map[string]any {
	out := map[string]any{}
	for i := 0; i+1 < len(kv); i += 2 {
		out[kv[i].(string)] = kv[i+1]
	}
	return out
}

func doc(key, role string, mut func(map[string]any)) map[string]any {
	md := rm("key", key, "name", key, "version", "1.0.0")
	if role != "" {
		md["role"] = role
	}
	m := rm(
		"apiVersion", "asteby.com/v3", "kind", "Addon", "metadata", md,
		"compatibility", rm("requires", []any{rm("key", "kernel", "version", ">=3.0.0 <4.0.0"), rm("key", "acme_core", "version", "^1")}),
		"tenancy", rm("isolation", "shared", "rls_column", "organization_id"),
		"capabilities", []any{rm("kind", "db:write", "target", "addon_"+key+".*")},
	)
	if mut != nil {
		mut(m)
	}
	return m
}

func provider(m map[string]any) {
	m["implements"] = []any{rm("contract", "acme.document", "version", "^1.0")}
}

func TestFromV3_CarriesRoleFields(t *testing.T) {
	m := legacyFromV3(t, doc("acme_pay_term", "extension", func(m map[string]any) {
		provider(m)
		m["extends_addon"] = rm("key", "acme_core", "version", "^1")
		m["replaces"] = []any{"acme_legacy"}
		m["metadata"].(map[string]any)["deprecated"] = rm("since", "2.0.0", "replacement", "acme_next", "sunset", "2027-01-31")
	}))
	if m.Role != "extension" || len(m.Implements) != 1 || m.ExtendsAddon == nil || m.ExtendsAddon.Key != "acme_core" ||
		len(m.Replaces) != 1 || m.Deprecated == nil || m.Deprecated.Replacement != "acme_next" {
		t.Fatalf("role fields lost in FromV3: %+v", m)
	}
	if err := m.Validate("3.1.0"); err != nil {
		t.Fatalf("a valid declared extension must pass the legacy validator: %v", err)
	}
}

func TestLegacyValidator_InvalidRoleValueRejected(t *testing.T) {
	m := Manifest{Key: "acme_items", Name: "Items", Version: "1.0.0", Role: "wizard"}
	if err := m.Validate("3.1.0"); err == nil || !strings.Contains(err.Error(), "metadata.role") {
		t.Fatalf("want invalid role rejected, got %v", err)
	}
}

// An undeclared role never fails the legacy validator; ValidateAdvisory names
// the inferred one.
func TestLegacyValidator_InferredRoleWarns(t *testing.T) {
	m := legacyFromV3(t, doc("acme_pay", "", provider))
	if err := m.Validate("3.1.0"); err != nil {
		t.Fatalf("undeclared role must not fail: %v", err)
	}
	warns, err := m.ValidateAdvisory("3.1.0")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(warns, "\n"), "role_missing") {
		t.Fatalf("want role_missing warning, got %v", warns)
	}
}
