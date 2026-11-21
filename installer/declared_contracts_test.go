package installer

import (
	"testing"

	"github.com/asteby/metacore-kernel/capability"
	"github.com/asteby/metacore-kernel/manifest"
	v3 "github.com/asteby/metacore-kernel/manifest/v3"
)

// Installing (or upgrading) an addon that owns contracts records them in the
// capability registry; a conflicting owner is logged, never an install failure.
func TestRegisterDeclaredContracts(t *testing.T) {
	t.Cleanup(func() { capability.UnregisterDeclared("acme_core"); capability.UnregisterDeclared("other_core") })
	m := &manifest.Manifest{Key: "acme_core", Version: "1.0.0", Contracts: []v3.Contract{
		{Key: "acme.document", Version: "1.0.0", Operations: []v3.ContractOperation{{Key: "issue", Kind: "action"}}},
	}}
	registerDeclaredContracts(m)
	if info, ok := capability.Known("acme.document.issue"); !ok || info.Source != "addon:acme_core" {
		t.Fatalf("contract not registered on install: %+v %v", info, ok)
	}
	// Upgrade: same owner replaces.
	m.Contracts[0].Version = "1.1.0"
	registerDeclaredContracts(m)
	if info, _ := capability.Known("acme.document"); info.Version != "1.1.0" {
		t.Fatalf("upgrade must replace the version: %+v", info)
	}
	// Another addon claiming the same contract key must not panic nor fail.
	other := &manifest.Manifest{Key: "other_core", Version: "1.0.0", Contracts: m.Contracts}
	registerDeclaredContracts(other)
	if info, _ := capability.Known("acme.document"); info.Source != "addon:acme_core" {
		t.Fatalf("the first owner keeps the contract: %+v", info)
	}
	// An addon without contracts is a no-op.
	registerDeclaredContracts(&manifest.Manifest{Key: "plain", Version: "1.0.0"})
}
