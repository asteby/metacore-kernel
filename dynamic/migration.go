package dynamic

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// Migration is a versioned SQL file applied to the addon's schema.
// Each migration is identified by (addon_key, version) and locked by checksum
// so tampered files are rejected at apply time.
type Migration struct {
	ID        uint64    `gorm:"primaryKey"`
	AddonKey  string    `gorm:"size:100;not null;uniqueIndex:idx_addon_ver"`
	// Was size:40 — tight enough that a normal descriptive filename
	// ("<n>_<what>_<why>.up", e.g. pos@017_sale_payment_organization_id_tenant_schema)
	// blew past it and aborted the upgrade with SQLSTATE 22001 before running
	// any SQL (see asteby-hq/addons#930). AutoMigrate widens the column on
	// existing installs; no manual ALTER needed.
	Version   string    `gorm:"size:100;not null;uniqueIndex:idx_addon_ver"`
	Checksum  string    `gorm:"size:64;not null"`
	AppliedAt time.Time `gorm:"autoCreateTime"`
}

func (Migration) TableName() string { return "metacore_addon_migrations" }

// File is an unapplied migration candidate loaded from a bundle.
type File struct {
	Version string // e.g. "0001_init"
	SQL     string
}

// Checksum returns the sha256 of a migration file's SQL content.
func Checksum(sql string) string {
	h := sha256.Sum256([]byte(sql))
	return hex.EncodeToString(h[:])
}

// Apply runs each pending migration inside its own transaction, scoped to
// the addon schema. If a migration is already applied it is skipped; if its
// on-disk checksum diverges from what was recorded, Apply returns an error
// instead of silently re-running mutated SQL.
func Apply(db *gorm.DB, addonKey string, orgID uuid.UUID, iso Isolation, files []File) error {
	return ApplyWithOptions(db, addonKey, orgID, iso, files, ApplyOptions{})
}

// ApplyOptions lets the host tell the migration runner where the addon's
// model tables really live.
//
// By default every migration runs with `search_path TO addon_<key>, public`:
// bare names resolve to the addon schema first. That is right for hosts that
// serve the addon's models from its own schema (the kernel's CreateTable
// materialises them there). It is wrong for a host that materialises the
// models in another schema — ops creates every declared model in `public` and
// the runtime reads them unqualified — while `addon_<key>` still carries empty
// twins of the same tables (kernel CreateTable, old migrations). There a bare
// `ALTER TABLE stock …`, `CREATE INDEX … ON stock(…)` or `to_regclass('stock')`
// resolves to the empty twin instead of the live table (inventory@017 "column
// deleted_at does not exist", fiscal_mexico@006 xml_content, purchases@008
// index on the empty table).
type ApplyOptions struct {
	// PrimarySchema is the schema the host serves this addon's model tables
	// from (ops: "public"). Empty, or equal to the addon schema, keeps the
	// default order.
	PrimarySchema string
	// ModelTables are the addon's model table names (manifest
	// model_definitions[].table_name). PrimarySchema is only put first when at
	// least one of them already exists there, i.e. the host has materialised
	// the addon's models in it. On a first install, before the host created
	// anything, the order stays the default one.
	ModelTables []string
}

