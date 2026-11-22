package installer

// Contract registry wiring against a real Postgres (TEST_POSTGRES_DSN); skipped
// otherwise. A failed install leaves no declared key behind; a committed one
// registers it, and a restart (a brand-new Registry) rehydrates the same state
// from the persisted rows.

import (
	"testing"

	"github.com/asteby/metacore-kernel/capability"
	v3 "github.com/asteby/metacore-kernel/manifest/v3"
	"github.com/google/uuid"
)

func TestInstall_ContractsRegisteredOnlyAfterCommit(t *testing.T) {
	f := newCompFixture(t)
	f.b.Manifest.Contracts = []v3.Contract{{Key: "acme.document", Version: "1.0.0", Operations: []v3.ContractOperation{{Key: "issue", Kind: "action"}}}}
	reg := capability.NewRegistry()
	f.inst.ContractRegistry = func(uuid.UUID) *capability.Registry { return reg }

	// A failure after the persist step rolls the install back: nothing dangles.
	f.failAt("persist")
	if _, _, err := f.inst.Install(f.org, f.b); err == nil {
		t.Fatal("want the injected failure")
	}
	if _, ok := reg.Known("acme.document.issue"); ok {
		t.Fatal("a failed install must not leave its contract keys registered")
	}

	// The retry commits: the keys are known, with the contract's version.
	f.inst.installFailpoint = nil
	if _, _, err := f.inst.Install(f.org, f.b); err != nil {
		t.Fatalf("retry: %v", err)
	}
	live, ok := reg.Known("acme.document.issue")
	if !ok || live.Version != "1.0.0" || live.Source != "addon:"+f.key {
		t.Fatalf("committed install must register: %+v %v", live, ok)
	}

	// Restart: a new empty Registry, rehydrated from the persisted installation.
	restarted := capability.NewRegistry()
	ic, err := f.inst.InstalledContracts(f.org)
	if err != nil {
		t.Fatal(err)
	}
	if rep := restarted.Rehydrate(ic); len(rep.Failed) != 0 {
		t.Fatalf("rehydrate: %+v", rep)
	}
	again, ok := restarted.Known("acme.document.issue")
	if !ok || again != live {
		t.Fatalf("rehydrated state must equal the live one: live=%+v rehydrated=%+v ok=%v", live, again, ok)
	}
}

// Uninstall really forgets the addon in the org's registry (the unit test covers
// unregisterDeclaredContracts; this one covers the call from uninstallAddon).
func TestUninstall_UnregistersContractsAndImplements(t *testing.T) {
	f := newCompFixture(t)
	f.b.Manifest.Contracts = []v3.Contract{{Key: "acme.document", Version: "1.0.0", Operations: []v3.ContractOperation{{Key: "issue", Kind: "action"}}}}
	f.b.Manifest.Implements = []v3.Implements{{Contract: "acme.mail", Version: "^1.0"}}
	reg := capability.NewRegistry()
	f.inst.ContractRegistry = func(uuid.UUID) *capability.Registry { return reg }
	if _, _, err := f.inst.Install(f.org, f.b); err != nil {
		t.Fatalf("install: %v", err)
	}
	if len(reg.Implementers("acme.mail")) != 1 {
		t.Fatal("install must register the implementer")
	}
	if err := f.inst.Uninstall(f.org, f.key, false); err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	if got := reg.Implementers("acme.mail"); len(got) != 0 {
		t.Fatalf("an uninstalled provider must leave Implementers: %+v", got)
	}
	if _, ok := reg.Known("acme.document.issue"); ok {
		t.Fatal("an uninstalled contract owner must leave the registry")
	}
}
