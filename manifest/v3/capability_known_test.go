package v3

import (
	"os"
	"strings"
	"testing"

	"github.com/asteby/metacore-kernel/capability"
)

func warnsContaining(warns []string, sub string) []string {
	var out []string
	for _, w := range warns {
		if strings.Contains(w, sub) {
			out = append(out, w)
		}
	}
	return out
}

// Phase 1: an unknown key is a WARNING naming the key, never an error, in both
// modes. Real manifests: fx providers (an unregistered family) and the
// mercadopago key the plan names.
func TestCapabilityKeys_RealManifestsWarnNeverError(t *testing.T) {
	cases := map[string]string{
		"testdata/capabilities/fx_providers.manifest.json": "fx.rate.current",
		"testdata/capabilities/mercadopago.manifest.json":  "mercadopago.payments.brick_submit",
	}
	for f, key := range cases {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, strict := range []bool{true, false} {
			warns, err := ValidateWithOptions(raw, Options{Strict: strict})
			if err != nil {
				t.Fatalf("%s (strict=%v): must not error: %v", f, strict, err)
			}
			w := warnsContaining(warns, "["+RuleCapabilityKeyUnknown+"]")
			if len(w) == 0 || !strings.Contains(strings.Join(w, "\n"), `"`+key+`"`) {
				t.Errorf("%s (strict=%v): want capability_key_unknown naming %q, got %v", f, strict, key, warns)
			}
		}
	}
}

func provDoc(key, capKey string) map[string]interface{} {
	return roleDoc(key, "", func(m map[string]interface{}) {
		m["provides_capabilities"] = []interface{}{mm("key", capKey, "handler", mm("type", "wasm", "function", "do_it"))}
	})
}

func TestCapabilityKeys_KnownKeysStaySilent(t *testing.T) {
	// A kernel contract.
	warns, err := validateRole(t, provDoc("acme_pay", capability.CommercePaymentsStatus), Options{Strict: true})
	if err != nil || len(warnsContaining(warns, RuleCapabilityKeyUnknown)) != 0 {
		t.Fatalf("kernel key: warns=%v err=%v", warns, err)
	}
	// Declared by the same manifest's contracts[] (operation key).
	m := roleDoc("acme_core", "contract", asContract)
	m["provides_capabilities"] = []interface{}{mm("key", "acme.document.issue", "handler", mm("type", "wasm", "function", "do_it"))}
	warns, err = validateRole(t, m, Options{Strict: true})
	if err != nil || len(warnsContaining(warns, RuleCapabilityKeyUnknown)) != 0 {
		t.Fatalf("own contract key: warns=%v err=%v", warns, err)
	}
}

func TestCapabilityKeys_RegistryAndCatalogMakeKeysKnown(t *testing.T) {
	d := provDoc("acme_pay", "acme.document.issue")
	warns, _ := validateRole(t, d, Options{Strict: true})
	if len(warnsContaining(warns, RuleCapabilityKeyUnknown)) != 1 {
		t.Fatalf("unknown before registering: %v", warns)
	}
	// An installed contract addon registers its contract.
	if err := capability.RegisterDeclared("acme_core", capability.Declared{Key: "acme.document", Version: "1.0.0", Operations: []capability.DeclaredOperation{{Key: "issue"}}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { capability.UnregisterDeclared("acme_core") })
	warns, _ = validateRole(t, d, Options{Strict: true})
	if len(warnsContaining(warns, RuleCapabilityKeyUnknown)) != 0 {
		t.Fatalf("known after RegisterDeclared: %v", warns)
	}
	// A host catalog extends it; a typed nil catalog is ignored without panic.
	capability.UnregisterDeclared("acme_core")
	warns, _ = validateRole(t, d, Options{Strict: true, CapabilityCatalog: hostKeys{"acme.document.issue": true}})
	if len(warnsContaining(warns, RuleCapabilityKeyUnknown)) != 0 {
		t.Fatalf("known through the catalog: %v", warns)
	}
	var nilCat *hostKeysPtr
	warns, _ = validateRole(t, d, Options{Strict: true, CapabilityCatalog: nilCat})
	if len(warnsContaining(warns, RuleCapabilityKeyUnknown)) != 1 {
		t.Fatalf("typed nil catalog must behave as none: %v", warns)
	}
}

type hostKeys map[string]bool

func (h hostKeys) KnownCapability(k string) bool { return h[k] }

type hostKeysPtr struct{}

func (*hostKeysPtr) KnownCapability(string) bool { panic("typed nil catalog consulted") }

func TestCapabilityKeys_RuleIsWarnOnlyWindow(t *testing.T) {
	if _, ok := warnOnlyWindow[RuleCapabilityKeyUnknown]; !ok {
		t.Error("capability_key_unknown must be warn-only in this release")
	}
	for _, l := range LegacyTolerated() {
		if l == RuleCapabilityKeyUnknown {
			return
		}
	}
	t.Error("capability_key_unknown must be in legacyTolerated")
}

type versionedCat struct {
	fakeCat
	versions map[string]string
}

func (c versionedCat) ContractVersion(k string) (string, bool) { v, ok := c.versions[k]; return v, ok }

// implements[].version is checked against the contract's version when a
// catalog or the registry knows it: an error for a declared provider, a
// warning for an inferred one.
func TestImplementsVersionCompatibility(t *testing.T) {
	cat := versionedCat{
		fakeCat:  fakeCat{contracts: map[string][]string{"acme.document": {"doc_records"}}},
		versions: map[string]string{"acme.document": "2.0.0"},
	}
	declaredProv := roleDoc("acme_pay", "provider", asProvider) // implements ^1.0
	if _, err := validateRole(t, declaredProv, Options{Strict: true, RoleCatalog: cat}); err == nil || !strings.Contains(err.Error(), "is not satisfied by contract version 2.0.0") {
		t.Fatalf("declared provider: want version error, got %v", err)
	}
	inferred := roleDoc("acme_pay", "", asProvider)
	warns, err := validateRole(t, inferred, Options{Strict: true, RoleCatalog: cat})
	if err != nil || len(warnsContaining(warns, "is not satisfied by contract version 2.0.0")) != 1 {
		t.Fatalf("inferred provider: want a warning, got warns=%v err=%v", warns, err)
	}
	cat.versions["acme.document"] = "1.4.0"
	if _, err := validateRole(t, declaredProv, Options{Strict: true, RoleCatalog: cat}); err != nil {
		t.Fatalf("a satisfied range must pass: %v", err)
	}
	// Without catalog, the process registry of declared contracts supplies it.
	if err := capability.RegisterDeclared("acme_core", capability.Declared{Key: "acme.document", Version: "2.0.0"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { capability.UnregisterDeclared("acme_core") })
	if _, err := validateRole(t, declaredProv, Options{Strict: true}); err == nil || !strings.Contains(err.Error(), "not satisfied") {
		t.Fatalf("registry version must be checked without a catalog: %v", err)
	}
	// A catalog that does not serve versions (the published RoleCatalog only) checks nothing.
	capability.UnregisterDeclared("acme_core")
	if _, err := validateRole(t, declaredProv, Options{Strict: true, RoleCatalog: fakeCat{contracts: map[string][]string{"acme.document": {}}}}); err != nil {
		t.Fatalf("no version source, no check: %v", err)
	}
}
