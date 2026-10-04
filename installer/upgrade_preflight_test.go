package installer

import (
	"context"
	"errors"
	"testing"

	"github.com/asteby/metacore-kernel/bundle"
	"github.com/asteby/metacore-kernel/dynamic"
	"github.com/asteby/metacore-kernel/lifecycle"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// dryRunApplier is a recordingApplier that also rehearses: it fails the dry
// run with a phase-tagged error, like defaultSchemaApplier does on Postgres.
type dryRunApplier struct {
	recordingApplier
	dryErr error
	dryRan int
}

func (d *dryRunApplier) DryRunForUpgrade(_ *gorm.DB, _ uuid.UUID, _ dynamic.Isolation, _ *bundle.Bundle) error {
	d.dryRan++
	return d.dryErr
}

func preflightInstaller(db *gorm.DB, a schemaApplier) *Installer {
	return &Installer{DB: db, KernelVersion: "2.0.0", Lifecycles: lifecycle.NewRegistry(),
		AllowUnsigned: true, schemaApplier: a, Broadcaster: NoopBroadcaster{}}
}

// A failing migration found by the dry run: typed error (addon, version,
// phase, original message), installed version intact, and NOTHING of the real
// apply ran (no schema applier call, no before hook).
func TestUpgrade_Preflight_FailingMigration_TypedErrorVersionIntact(t *testing.T) {
	db := upgradeTestDB(t)
	orgID := uuid.New()
	seedInstallation(t, db, orgID, "demo", "1.0.0", map[string]any{"old": "kept"})

	boom := errors.New(`apply demo@0002_boom: pq: syntax error at or near "BOOM"`)
	app := &dryRunApplier{dryErr: inPhase(UpgradePhaseMigrations, boom)}
	_, err := preflightInstaller(db, app).Upgrade(context.Background(), orgID,
		makeBundle("demo", "1.1.0", []dynamic.File{{Version: "0002_boom", SQL: "BOOM"}}))

	var ue *UpgradeError
	if !errors.As(err, &ue) {
		t.Fatalf("want *UpgradeError, got %T %v", err, err)
	}
	if ue.Addon != "demo" || ue.Version != "1.1.0" || ue.FromVersion != "1.0.0" ||
		ue.Phase != UpgradePhaseMigrations || !ue.Preflight || !errors.Is(err, boom) {
		t.Fatalf("unexpected UpgradeError: %+v", ue)
	}
	if app.dryRan != 1 || app.callCount() != 0 {
		t.Fatalf("dry runs=%d real applies=%d; the real apply must not run after a failed rehearsal", app.dryRan, app.callCount())
	}
	var got Installation
	if err := db.Where("organization_id = ? AND addon_key = ?", orgID, "demo").Take(&got).Error; err != nil {
		t.Fatal(err)
	}
	if got.Version != "1.0.0" || got.Settings["old"] != "kept" {
		t.Fatalf("installation mutated: %+v", got)
	}
}

// A passing rehearsal is followed by the real apply; a real-apply failure is
// typed too (not preflight) and keeps the row at the old version.
func TestUpgrade_Preflight_PassThenRealApplyFailure_Typed(t *testing.T) {
	db := upgradeTestDB(t)
	orgID := uuid.New()
	seedInstallation(t, db, orgID, "demo", "1.0.0", nil)
	app := &dryRunApplier{}
	app.err = inPhase(UpgradePhaseSchema, errors.New("CreateTable X: boom"))
	_, err := preflightInstaller(db, app).Upgrade(context.Background(), orgID, makeBundle("demo", "1.1.0", nil))
	var ue *UpgradeError
	if !errors.As(err, &ue) || ue.Phase != UpgradePhaseSchema || ue.Preflight {
		t.Fatalf("want non-preflight schema UpgradeError, got %v", err)
	}
	if app.dryRan != 1 || app.callCount() != 1 {
		t.Fatalf("dry=%d real=%d", app.dryRan, app.callCount())
	}
	var got Installation
	_ = db.Where("organization_id = ? AND addon_key = ?", orgID, "demo").Take(&got).Error
	if got.Version != "1.0.0" {
		t.Fatalf("version = %q", got.Version)
	}
}

// A bundle read non-strictly carries legacy-tolerated findings; Upgrade logs
// them and proceeds instead of aborting (the #453 incident).
func TestUpgrade_ToleratedValidationWarnings_DoNotAbort(t *testing.T) {
	db := upgradeTestDB(t)
	orgID := uuid.New()
	seedInstallation(t, db, orgID, "demo", "1.0.0", nil)
	b := makeBundle("demo", "1.1.0", nil)
	b.ValidationWarnings = []string{`[requires_state_in_stages] contributions.actions[0].requires_state "x" is not a stage of model "M"`}
	got, err := preflightInstaller(db, &recordingApplier{}).Upgrade(context.Background(), orgID, b)
	if err != nil || got.Version != "1.1.0" {
		t.Fatalf("upgrade must proceed: %v", err)
	}
}

// Validation failures are typed with the validate phase and keep Unwrap.
func TestUpgrade_ValidationFailure_TypedValidatePhase(t *testing.T) {
	db := upgradeTestDB(t)
	orgID := uuid.New()
	seedInstallation(t, db, orgID, "demo", "1.0.0", nil)
	b := makeBundle("demo", "1.1.0", nil)
	b.Manifest.Name = ""
	_, err := preflightInstaller(db, &recordingApplier{}).Upgrade(context.Background(), orgID, b)
	var ue *UpgradeError
	if !errors.As(err, &ue) || ue.Phase != UpgradePhaseValidate || ue.Addon != "demo" || ue.Version != "1.1.0" {
		t.Fatalf("want validate UpgradeError, got %v", err)
	}
}
