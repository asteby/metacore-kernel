// compensation.go — ordered reverse compensation for Installer.Install.
//
// Design decision (transaction vs. compensation)
//
// Wrapping schema + tables + migrations in ONE Postgres transaction (DDL is
// transactional in Postgres) was evaluated and rejected:
//
//   - dynamic.ApplyWithOptions deliberately commits ONE transaction per
//     migration and records it in the metacore_addon_migrations ledger;
//     gorm cannot Begin() inside an existing transaction, so the whole
//     schema layer would have to be rewritten (it is outside the installer's
//     ownership) and migrations that need autocommit statements
//     (CREATE INDEX CONCURRENTLY, VACUUM) would break.
//   - Even a perfect SQL transaction cannot cover the non-SQL steps of an
//     install: lifecycle hooks dispatched to addons (HTTP/WASM), the frontend
//     files on disk, the WASM runtime registration and the native sidecar.
//   - A single long transaction would hold DDL locks on shared tables while
//     hooks and file IO run.
//
// Instead Install records, after each side-effecting step, a compensating
// action, and on failure runs them in reverse order:
//
//	native service  → runtime.Remove           (fresh install only)
//	wasm backend    → UnloadAddon              (fresh install only, optional iface)
//	frontend        → delete new dir, restore the previous one from backup
//	i18n catalog    → unregister               (fresh install only)
//	hooks           → unregister, re-register the previous manifest's hooks
//	OnEnable        → OnDisable + disable hook (fresh install only)
//	lifecycle reg.  → restore previous / unregister
//	installation    → fresh: status=failed + last_error; re-install: restore
//	                  the previous row snapshot + last_error
//
// Schema, tables and migrations are NOT undone on purpose: they are additive,
// idempotent and tracked by the migrations ledger, so a retry resumes where
// the failure happened, while dropping them could destroy tenant data.
package installer

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/asteby/metacore-kernel/bundle"
	"github.com/asteby/metacore-kernel/i18n"
	"github.com/asteby/metacore-kernel/lifecycle"
	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/logger"
)

// BackendRuntimeUnloader is an optional extension of BackendRuntimeLoader.
// When the host's loader implements it, a failed Install unloads the addon's
// WASM module that was registered earlier in the same Install.
type BackendRuntimeUnloader interface {
	UnloadAddon(ctx context.Context, addonKey string) error
}

type compStep struct {
	name string
	undo func() error
}

// installTxn accumulates compensations for one Install call.
type installTxn struct {
	i        *Installer
	orgID    uuid.UUID
	b        *bundle.Bundle
	fresh    bool // no installation row existed before this Install
	prev     *Installation
	prevLC   lifecycle.Addon
	prevOK   bool
	persists bool // the installation row was written by this Install
	steps    []compStep
	undone   []string // names of compensations attempted, in execution order
	commits  []func()
}

func (t *installTxn) add(name string, undo func() error) {
	t.steps = append(t.steps, compStep{name, undo})
}

func (t *installTxn) onCommit(f func()) { t.commits = append(t.commits, f) }

// check is called after each completed step: it lets tests inject a failure
// at that exact point (Installer.installFailpoint) and is a no-op otherwise.
func (t *installTxn) check(step string) error {
	if t.i.installFailpoint == nil {
		return nil
	}
	return t.i.installFailpoint(step)
}

func (t *installTxn) commit() {
	for _, f := range t.commits {
		f()
	}
}

// rollback runs the compensations in reverse order. Every step is attempted
// even when an earlier one fails; failures are logged (never the cause's
// secrets) and joined into the returned error.
func (t *installTxn) rollback(cause error) error {
	var errs []error
	for n := len(t.steps) - 1; n >= 0; n-- {
		s := t.steps[n]
		t.undone = append(t.undone, s.name)
		if err := s.undo(); err != nil {
			slog.Error("installer.compensation_failed", "addon", t.b.Manifest.Key, "step", s.name, "err", err.Error())
			errs = append(errs, fmt.Errorf("%s: %w", s.name, err))
		}
	}
	t.i.recordInstallFailure(t, cause)
	if t.i.rollbackObserver != nil {
		t.i.rollbackObserver(t.undone)
	}
	slog.Warn("installer.install_rolled_back", "addon", t.b.Manifest.Key, "version", t.b.Manifest.Version, "steps", len(t.steps), "cause", cause.Error())
	return errors.Join(errs...)
}

func truncateErr(msg string) string {
	if len(msg) > 500 {
		msg = strings.ToValidUTF8(msg[:500], "")
	}
	return msg
}

// beginInstallTxn snapshots what compensation needs to restore.
func (i *Installer) beginInstallTxn(orgID uuid.UUID, b *bundle.Bundle) *installTxn {
	t := &installTxn{i: i, orgID: orgID, b: b, fresh: true}
	var prev Installation
	if err := i.DB.Where("organization_id = ? AND addon_key = ?", orgID, b.Manifest.Key).Take(&prev).Error; err == nil {
		t.prev = &prev
		// A leftover "failed" placeholder means nothing was running: treat
		// the retry as a fresh install for compensation purposes.
		t.fresh = prev.Status == "failed"
	}
	t.prevLC, t.prevOK = i.Lifecycles.Get(b.Manifest.Key)
	return t
}

