package installer

// Failure-injection tests for Install compensation (see compensation.go).
// Run against a real Postgres (TEST_POSTGRES_DSN); skipped otherwise.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/asteby/metacore-kernel/bundle"
	"github.com/asteby/metacore-kernel/dynamic"
	"github.com/asteby/metacore-kernel/lifecycle"
	"github.com/asteby/metacore-kernel/manifest"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// trackedLC counts OnDisable calls (compensation of OnEnable).
type trackedLC struct {
	*lifecycle.ManifestOnly
	mu       sync.Mutex
	disabled int
}

func (l *trackedLC) OnDisable(db *gorm.DB, org uuid.UUID) error {
	l.mu.Lock()
	l.disabled++
	l.mu.Unlock()
	return nil
}

type fakeBackend struct {
	mu       sync.Mutex
	loaded   int
	unloaded int
}

func (f *fakeBackend) LoadFromBundle(context.Context, *bundle.Bundle) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.loaded++
	return nil
}
func (f *fakeBackend) UnloadAddon(context.Context, string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.unloaded++
	return nil
}

type compFixture struct {
	db      *gorm.DB
	inst    *Installer
	b       *bundle.Bundle
	org     uuid.UUID
	key     string
	fe      string
	backend *fakeBackend
	undone  []string
}

func newCompFixture(t *testing.T) *compFixture {
	t.Helper()
	db, key, _, migs, m := freshInstallFixture(t)
	t.Cleanup(func() { db.Exec(`DELETE FROM public.metacore_addon_i18n WHERE addon_key = ?`, key) })
	m.Frontend = &manifest.FrontendSpec{Entry: "remoteEntry.js", Format: "federation", Expose: "./plugin"}
	m.Backend = &manifest.BackendSpec{Runtime: "wasm", Entry: "backend/backend.wasm"}
	f := &compFixture{db: db, key: key, org: uuid.New(), fe: t.TempDir(), backend: &fakeBackend{}}
	f.b = &bundle.Bundle{Manifest: m, Migrations: migs,
		Frontend: map[string][]byte{"frontend/remoteEntry.js": []byte("v1")}}
	f.inst = freshInstaller(db).WithModelMaterializer(hostMaterializer)
	f.inst.FrontendBasePath = f.fe
	f.inst.BackendRuntime = f.backend
	f.inst.DynamicHooks = dynamic.NewHookRegistry()
	f.inst.rollbackObserver = func(u []string) { f.undone = u }
	return f
}

func (f *compFixture) failAt(step string) {
	f.undone = nil
	f.inst.installFailpoint = func(s string) error {
		if s == step {
			return fmt.Errorf("injected@%s", step)
		}
		return nil
	}
}

func (f *compFixture) row(t *testing.T) *Installation {
	t.Helper()
	var r Installation
	if err := f.db.Where("organization_id = ? AND addon_key = ?", f.org, f.key).Take(&r).Error; err != nil {
		return nil
	}
	return &r
}

func (f *compFixture) backupDirs(t *testing.T) []string {
	m, _ := filepath.Glob(filepath.Join(f.fe, ".*.bak-*"))
	return m
}