// ApplyWithOptions is Apply with a host-declared primary schema. The decision
// is taken per migration, right before it runs, so a migration that creates a
// model table is seen by the next one. Only the search_path of pending
// migrations changes: already-applied files are skipped exactly as in Apply
// (the ledger is untouched), and migrations that qualify their tables
// (`public.stock`, loops over pg_namespace) resolve the same either way.
func ApplyWithOptions(db *gorm.DB, addonKey string, orgID uuid.UUID, iso Isolation, files []File, opts ApplyOptions) error {
	if err := db.AutoMigrate(&Migration{}); err != nil {
		return fmt.Errorf("migrate metacore_addon_migrations: %w", err)
	}
	schema := SchemaName(addonKey, orgID, iso)
	for _, f := range files {
		got := Checksum(f.SQL)
		var existing Migration
		// Fresh statement each iteration — gorm chains Where() on a reused *DB,
		// so a loop-local ledger handle made every lookup AND all prior versions
		// together, never matched a row, and re-ran the whole migration history
		// on every upgrade (breaking on legacy SQL like customers@004).
		err := db.Table("public.metacore_addon_migrations").
			Where("addon_key = ? AND version = ?", addonKey, f.Version).
			First(&existing).Error
		if err == nil {
			if existing.Checksum != got {
				if !declaresInPlaceEdit(f.SQL) {
					return fmt.Errorf(
						"migration %s@%s checksum mismatch: recorded %s, file %s (refusing to re-apply mutated SQL)",
						addonKey, f.Version, existing.Checksum, got)
				}
				// The author declared this file was edited after publishing
				// (an exception to immutability, always idempotent SQL). It is
				// already applied, so skip it — never re-run it — and pin the
				// new checksum so the drift is reported once, not every upgrade.
				log.Printf("dynamic: migration %s@%s edited in place (recorded %s, file %s): keeping it applied, updating ledger checksum",
					addonKey, f.Version, existing.Checksum, got)
				if err := db.Table("public.metacore_addon_migrations").
					Where("addon_key = ? AND version = ?", addonKey, f.Version).
					Update("checksum", got).Error; err != nil {
					return fmt.Errorf("update ledger checksum %s@%s: %w", addonKey, f.Version, err)
				}
			}
			continue
		}
		if !isNotFound(err) {
			return err
		}
		tx := db.Begin()
		// Scope session search_path so bare table names resolve to the schema
		// the addon's model tables really live in (see ApplyOptions).
		path, err := migrationSearchPath(tx, schema, opts)
		if err != nil {
			tx.Rollback()
			return fmt.Errorf("apply %s@%s: resolve search_path: %w", addonKey, f.Version, err)
		}
		if path[0] != schema {
			log.Printf("dynamic: migration %s@%s runs with search_path %s (addon models live in %s)",
				addonKey, f.Version, strings.Join(path, ", "), path[0])
		}
		if err := tx.Exec(`SET LOCAL search_path TO ` + quoteIdents(path)).Error; err != nil {
			tx.Rollback()
			return err
		}
		if err := execScript(tx, f.SQL); err != nil {
			if !isBenignDDLConflict(err) {
				tx.Rollback()
				return fmt.Errorf("apply %s@%s: %w", addonKey, f.Version, err)
			}
			// Postgres aborts the whole tx after any error (SQLSTATE 25P02).
			// Objects already exist from a partial prior run — roll back and
			// record the ledger row in a fresh transaction (always public).
			tx.Rollback()
			if err := recordMigration(db, addonKey, f.Version, got); err != nil {
				return err
			}
			continue
		}
		// Reset search_path before writing the ledger so a migration file that
		// issued SET search_path (non-LOCAL) cannot redirect the INSERT into
		// addon_<key>.metacore_addon_migrations (local-dev 23505 on retry).
		if err := tx.Exec(`SET LOCAL search_path TO public`).Error; err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Table("public.metacore_addon_migrations").Create(&Migration{AddonKey: addonKey, Version: f.Version, Checksum: got}).Error; err != nil {
			tx.Rollback()
			if isBenignDDLConflict(err) {
				// Concurrent retry / already recorded in public — treat as applied.
				continue
			}
			return err
		}
		if err := tx.Commit().Error; err != nil {
			return err
		}
	}
	return nil
}

