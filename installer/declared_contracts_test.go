package installer

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/asteby/metacore-kernel/capability"
	"github.com/asteby/metacore-kernel/lifecycle"
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

// A restart empties the process registry; RehydrateDeclaredContracts rebuilds it
// from the manifests the host reloaded into the lifecycle registry, and one bad
// addon never blocks the others.
func TestRehydrateDeclaredContracts(t *testing.T) {
	t.Cleanup(func() { capability.UnregisterDeclared("acme_core"); capability.UnregisterDeclared("rival_core") })
	inst := &Installer{Lifecycles: lifecycle.NewRegistry()}
	inst.Lifecycles.Register("acme_core", &lifecycle.ManifestOnly{Data: manifest.Manifest{Key: "acme_core", Version: "1.0.0", Contracts: []v3.Contract{
		{Key: "acme.document", Version: "1.3.0", Operations: []v3.ContractOperation{{Key: "issue", Kind: "action"}}},
	}}})
	inst.Lifecycles.Register("plain", &lifecycle.ManifestOnly{Data: manifest.Manifest{Key: "plain", Version: "1.0.0"}})
	inst.Lifecycles.Register("rival_core", &lifecycle.ManifestOnly{Data: manifest.Manifest{Key: "rival_core", Version: "1.0.0", Contracts: []v3.Contract{
		{Key: "acme.document", Version: "9.0.0"}, {Key: "rival.thing", Version: "1.0.0"},
	}}})
	if _, ok := capability.Known("acme.document.issue"); ok {
		t.Fatal("registry must start empty (simulated restart)")
	}
	rep := inst.RehydrateDeclaredContracts()
	if info, ok := capability.Known("acme.document.issue"); !ok || info.Version != "1.3.0" {
		t.Fatalf("rehydrate must restore declared keys: %+v %v", info, ok)
	}
	if len(rep.Registered) == 0 {
		t.Fatalf("report: %+v", rep)
	}
	// Exactly one of the two rivals for acme.document wins, the other is reported, none is fatal.
	if len(rep.Failed) != 1 {
		t.Fatalf("one conflicting owner must be reported, got %+v", rep)
	}
	// Idempotent.
	rep2 := inst.RehydrateDeclaredContracts()
	if _, ok := capability.Known("acme.document.issue"); !ok || len(rep2.Failed) > 1 {
		t.Fatalf("second pass: %+v", rep2)
	}
	var nilInst *Installer
	if r := nilInst.RehydrateDeclaredContracts(); len(r.Registered) != 0 {
		t.Fatal("nil installer is a no-op")
	}
}

// Removing a contract owner while others still implement its contract is
// reported (never silent) and never panics; a plain addon logs nothing.
func TestWarnDanglingContracts(t *testing.T) {
	reg := lifecycle.NewRegistry()
	owner := manifest.Manifest{Key: "acme_core", Version: "1.0.0", Contracts: []v3.Contract{{Key: "acme.document", Version: "1.0.0"}}}
	reg.Register("acme_pay", &lifecycle.ManifestOnly{Data: manifest.Manifest{Key: "acme_pay", Version: "1.0.0", Implements: []v3.Implements{{Contract: "acme.document", Version: "^1"}}}})
	reg.Register("unrelated", &lifecycle.ManifestOnly{Data: manifest.Manifest{Key: "unrelated", Version: "1.0.0"}})

	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })

	warnDanglingContracts(reg, owner)
	out := buf.String()
	if !strings.Contains(out, "installer.contract_removed_with_implementers") || !strings.Contains(out, "acme_pay") || strings.Contains(out, "unrelated") {
		t.Fatalf("want a warning naming acme_pay only, got %q", out)
	}
	buf.Reset()
	warnDanglingContracts(reg, manifest.Manifest{Key: "plain", Version: "1.0.0"})
	warnDanglingContracts(nil, owner)
	if buf.Len() != 0 {
		t.Fatalf("nothing to warn: %q", buf.String())
	}
}
