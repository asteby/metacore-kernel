package installer

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/asteby/metacore-kernel/capability"
	"github.com/asteby/metacore-kernel/lifecycle"
	"github.com/asteby/metacore-kernel/manifest"
	v3 "github.com/asteby/metacore-kernel/manifest/v3"
	"github.com/google/uuid"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func contractManifest(key, ver, contractVer string) manifest.Manifest {
	return manifest.Manifest{Key: key, Version: ver, Contracts: []v3.Contract{
		{Key: "acme.document", Version: contractVer, Operations: []v3.ContractOperation{{Key: "issue", Kind: "action"}}},
	}}
}

// Install / Upgrade record the contracts in the registry of THEIR org: two orgs
// with different versions of the same contract never see each other's.
func TestRegisterDeclaredContracts_PerOrg(t *testing.T) {
	orgA, orgB := uuid.New(), uuid.New()
	regs := map[uuid.UUID]*capability.Registry{orgA: capability.NewRegistry(), orgB: capability.NewRegistry()}
	inst := &Installer{ContractRegistry: func(o uuid.UUID) *capability.Registry { return regs[o] }}

	m := contractManifest("acme_core", "1.0.0", "1.0.0")
	inst.registerDeclaredContracts(orgA, &m)
	m2 := contractManifest("acme_core", "2.0.0", "2.0.0")
	inst.registerDeclaredContracts(orgB, &m2)
	if v, _ := regs[orgA].ContractVersion("acme.document"); v != "1.0.0" {
		t.Fatalf("org A must keep 1.0.0, got %q", v)
	}
	if v, _ := regs[orgB].ContractVersion("acme.document"); v != "2.0.0" {
		t.Fatalf("org B: %q", v)
	}
	// Upgrade in org A replaces only org A.
	up := contractManifest("acme_core", "1.1.0", "1.1.0")
	inst.registerDeclaredContracts(orgA, &up)
	if v, _ := regs[orgA].ContractVersion("acme.document"); v != "1.1.0" {
		t.Fatalf("upgrade must replace in its org: %q", v)
	}
	if v, _ := regs[orgB].ContractVersion("acme.document"); v != "2.0.0" {
		t.Fatalf("org B unaffected by org A's upgrade: %q", v)
	}
	// No wiring, no contracts, or a conflict: never a panic nor a failure.
	(&Installer{}).registerDeclaredContracts(orgA, &m)
	inst.registerDeclaredContracts(orgA, &manifest.Manifest{Key: "plain", Version: "1.0.0"})
	rival := contractManifest("rival_core", "1.0.0", "9.0.0")
	inst.registerDeclaredContracts(orgA, &rival)
	if info, _ := regs[orgA].Known("acme.document"); info.Source != "addon:acme_core" {
		t.Fatalf("the first owner keeps the contract: %+v", info)
	}
	var nilInst *Installer
	nilInst.registerDeclaredContracts(orgA, &m)
}

func newInstallationsDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{DisableForeignKeyConstraintWhenMigrating: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`CREATE TABLE metacore_installations (
		id TEXT PRIMARY KEY, organization_id TEXT, addon_key TEXT, version TEXT, status TEXT, source TEXT,
		secret_hash TEXT, secret_enc TEXT, settings TEXT, manifest_hash TEXT,
		installed_at DATETIME, enabled_at DATETIME, disabled_at DATETIME)`).Error; err != nil {
		t.Fatal(err)
	}
	return db
}