// Fresh install: whichever step fails, nothing keeps running, the row says
// failed with the cause, and a plain retry succeeds and heals the row.
func TestInstall_Compensation_FreshInstallEveryStep(t *testing.T) {
	steps := []struct {
		step string
		// compensations that must have run (subset check, order irrelevant)
		undone []string
	}{
		{"schema", nil},
		{"models", nil},
		{"migrations", nil},
		{"lifecycle_install", []string{"lifecycle_registry"}},
		{"lifecycle_enable", []string{"lifecycle_enable", "lifecycle_registry"}},
		{"hooks", []string{"hooks", "lifecycle_enable", "lifecycle_registry"}},
		{"persist", []string{"hooks", "lifecycle_enable", "lifecycle_registry"}},
		{"i18n", []string{"i18n", "hooks", "lifecycle_enable", "lifecycle_registry"}},
		{"frontend", []string{"frontend", "i18n", "hooks", "lifecycle_enable", "lifecycle_registry"}},
		{"backend", []string{"backend_runtime", "frontend", "i18n", "hooks", "lifecycle_enable", "lifecycle_registry"}},
	}
	for _, tc := range steps {
		t.Run(tc.step, func(t *testing.T) {
			f := newCompFixture(t)
			f.failAt(tc.step)

			_, _, err := f.inst.Install(f.org, f.b)
			if err == nil || !strings.Contains(err.Error(), "injected@"+tc.step) {
				t.Fatalf("want injected failure, got %v", err)
			}
			for _, want := range tc.undone {
				if !slices.Contains(f.undone, want) {
					t.Errorf("compensation %q did not run; ran %v", want, f.undone)
				}
			}
			// Reverse order: whatever ran must be sorted last-registered first.
			if i, j := slices.Index(f.undone, "lifecycle_registry"), slices.Index(f.undone, "hooks"); i >= 0 && j >= 0 && j > i {
				t.Errorf("compensation not in reverse order: %v", f.undone)
			}
			if _, ok := f.inst.Lifecycles.Get(f.key); ok {
				t.Error("lifecycle registration leaked after a failed install")
			}
			if _, err := os.Stat(filepath.Join(f.fe, f.key)); !os.IsNotExist(err) {
				t.Error("frontend dir left behind")
			}
			if f.backend.loaded != f.backend.unloaded {
				t.Errorf("backend loaded=%d unloaded=%d", f.backend.loaded, f.backend.unloaded)
			}
			row := f.row(t)
			if row == nil || row.Status != "failed" {
				t.Fatalf("installation must be marked failed, got %+v", row)
			}
			if msg := f.inst.LastInstallError(f.org, f.key); !strings.Contains(msg, "injected@"+tc.step) {
				t.Errorf("last_error = %q", msg)
			}

			// Retry (no fault): heals the row and the returned secret is the stored one.
			f.inst.installFailpoint = nil
			inst, secret, err := f.inst.Install(f.org, f.b)
			if err != nil {
				t.Fatalf("retry: %v", err)
			}
			row = f.row(t)
			if row.Status != "enabled" || inst.Status != "enabled" {
				t.Errorf("retry status = %s", row.Status)
			}
			if msg := f.inst.LastInstallError(f.org, f.key); msg != "" {
				t.Errorf("last_error not cleared: %q", msg)
			}
			if row.SecretHash != hashSecret(secret) {
				t.Error("stored secret hash does not match the secret returned by the retry")
			}
			if b, _ := os.ReadFile(filepath.Join(f.fe, f.key, "remoteEntry.js")); string(b) != "v1" {
				t.Error("frontend not materialised on retry")
			}
		})
	}
}

func TestInstall_Compensation_OnEnableUndoneWithOnDisable(t *testing.T) {
	f := newCompFixture(t)
	f.b.Manifest.Frontend, f.b.Manifest.Backend = nil, nil
	lc := &trackedLC{ManifestOnly: &lifecycle.ManifestOnly{Data: f.b.Manifest}}
	f.inst.Lifecycles.Register(f.key, lc)
	f.failAt("hooks")
	if _, _, err := f.inst.Install(f.org, f.b); err == nil {
		t.Fatal("want failure")
	}
	if lc.disabled != 1 {
		t.Fatalf("OnDisable must compensate OnEnable exactly once, got %d", lc.disabled)
	}
}

