package manifest_test

import (
	"testing"

	"github.com/asteby/metacore-kernel/manifest"
	v3 "github.com/asteby/metacore-kernel/manifest/v3"
)

func TestValidFormulaHandler(t *testing.T) {
	for _, ok := range []string{"wasm:price", "compiled:Price_2", "compiled:_x"} {
		if !v3.ValidFormulaHandler(ok) {
			t.Fatalf("%q must be valid", ok)
		}
	}
	for _, bad := range []string{"", "price", "compiled:", "webhook:x", "compiled:2x", "compiled:a-b", "wasm: x"} {
		if v3.ValidFormulaHandler(bad) {
			t.Fatalf("%q must be invalid", bad)
		}
	}
}

func TestLegacyValidate_CompiledHookTarget(t *testing.T) {
	m := manifest.Manifest{Key: "mod", Name: "Mod", Version: "1.0.0", LifecycleHooks: map[string][]manifest.HookDef{
		"install": {{Target: manifest.HookTarget{Type: "compiled", Function: "OnInstall"}}},
	}}
	if err := m.Validate("1.0.0"); err != nil {
		t.Fatalf("compiled hook target must validate: %v", err)
	}
	m.LifecycleHooks["install"][0].Target.Function = ""
	if err := m.Validate("1.0.0"); err == nil {
		t.Fatal("compiled hook without function must be rejected")
	}
}

const compiledOnlyManifest = `{
  "apiVersion": "asteby.com/v3",
  "kind": "Addon",
  "metadata": { "key": "native_mod", "name": "Native", "version": "0.1.0" },
  "compatibility": { "requires": [{ "key": "kernel", "version": ">=3.0.0 <4.0.0" }] },
  "contributions": {
    "actions": [
      { "key": "pay", "label": "Pay", "target_model": "Sale", "handler": { "type": "compiled", "function": "OnPay" } }
    ]
  }
}`

// A pure compiled module has no wasm backend; its action projects to the shared
// trigger; mixing in a wasm handler whitelists the compiled symbols too.
func TestFromV3_CompiledOnlyHasNoBackendAndSharesTrigger(t *testing.T) {
	m, err := v3.Parse([]byte(compiledOnlyManifest))
	if err != nil {
		t.Fatal(err)
	}
	out := manifest.FromV3(m)
	if out.Backend != nil {
		t.Fatalf("a compiled-only module must not derive a wasm backend: %+v", out.Backend)
	}
	var found bool
	for _, acts := range out.Actions {
		for _, a := range acts {
			if a.Key == "pay" {
				found = true
				if a.Trigger == nil || a.Trigger.Type != "wasm" || a.Trigger.Export != "OnPay" {
					t.Fatalf("trigger = %+v", a.Trigger)
				}
			}
		}
	}
	if !found {
		t.Fatal("action not projected")
	}
}