// install -> restart (a brand-new Registry) -> Rehydrate(InstalledContracts):
// the same keys and versions, per org, from the persisted installation rows and
// the manifests the host reloaded.
func TestInstalledContracts_RehydrateAfterRestart(t *testing.T) {
	db := newInstallationsDB(t)
	orgA, orgB := uuid.New(), uuid.New()
	now := time.Now()
	for i, r := range []Installation{
		{OrganizationID: orgA, AddonKey: "acme_core", Version: "1.0.0", Status: "enabled", Source: "bundle", InstalledAt: now},
		{OrganizationID: orgA, AddonKey: "plain", Version: "1.0.0", Status: "enabled", Source: "bundle", InstalledAt: now.Add(time.Second)},
		{OrganizationID: orgA, AddonKey: "rival_core", Version: "1.0.0", Status: "enabled", Source: "bundle", InstalledAt: now.Add(2 * time.Second)},
		{OrganizationID: orgB, AddonKey: "acme_core", Version: "1.0.0", Status: "enabled", Source: "bundle", InstalledAt: now},
		{OrganizationID: orgB, AddonKey: "not_loaded", Version: "1.0.0", Status: "enabled", Source: "bundle", InstalledAt: now},
	} {
		r.ID = uuid.New()
		if err := db.Create(&r).Error; err != nil {
			t.Fatalf("row %d: %v", i, err)
		}
	}
	lcs := lifecycle.NewRegistry()
	core := contractManifest("acme_core", "1.0.0", "1.2.0")
	lcs.Register("acme_core", &lifecycle.ManifestOnly{Data: core})
	lcs.Register("plain", &lifecycle.ManifestOnly{Data: manifest.Manifest{Key: "plain", Version: "1.0.0"}})
	rival := contractManifest("rival_core", "1.0.0", "9.0.0")
	lcs.Register("rival_core", &lifecycle.ManifestOnly{Data: rival})

	var regs = map[uuid.UUID]*capability.Registry{orgA: capability.NewRegistry(), orgB: capability.NewRegistry()}
	inst := &Installer{DB: db, Lifecycles: lcs, ContractRegistry: func(o uuid.UUID) *capability.Registry { return regs[o] }}
	// The live process: installs registered one by one.
	inst.registerDeclaredContracts(orgA, &core)
	inst.registerDeclaredContracts(orgA, &rival)
	inst.registerDeclaredContracts(orgB, &core)

	for _, org := range []uuid.UUID{orgA, orgB} {
		live := regs[org]
		restarted := capability.NewRegistry() // after a restart
		ic, err := inst.InstalledContracts(org)
		if err != nil {
			t.Fatal(err)
		}
		rep := restarted.Rehydrate(ic)
		for _, key := range []string{"acme.document", "acme.document.issue"} {
			a, aok := live.Known(key)
			b, bok := restarted.Known(key)
			if !aok || !bok || a != b {
				t.Errorf("org %s key %q: live=%+v/%v rehydrated=%+v/%v", org, key, a, aok, b, bok)
			}
		}
		if v, _ := restarted.ContractVersion("acme.document"); v != "1.2.0" {
			t.Errorf("rehydrated version = %q", v)
		}
		if org == orgA && len(rep.Failed) != 1 {
			t.Errorf("org A: the later rival owner is reported: %+v", rep)
		}
		if org == orgB && len(rep.Failed) != 0 {
			t.Errorf("org B: %+v", rep)
		}
	}
	// An org with nothing installed rehydrates to an empty registry.
	ic, err := inst.InstalledContracts(uuid.New())
	if err != nil || len(ic) != 0 {
		t.Fatalf("empty org: %v %v", ic, err)
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

// The lifecycle registry keeps ONE manifest per addon key for the whole
// process, while each org persists the version it installed. Rehydrating an org
// whose row differs from the loaded manifest must not hand it the other
// tenant's version (regression: org A on 1.x came back with org B's 2.0).
func TestInstalledContracts_SkipsVersionNotInstalledByTheOrg(t *testing.T) {
	db := newInstallationsDB(t)
	orgA, orgB := uuid.New(), uuid.New()
	now := time.Now()
	for _, r := range []Installation{
		{OrganizationID: orgA, AddonKey: "acme_core", Version: "1.0.0", Status: "enabled", Source: "bundle", InstalledAt: now},
		{OrganizationID: orgB, AddonKey: "acme_core", Version: "2.0.0", Status: "enabled", Source: "bundle", InstalledAt: now},
	} {
		r.ID = uuid.New()
		if err := db.Create(&r).Error; err != nil {
			t.Fatal(err)
		}
	}
	lcs := lifecycle.NewRegistry()
	loaded := contractManifest("acme_core", "2.0.0", "2.0.0") // the process loaded 2.0.0
	lcs.Register("acme_core", &lifecycle.ManifestOnly{Data: loaded})
	inst := &Installer{DB: db, Lifecycles: lcs}

	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })

	icA, err := inst.InstalledContracts(orgA)
	if err != nil {
		t.Fatal(err)
	}
	if len(icA) != 0 {
		t.Fatalf("org A installed 1.0.0 but 2.0.0 is loaded: it must not be fed org B's version, got %+v", icA)
	}
	if !strings.Contains(buf.String(), "installer.contracts_rehydrate_version_mismatch") {
		t.Fatalf("the skip must be visible: %q", buf.String())
	}
	icB, _ := inst.InstalledContracts(orgB)
	reg := capability.NewRegistry()
	reg.Rehydrate(icB)
	if v, _ := reg.ContractVersion("acme.document"); v != "2.0.0" {
		t.Fatalf("org B (2.0.0 installed and loaded) rehydrates 2.0.0, got %q", v)
	}
}