// DryRunMigrations executes the pending migrations of files on tx exactly the
// way ApplyWithOptions would (same search_path, same checksum refusal, same
// benign "already exists" tolerance) but never writes the ledger. tx MUST be an
// open transaction the caller rolls back: Postgres DDL is transactional, so
// nothing is left behind and a failing file surfaces BEFORE the real Apply
// touches the installation. Each file runs under a savepoint so a benign
// conflict does not poison the transaction for the next file, and later files
// see the effects of earlier ones just as in a real run. The ledger table is
// read, never created: it exists whenever the addon has been installed.
func DryRunMigrations(tx *gorm.DB, addonKey string, orgID uuid.UUID, iso Isolation, files []File, opts ApplyOptions) error {
	schema := SchemaName(addonKey, orgID, iso)
	for i, f := range files {
		got := Checksum(f.SQL)
		var existing Migration
		err := tx.Table("public.metacore_addon_migrations").
			Where("addon_key = ? AND version = ?", addonKey, f.Version).
			First(&existing).Error
		if err == nil {
			if existing.Checksum != got && !declaresInPlaceEdit(f.SQL) {
				return fmt.Errorf(
					"migration %s@%s checksum mismatch: recorded %s, file %s (refusing to re-apply mutated SQL)",
					addonKey, f.Version, existing.Checksum, got)
			}
			continue
		}
		if !isNotFound(err) {
			return err
		}
		sp := fmt.Sprintf("metacore_dryrun_%d", i)
		if err := tx.SavePoint(sp).Error; err != nil {
			return err
		}
		path, err := migrationSearchPath(tx, schema, opts)
		if err != nil {
			return fmt.Errorf("dry-run %s@%s: resolve search_path: %w", addonKey, f.Version, err)
		}
		if err := tx.Exec(`SET LOCAL search_path TO ` + quoteIdents(path)).Error; err != nil {
			return err
		}
		if err := execScript(tx, f.SQL); err != nil {
			if !isBenignDDLConflict(err) {
				return fmt.Errorf("apply %s@%s: %w", addonKey, f.Version, err)
			}
			if rerr := tx.RollbackTo(sp).Error; rerr != nil {
				return rerr
			}
		}
		if err := tx.Exec(`SET LOCAL search_path TO public`).Error; err != nil {
			return err
		}
	}
	return nil
}

// replayAfterModelsMarker tags a migration whose effect depends on the host's
// model tables existing (a trigger, a default or a generated helper on a
// table the manifest materialises). Some hosts (ops) create those tables
// AFTER the installer ran the migrations, so on a first install the migration
// found nothing to attach to and the ledger recorded it as applied.
var replayAfterModelsMarker = regexp.MustCompile(`(?im)^\s*--\s*metacore:\s*replay-after-models\b`)

func declaresReplayAfterModels(sql string) bool { return replayAfterModelsMarker.MatchString(sql) }

// ReplayAfterModels re-runs, outside the ledger, every migration that carries
// the `-- metacore: replay-after-models` marker. Hosts call it once the addon's
// model tables exist (after their own CreateTable / HotRegister step), on every
// install, upgrade and boot re-register. The marked SQL MUST be idempotent
// (CREATE OR REPLACE, DROP … IF EXISTS, ADD COLUMN IF NOT EXISTS): it runs many
// times, each with the same search_path a first-time Apply would use now that
// the model tables exist. The ledger is not touched, so the file's checksum
// rules are unchanged. Unmarked migrations are never replayed.
func ReplayAfterModels(db *gorm.DB, addonKey string, orgID uuid.UUID, iso Isolation, files []File, opts ApplyOptions) error {
	schema := SchemaName(addonKey, orgID, iso)
	for _, f := range files {
		if !declaresReplayAfterModels(f.SQL) {
			continue
		}
		tx := db.Begin()
		path, err := migrationSearchPath(tx, schema, opts)
		if err != nil {
			tx.Rollback()
			return fmt.Errorf("replay %s@%s: resolve search_path: %w", addonKey, f.Version, err)
		}
		if err := tx.Exec(`SET LOCAL search_path TO ` + quoteIdents(path)).Error; err != nil {
			tx.Rollback()
			return err
		}
		if err := execScript(tx, f.SQL); err != nil {
			tx.Rollback()
			return fmt.Errorf("replay %s@%s: %w", addonKey, f.Version, err)
		}
		if err := tx.Commit().Error; err != nil {
			return err
		}
	}
	return nil
}