// recordInstallFailure is the last compensation: it leaves the installation
// table telling the truth about the failed attempt.
func (i *Installer) recordInstallFailure(t *installTxn, cause error) {
	msg := truncateErr(cause.Error())
	key := t.b.Manifest.Key
	switch {
	case t.prev == nil:
		// Fresh install: a row with status=failed makes the failure visible.
		row := &Installation{
			OrganizationID: t.orgID,
			AddonKey:       key,
			Version:        t.b.Manifest.Version,
			Status:         "failed",
			Source:         sourceOf(t.b.Manifest),
		}
		if err := i.DB.Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "organization_id"}, {Name: "addon_key"}},
			DoUpdates: clause.AssignmentColumns([]string{"version", "status", "source"}),
		}).Create(row).Error; err != nil {
			slog.Error("installer.mark_failed_error", "addon", key, "err", err.Error())
		}
		i.setLastError(t.orgID, key, msg)
	case t.persists:
		// Re-install over an existing row that this Install already
		// overwrote: put the previous snapshot back.
		err := i.DB.Model(&Installation{}).
			Where("organization_id = ? AND addon_key = ?", t.orgID, key).
			Updates(map[string]any{
				"version": t.prev.Version, "manifest_hash": t.prev.ManifestHash, "status": t.prev.Status,
				"source": t.prev.Source, "enabled_at": t.prev.EnabledAt,
			}).Error
		if err != nil {
			slog.Error("installer.restore_installation_error", "addon", key, "err", err.Error())
		}
		i.setLastError(t.orgID, key, msg)
	default:
		i.setLastError(t.orgID, key, msg)
	}
}

// guardFrontendKey mirrors WriteFrontend's addon-key validation.
func guardFrontendKey(addonKey string) error {
	c := filepath.Clean(addonKey)
	if c == "" || c == "." || strings.Contains(c, "..") || filepath.IsAbs(c) || strings.ContainsRune(c, filepath.Separator) {
		return fmt.Errorf("frontend: invalid addon key %q", addonKey)
	}
	return nil
}

// stageFrontendBackup moves an existing frontend dir aside so a failed
// re-install can put it back. Returns the backup path ("" when nothing to back up).
func stageFrontendBackup(basePath, addonKey string) (string, error) {
	if basePath == "" {
		return "", nil
	}
	if err := guardFrontendKey(addonKey); err != nil {
		return "", err
	}
	dir := FrontendDir(basePath, addonKey)
	if _, err := os.Stat(dir); err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	bak := filepath.Join(basePath, "."+addonKey+".bak-"+uuid.NewString()[:8])
	if err := os.Rename(dir, bak); err != nil {
		return "", fmt.Errorf("frontend: stage backup: %w", err)
	}
	return bak, nil
}

func restoreHooks(i *Installer, key string, prevLC lifecycle.Addon, prevOK bool) {
	if i.DynamicHooks == nil {
		return
	}
	i.DynamicHooks.UnregisterAddon(key)
	if !prevOK {
		return
	}
	// Compute hooks (RegisterComputeHooks) are not addon-owned in the
	// registry, so they can neither be reverted nor safely re-registered;
	// only the manifest CRUD hooks are restored.
	if i.HookRunner != nil {
		i.DynamicHooks.RegisterManifestHooks(key, prevLC.Manifest(), i.HookRunner)
	}
}

// otherInstallations counts installation rows for addonKey in orgs other than orgID.
func (i *Installer) otherInstallations(orgID uuid.UUID, addonKey string) int64 {
	var n int64
	i.DB.Model(&Installation{}).Where("addon_key = ? AND organization_id <> ?", addonKey, orgID).Count(&n)
	return n
}

func removeI18n(i *Installer, key string) error {
	return i18n.UnregisterAddonI18n(context.Background(), i.DB, key)
}

// ensureLastErrorColumn adds the diagnostic column outside the Installation
// struct on purpose: hosts and tests that hand-roll metacore_installations DDL
// keep working, and the column is only used best-effort (setLastError /
// LastInstallError). Postgres only; failures are ignored.
func ensureLastErrorColumn(db *gorm.DB) {
	if db.Dialector.Name() != "postgres" {
		return
	}
	_ = db.Exec(`ALTER TABLE metacore_installations ADD COLUMN IF NOT EXISTS last_error varchar(500)`).Error
}

// LastInstallError returns the diagnostic of the last failed install attempt
// (or the revocation that disabled the addon); "" when none or unavailable.
func (i *Installer) LastInstallError(orgID uuid.UUID, addonKey string) string {
	var msg *string
	err := i.DB.Session(&gorm.Session{Logger: logger.Discard}).Table("metacore_installations").
		Select("last_error").Where("organization_id = ? AND addon_key = ?", orgID, addonKey).Limit(1).Row().Scan(&msg)
	if err != nil || msg == nil {
		return ""
	}
	return *msg
}
