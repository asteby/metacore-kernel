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
		t.Fatalf("unknown without a catalog: %v", warns)
	}
	// The scope's registry (an organization's installs) makes it known.
	reg := capability.NewRegistry()
	if err := reg.Register("acme_core", capability.Declared{Key: "acme.document", Version: "1.0.0", Operations: []capability.DeclaredOperation{{Key: "issue"}}}); err != nil {
		t.Fatal(err)
	}
	warns, _ = validateRole(t, d, Options{Strict: true, CapabilityCatalog: reg})
	if len(warnsContaining(warns, RuleCapabilityKeyUnknown)) != 0 {
		t.Fatalf("known through the registry: %v", warns)
	}
	// ...and ONLY for that scope: another organization's registry does not know it,
	// and there is no process-wide fallback.
	other := capability.NewRegistry()
	warns, _ = validateRole(t, d, Options{Strict: true, CapabilityCatalog: other})
	if len(warnsContaining(warns, RuleCapabilityKeyUnknown)) != 1 {
		t.Fatalf("a registry of another scope must not make the key known: %v", warns)
	}
	// A host catalog works the same; a typed nil catalog is ignored without panic.
	warns, _ = validateRole(t, d, Options{Strict: true, CapabilityCatalog: hostKeys{"acme.document.issue": true}})
	if len(warnsContaining(warns, RuleCapabilityKeyUnknown)) != 0 {
		t.Fatalf("known through the catalog: %v", warns)
	}
	var nilCat *hostKeysPtr
	warns, _ = validateRole(t, d, Options{Strict: true, CapabilityCatalog: nilCat})
	if len(warnsContaining(warns, RuleCapabilityKeyUnknown)) != 1 {
		t.Fatalf("typed nil catalog must behave as none: %v", warns)
	}
	var nilReg *capability.Registry
	warns, _ = validateRole(t, d, Options{Strict: true, CapabilityCatalog: nilReg})
	if len(warnsContaining(warns, RuleCapabilityKeyUnknown)) != 1 {
		t.Fatalf("a nil *Registry must behave as an empty one: %v", warns)
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

// registryCat adapts one scope's Registry to the validators' RoleCatalog (what
// a host does): the version comes from that scope alone.
type registryCat struct {
	fakeCat
	reg *capability.Registry
}

func (c registryCat) ContractVersion(k string) (string, bool) { return c.reg.ContractVersion(k) }

func scopeWith(t *testing.T, version string) registryCat {
	t.Helper()
	reg := capability.NewRegistry()
	if err := reg.Register("acme_core", capability.Declared{Key: "acme.document", Version: version}); err != nil {
		t.Fatal(err)
	}
	return registryCat{fakeCat: fakeCat{contracts: map[string][]string{"acme.document": {"doc_records"}}}, reg: reg}
}

// implements[].version is checked against the version the HOST's scope serves:
// an error for a declared provider, a warning for an inferred one. Two
// organizations with different installed versions get different answers, and
// the kernel keeps no process-wide fallback (one tenant's upgrade to 2.0.0 must
// never reject another tenant's provider of ^1.0).
func TestImplementsVersionCompatibility_PerScope(t *testing.T) {
	declaredProv := roleDoc("acme_pay", "provider", asProvider) // implements ^1.0
	orgA, orgB := scopeWith(t, "2.0.0"), scopeWith(t, "1.4.0")

	if _, err := validateRole(t, declaredProv, Options{Strict: true, RoleCatalog: orgA}); err == nil || !strings.Contains(err.Error(), "is not satisfied by contract version 2.0.0") {
		t.Fatalf("org A: want version error, got %v", err)
	}
	if _, err := validateRole(t, declaredProv, Options{Strict: true, RoleCatalog: orgB}); err != nil {
		t.Fatalf("org B (1.4.0) must still accept ^1.0: %v", err)
	}
	inferred := roleDoc("acme_pay", "", asProvider)
	warns, err := validateRole(t, inferred, Options{Strict: true, RoleCatalog: orgA})
	if err != nil || len(warnsContaining(warns, "is not satisfied by contract version 2.0.0")) != 1 {
		t.Fatalf("inferred provider: want a warning, got warns=%v err=%v", warns, err)
	}
	// A prerelease does not satisfy ^1.0.
	if _, err := validateRole(t, declaredProv, Options{Strict: true, RoleCatalog: scopeWith(t, "1.1.0-beta")}); err == nil {
		t.Fatal("1.1.0-beta must not satisfy ^1.0")
	}
}

// With no source for the contract's version the check is SKIPPED, loudly: a
// warning, never an error and never a silent pass.
func TestImplementsVersionUnchecked_WarnsWithoutVersionSource(t *testing.T) {
	prov := roleDoc("acme_pay", "provider", asProvider)
	cases := map[string]Options{
		"no catalog":                 {Strict: true},
		"catalog without versions":   {Strict: true, RoleCatalog: fakeCat{contracts: map[string][]string{"acme.document": {}}}},
		"catalog unaware of version": {Strict: true, RoleCatalog: registryCat{fakeCat: fakeCat{contracts: map[string][]string{"acme.document": {}}}, reg: capability.NewRegistry()}},
	}
	for name, o := range cases {
		warns, err := validateRole(t, prov, o)
		if err != nil {
			t.Errorf("%s: must not error: %v", name, err)
		}
		if len(warnsContaining(warns, "["+RuleImplementsVersionUnchecked+"]")) != 1 {
			t.Errorf("%s: want implements_version_unchecked, got %v", name, warns)
		}
	}
	var nilCat *fakeCatPtr
	warns, err := validateRole(t, prov, Options{Strict: true, RoleCatalog: nilCat})
	if err != nil || len(warnsContaining(warns, RuleImplementsVersionUnchecked)) != 1 {
		t.Errorf("typed nil catalog: warns=%v err=%v", warns, err)
	}
	if _, ok := warnOnlyWindow[RuleImplementsVersionUnchecked]; !ok {
		t.Error("implements_version_unchecked must be warn-only")
	}
}

type fakeCatPtr struct{}

func (*fakeCatPtr) RoleOf(string) (string, bool)           { panic("typed nil catalog consulted") }
func (*fakeCatPtr) WritableTables(string) ([]string, bool) { panic("typed nil catalog consulted") }