// migrationSearchPath returns the schemas, in order, a pending migration of
// the addon runs with. Default: addon schema, public. When the host declared a
// PrimarySchema and at least one of the addon's model tables exists there, that
// schema goes first and the addon schema stays right after it, so tables that
// only exist in the addon schema still resolve.
func migrationSearchPath(tx *gorm.DB, addonSchema string, opts ApplyOptions) ([]string, error) {
	path := []string{addonSchema, "public"}
	primary := strings.TrimSpace(opts.PrimarySchema)
	if primary == "" || primary == addonSchema || len(opts.ModelTables) == 0 {
		return path, nil
	}
	var found bool
	if err := tx.Raw(
		`SELECT EXISTS (SELECT 1 FROM pg_catalog.pg_class c
		   JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
		  WHERE n.nspname = ? AND c.relname IN ? AND c.relkind IN ('r', 'p'))`,
		primary, opts.ModelTables,
	).Scan(&found).Error; err != nil {
		return nil, err
	}
	if !found {
		return path, nil
	}
	out := []string{primary, addonSchema}
	if primary != "public" {
		out = append(out, "public")
	}
	return out, nil
}

// execScript runs a whole migration file on tx. A migration is a script
// (several statements, DO $$ … $$ blocks, functions), so it must reach Postgres
// through the simple query protocol. tx.Exec is not enough: a host that opened
// GORM with PrepareStmt (ops does) wraps the connection in a statement cache
// that PREPAREs every query, and Postgres refuses a prepared statement with
// more than one command (SQLSTATE 42601 "cannot insert multiple commands into
// a prepared statement"). Unwrapping the cache and executing without arguments
// makes pgx use the simple protocol; splitting the file on ';' instead would
// break dollar-quoted bodies.
func execScript(tx *gorm.DB, script string) error {
	pool := tx.Statement.ConnPool
	switch p := pool.(type) {
	case *gorm.PreparedStmtTX:
		pool = p.Tx
	case *gorm.PreparedStmtDB:
		pool = p.ConnPool
	}
	_, err := pool.ExecContext(tx.Statement.Context, script)
	return err
}

// quoteIdents renders schema names as a comma-separated list of quoted
// Postgres identifiers.
func quoteIdents(names []string) string {
	q := make([]string, len(names))
	for i, n := range names {
		q[i] = `"` + strings.ReplaceAll(n, `"`, `""`) + `"`
	}
	return strings.Join(q, ", ")
}

func recordMigration(db *gorm.DB, addonKey, version, checksum string) error {
	err := db.Table("public.metacore_addon_migrations").
		Create(&Migration{AddonKey: addonKey, Version: version, Checksum: checksum}).Error
	if err != nil && isBenignDDLConflict(err) {
		return nil
	}
	return err
}

// inPlaceEditMarker is the convention addons use when a migration that was
// already published had to be edited anyway ("EDITED IN PLACE (exception to
// the immutable-migrations rule ...)"). Only files carrying it may drift from
// their recorded checksum; every other mutated migration is still refused.
var inPlaceEditMarker = regexp.MustCompile(`(?im)^\s*--.*\bEDITED IN PLACE\b`)

func declaresInPlaceEdit(sql string) bool { return inPlaceEditMarker.MatchString(sql) }

func isNotFound(err error) bool {
	return errors.Is(err, gorm.ErrRecordNotFound)
}

// isBenignDDLConflict reports Postgres "already exists" errors during addon
// migrations. Local dev often retries a partially-applied install (hub
// reinstall, air restart) leaving tables/indexes behind while the migration
// ledger row is missing — treating these as applied keeps onboarding unblocked.
func isBenignDDLConflict(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "already exists") ||
		strings.Contains(msg, "duplicate key") ||
		strings.Contains(msg, "sqlstate 42p07") ||
		strings.Contains(msg, "sqlstate 42710") ||
		errors.Is(err, gorm.ErrDuplicatedKey)
}
