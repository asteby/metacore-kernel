package installer

import (
	"testing"

	"github.com/asteby/metacore-kernel/lifecycle"
	"github.com/asteby/metacore-kernel/manifest"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// compiledLifecycle is a stand-in for an addon that registered a custom
// (compiled) lifecycle.Addon before Install ran — refreshDeclarativeLifecycle
// must leave it untouched.
type compiledLifecycle struct{ data manifest.Manifest }

func (c *compiledLifecycle) Manifest() manifest.Manifest           { return c.data }
func (c *compiledLifecycle) OnInstall(*gorm.DB, uuid.UUID) error   { return nil }
func (c *compiledLifecycle) OnUninstall(*gorm.DB, uuid.UUID) error { return nil }
func (c *compiledLifecycle) OnEnable(*gorm.DB, uuid.UUID) error    { return nil }
func (c *compiledLifecycle) OnDisable(*gorm.DB, uuid.UUID) error   { return nil }

// Re-install of an already-installed declarative addon must bump the in-memory
// lifecycle version. This is the exact regression behind the marketplace
// "Actualización disponible" badge that stayed stuck after an upgrade: the host
// reports installed versions from this registry, and Install used to skip the
// re-registration when an entry already existed.
func TestRefreshDeclarativeLifecycle_SwapsManifestOnlyToNewVersion(t *testing.T) {
	reg := lifecycle.NewRegistry()
	reg.Register("demo", &lifecycle.ManifestOnly{Data: manifest.Manifest{Key: "demo", Version: "0.1.9"}})

	refreshDeclarativeLifecycle(reg, manifest.Manifest{Key: "demo", Version: "0.1.10"})

	lc, ok := reg.Get("demo")
	if !ok {
		t.Fatal("lifecycle missing after refresh")
	}
	if got := lc.Manifest().Version; got != "0.1.10" {
		t.Fatalf("lifecycle version = %q, want 0.1.10 (stale registry = stuck update badge)", got)
	}
}

// A first install (nothing registered yet) must register the manifest.
func TestRefreshDeclarativeLifecycle_RegistersWhenAbsent(t *testing.T) {
	reg := lifecycle.NewRegistry()

	refreshDeclarativeLifecycle(reg, manifest.Manifest{Key: "demo", Version: "1.0.0"})

	lc, ok := reg.Get("demo")
	if !ok {
		t.Fatal("lifecycle not registered on first install")
	}
	if lc.Manifest().Version != "1.0.0" {
		t.Fatalf("version = %q, want 1.0.0", lc.Manifest().Version)
	}
}

// A compiled addon's custom lifecycle must NOT be clobbered by a declarative
// shim on re-install — that would drop its OnInstall/Boot behaviour.
func TestRefreshDeclarativeLifecycle_PreservesCompiledLifecycle(t *testing.T) {
	reg := lifecycle.NewRegistry()
	custom := &compiledLifecycle{data: manifest.Manifest{Key: "demo", Version: "0.1.9"}}
	reg.Register("demo", custom)

	refreshDeclarativeLifecycle(reg, manifest.Manifest{Key: "demo", Version: "0.1.10"})

	lc, _ := reg.Get("demo")
	if _, isManifestOnly := lc.(*lifecycle.ManifestOnly); isManifestOnly {
		t.Fatal("compiled lifecycle was replaced by a declarative ManifestOnly shim")
	}
	if lc != lifecycle.Addon(custom) {
		t.Fatal("compiled lifecycle instance was swapped out")
	}
}