// Implementers: who implements a contract in THIS scope, fed by Install and by
// Rehydrate, including addons that implement a contract without owning one.
func TestInstalledContracts_ImplementersRehydrated(t *testing.T) {
	db := newInstallationsDB(t)
	org := uuid.New()
	now := time.Now()
	for i, k := range []string{"acme_core", "acme_pay", "acme_mail"} {
		r := Installation{ID: uuid.New(), OrganizationID: org, AddonKey: k, Version: "1.0.0", Status: "enabled", Source: "bundle", InstalledAt: now.Add(time.Duration(i) * time.Second)}
		if err := db.Create(&r).Error; err != nil {
			t.Fatal(err)
		}
	}
	lcs := lifecycle.NewRegistry()
	lcs.Register("acme_core", &lifecycle.ManifestOnly{Data: contractManifest("acme_core", "1.0.0", "1.0.0")})
	lcs.Register("acme_pay", &lifecycle.ManifestOnly{Data: manifest.Manifest{Key: "acme_pay", Version: "1.0.0", Implements: []v3.Implements{{Contract: "acme.document", Version: "^1.0"}}}})
	lcs.Register("acme_mail", &lifecycle.ManifestOnly{Data: manifest.Manifest{Key: "acme_mail", Version: "1.0.0", Implements: []v3.Implements{{Contract: "acme.document", Version: ">=1.2.0"}, {Contract: "acme.mail", Version: "^1"}}}})
	inst := &Installer{DB: db, Lifecycles: lcs}

	ic, err := inst.InstalledContracts(org)
	if err != nil {
		t.Fatal(err)
	}
	reg := capability.NewRegistry()
	if rep := reg.Rehydrate(ic); len(rep.Failed) != 0 {
		t.Fatalf("rehydrate: %+v", rep)
	}
	got := reg.Implementers("acme.document")
	if len(got) != 2 || got[0].Owner != "acme_mail" || got[0].Range != ">=1.2.0" || got[1].Owner != "acme_pay" || got[1].Range != "^1.0" {
		t.Fatalf("Implementers(acme.document) = %+v", got)
	}
	if got := reg.Implementers("acme.mail"); len(got) != 1 || got[0].Owner != "acme_mail" {
		t.Fatalf("Implementers(acme.mail) = %+v", got)
	}
	// Install wiring feeds the same query.
	live := capability.NewRegistry()
	w := &Installer{ContractRegistry: func(uuid.UUID) *capability.Registry { return live }}
	pay := manifest.Manifest{Key: "acme_pay", Version: "1.0.0", Implements: []v3.Implements{{Contract: "acme.document", Version: "^1.0"}}}
	w.registerDeclaredContracts(org, &pay)
	if got := live.Implementers("acme.document"); len(got) != 1 || got[0].Owner != "acme_pay" {
		t.Fatalf("install must register implementers: %+v", got)
	}
}