// Re-install over a healthy installation: a failure after the row/frontend
// were overwritten restores the previous version, row and frontend files.
func TestInstall_Compensation_ReinstallRestoresPreviousState(t *testing.T) {
	for _, step := range []string{"persist", "i18n", "frontend", "backend"} {
		t.Run(step, func(t *testing.T) {
			f := newCompFixture(t)
			if _, _, err := f.inst.Install(f.org, f.b); err != nil {
				t.Fatalf("v1: %v", err)
			}
			v1 := f.row(t)
			loadedBefore := f.backend.loaded

			b2 := &bundle.Bundle{Manifest: f.b.Manifest, Migrations: f.b.Migrations,
				Frontend: map[string][]byte{"frontend/remoteEntry.js": []byte("v2"), "frontend/new.js": []byte("n")}}
			b2.Manifest.Version = "1.1.0"
			f.failAt(step)
			if _, _, err := f.inst.Install(f.org, b2); err == nil {
				t.Fatal("want failure")
			}
			row := f.row(t)
			if row.Version != "1.0.0" || row.Status != "enabled" || row.ManifestHash != v1.ManifestHash {
				t.Errorf("previous row not preserved: %+v", row)
			}
			if msg := f.inst.LastInstallError(f.org, f.key); !strings.Contains(msg, "injected@"+step) {
				t.Errorf("last_error = %q", msg)
			}
			if b, _ := os.ReadFile(filepath.Join(f.fe, f.key, "remoteEntry.js")); string(b) != "v1" {
				t.Errorf("previous frontend not restored: %q", b)
			}
			if _, err := os.Stat(filepath.Join(f.fe, f.key, "new.js")); !os.IsNotExist(err) {
				t.Error("new frontend file leaked")
			}
			if n := len(f.backupDirs(t)); n != 0 {
				t.Errorf("%d backup dirs left", n)
			}
			// Re-install is not "fresh": the running v1 backend must not be unloaded.
			if f.backend.unloaded != 0 {
				t.Errorf("v1 backend unloaded on failed reinstall (loaded %d->%d)", loadedBefore, f.backend.loaded)
			}
			// The registry still reports the addon (it was installed before).
			if _, ok := f.inst.Lifecycles.Get(f.key); !ok {
				t.Error("lifecycle registration lost on failed reinstall")
			}
			// And a good v2 afterwards commits and leaves no backup behind.
			f.inst.installFailpoint = nil
			if _, _, err := f.inst.Install(f.org, b2); err != nil {
				t.Fatalf("v2: %v", err)
			}
			if r := f.row(t); r.Version != "1.1.0" {
				t.Errorf("v2 version = %s", r.Version)
			}
			if n := len(f.backupDirs(t)); n != 0 {
				t.Errorf("%d backup dirs after commit", n)
			}
			if b, _ := os.ReadFile(filepath.Join(f.fe, f.key, "remoteEntry.js")); string(b) != "v2" {
				t.Errorf("v2 frontend = %q", b)
			}
		})
	}
}

// A failing real step (not injected): a broken migration after schema/model
// creation leaves a failed row and no lifecycle registration.
func TestInstall_Compensation_RealMigrationFailure(t *testing.T) {
	f := newCompFixture(t)
	f.b.Migrations = append(f.b.Migrations, dynamic.File{Version: "009_bad", SQL: `SELECT * FROM table_that_does_not_exist_xyz;`})
	_, _, err := f.inst.Install(f.org, f.b)
	if err == nil {
		t.Fatal("want migration failure")
	}
	if r := f.row(t); r == nil || r.Status != "failed" {
		t.Fatalf("want failed row, got %+v", r)
	}
	if msg := f.inst.LastInstallError(f.org, f.key); !strings.Contains(msg, "table_that_does_not_exist_xyz") {
		t.Errorf("last_error = %q", msg)
	}
	if _, ok := f.inst.Lifecycles.Get(f.key); ok {
		t.Error("lifecycle leaked")
	}
	// Fixing the bundle and retrying resumes (earlier migrations are in the ledger).
	f.b.Migrations = f.b.Migrations[:len(f.b.Migrations)-1]
	if _, _, err := f.inst.Install(f.org, f.b); err != nil {
		t.Fatalf("retry after fix: %v", err)
	}
}

// Validation-stage rejections (signature, revocation) write nothing at all.
func TestInstall_Compensation_PreStateRejectionsWriteNoRow(t *testing.T) {
	f := newCompFixture(t)
	f.inst.WithRevocations(staticRevocations{list: revList(hub1(f.key))})
	_, _, err := f.inst.Install(f.org, f.b)
	if !errors.Is(err, ErrRevoked) {
		t.Fatalf("want ErrRevoked, got %v", err)
	}
	if r := f.row(t); r != nil {
		t.Fatalf("revoked install must not leave a row: %+v", r)
	}
}
