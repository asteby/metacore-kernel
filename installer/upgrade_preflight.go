package installer

import (
	"errors"
	"fmt"

	"github.com/asteby/metacore-kernel/bundle"
	"github.com/asteby/metacore-kernel/dynamic"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// UpgradePhase names the stage of an upgrade that failed.
type UpgradePhase string

const (
	// UpgradePhaseFetch: the host could not obtain / parse the new bundle
	// (hosts raise it with NewUpgradeError; the installer itself receives an
	// already-read bundle).
	UpgradePhaseFetch UpgradePhase = "fetch"
	// UpgradePhaseValidate: manifest validation, runtime compatibility or the
	// compiled-handler gate refused the new bundle.
	UpgradePhaseValidate UpgradePhase = "validate"
	// UpgradePhaseSchema: EnsureSchema / CreateTable / SyncSchema / model
	// materialisation failed.
	UpgradePhaseSchema UpgradePhase = "schema"
	// UpgradePhaseMigrations: a SQL migration of the new bundle failed.
	UpgradePhaseMigrations UpgradePhase = "migrations"
)

// UpgradeError is the typed failure of Installer.Upgrade for the validate,
// schema and migrations phases. The installed version is left untouched
// whenever Preflight is true (the failure was found by the dry run, before any
// state changed). Err keeps the original error: errors.Is / errors.As see
// through it.
type UpgradeError struct {
	Addon       string
	FromVersion string
	Version     string // target version
	Phase       UpgradePhase
	// Preflight is true when the dry run caught the failure: nothing was
	// applied and the installation row is exactly as before.
	Preflight bool
	Err       error
}

func (e *UpgradeError) Error() string {
	pre := ""
	if e.Preflight {
		pre = " (preflight, nothing applied)"
	}
	return fmt.Sprintf("installer.Upgrade: %s %s@%s%s: %v", e.Phase, e.Addon, e.Version, pre, e.Err)
}

func (e *UpgradeError) Unwrap() error { return e.Err }

// NewUpgradeError lets hosts type their own failures (e.g. UpgradePhaseFetch)
// with the same shape as the installer's.
func NewUpgradeError(addon, from, to string, phase UpgradePhase, err error) *UpgradeError {
	return &UpgradeError{Addon: addon, FromVersion: from, Version: to, Phase: phase, Err: err}
}

// phaseErr is what a schemaApplier returns to tell Upgrade which phase failed.
type phaseErr struct {
	phase UpgradePhase
	err   error
}

func (p *phaseErr) Error() string { return p.err.Error() }
func (p *phaseErr) Unwrap() error { return p.err }

func inPhase(phase UpgradePhase, err error) error {
	if err == nil {
		return nil
	}
	return &phaseErr{phase: phase, err: err}
}

// upgradeFailure builds the UpgradeError for err, taking the phase from a
// phaseErr when the applier supplied one and defaulting to def otherwise.
func upgradeFailure(addon, from, to string, def UpgradePhase, preflight bool, err error) *UpgradeError {
	phase := def
	var pe *phaseErr
	if errors.As(err, &pe) {
		phase = pe.phase
	}
	return &UpgradeError{Addon: addon, FromVersion: from, Version: to, Phase: phase, Preflight: preflight, Err: err}
}

// upgradeDryRunner is implemented by appliers that can rehearse the schema +
// migration work of an upgrade without persisting anything. Appliers that do
// not implement it (test doubles) are simply not pre-flighted.
type upgradeDryRunner interface {
	DryRunForUpgrade(db *gorm.DB, orgID uuid.UUID, iso dynamic.Isolation, b *bundle.Bundle) error
}

// errDryRunRollback is the sentinel that makes the dry-run transaction roll
// back after a successful rehearsal.
var errDryRunRollback = errors.New("installer: dry-run rollback")

// DryRunForUpgrade rehearses ApplyForUpgrade inside ONE transaction that is
// always rolled back: Postgres DDL is transactional, so the model tables and
// every pending migration are exercised against the live schema and then
// undone. Failures come back tagged with their phase (schema | migrations).
// Postgres only; any other dialect skips the rehearsal.
func (a defaultSchemaApplier) DryRunForUpgrade(db *gorm.DB, orgID uuid.UUID, iso dynamic.Isolation, b *bundle.Bundle) error {
	if db.Dialector == nil || db.Dialector.Name() != "postgres" {
		return nil
	}
	// The ledger table is created outside the rehearsal (idempotent, what
	// ApplyWithOptions does first anyway) so the dry run can read it.
	if err := db.AutoMigrate(&dynamic.Migration{}); err != nil {
		return inPhase(UpgradePhaseMigrations, fmt.Errorf("migrate metacore_addon_migrations: %w", err))
	}
	err := db.Transaction(func(tx *gorm.DB) error {
		if err := a.applyModels(tx, orgID, iso, b); err != nil {
			return inPhase(UpgradePhaseSchema, err)
		}
		if err := dynamic.DryRunMigrations(tx, b.Manifest.Key, orgID, iso, b.Migrations, migrationOptions(a.migrationSchema, b.Manifest)); err != nil {
			return inPhase(UpgradePhaseMigrations, fmt.Errorf("apply migrations: %w", err))
		}
		return errDryRunRollback
	})
	if errors.Is(err, errDryRunRollback) {
		return nil
	}
	return err
}
