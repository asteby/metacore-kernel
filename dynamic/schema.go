package dynamic

import (
	"database/sql"
	"fmt"
	"strings"

	"github.com/asteby/metacore-kernel/manifest"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// EnsureSchema creates the addon's Postgres schema if it doesn't exist.
// For shared isolation the orgID is ignored and the schema is global
// (addon_<key>). For schema-per-tenant it creates addon_<key>_<orgshort>.
// Called once per install before any CREATE TABLE.
func EnsureSchema(db *gorm.DB, addonKey string, orgID uuid.UUID, iso Isolation) error {
	schema := SchemaName(addonKey, orgID, iso)
	// Schema names are validated by manifest.Validate — safe to interpolate.
	return db.Exec(fmt.Sprintf(`CREATE SCHEMA IF NOT EXISTS %q`, schema)).Error
}

// CreateTable emits CREATE TABLE IF NOT EXISTS for a ModelDefinition.
// Idempotent. In shared mode it also enables row-level security so any SQL
// executed later under `SET app.current_org` is scoped automatically.
func CreateTable(db *gorm.DB, addonKey string, orgID uuid.UUID, iso Isolation, def manifest.ModelDefinition) error {
	schema := SchemaName(addonKey, orgID, iso)
	cols := []string{`"id" uuid PRIMARY KEY DEFAULT gen_random_uuid()`}
	// In shared mode org scoping is required for RLS. In per-tenant mode the
	// schema itself is the boundary so the column is only added if the addon
	// asks for it (rare — usually redundant once isolated).
	needsOrgColumn := def.OrgScoped || iso == IsolationShared
	if needsOrgColumn {
		cols = append(cols, `"organization_id" uuid NOT NULL`)
	}
	for _, c := range def.Columns {
		pgType, err := pgColumnType(c)
		if err != nil {
			return err
		}
		line := fmt.Sprintf(`%q %s`, c.Name, pgType)
		if c.Required {
			line += " NOT NULL"
		}
		if lit, ok := manifest.DefaultLiteral(c.Default); ok && lit != "" {
			line += " DEFAULT " + lit
		}
		cols = append(cols, line)
	}
	cols = append(cols,
		`"created_at" timestamptz NOT NULL DEFAULT NOW()`,
		`"updated_at" timestamptz NOT NULL DEFAULT NOW()`,
	)
	if def.SoftDelete {
		cols = append(cols, `"deleted_at" timestamptz`)
	}
	stmt := fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %q.%q (%s)`,
		schema, def.TableName, strings.Join(cols, ", "))
	if err := db.Exec(stmt).Error; err != nil {
		return fmt.Errorf("create table %s.%s: %w", schema, def.TableName, err)
	}
	if err := createIndexes(db, schema, def, needsOrgColumn); err != nil {
		return err
	}
	if iso == IsolationShared && needsOrgColumn {
		if err := enableRLS(db, schema, def.TableName); err != nil {
			return fmt.Errorf("enable RLS %s.%s: %w", schema, def.TableName, err)
		}
	}
	return nil
}

func createIndexes(db *gorm.DB, schema string, def manifest.ModelDefinition, hasOrg bool) error {
	if hasOrg {
		idx := fmt.Sprintf(`CREATE INDEX IF NOT EXISTS %q ON %q.%q ("organization_id")`,
			"idx_"+def.TableName+"_org", schema, def.TableName)
		if err := db.Exec(idx).Error; err != nil {
			return err
		}
	}
	for _, c := range def.Columns {
		if c.Index && !c.Unique {
			idx := fmt.Sprintf(`CREATE INDEX IF NOT EXISTS %q ON %q.%q (%q)`,
				"idx_"+def.TableName+"_"+c.Name, schema, def.TableName, c.Name)
			if err := db.Exec(idx).Error; err != nil {
				return err
			}
		}
		if c.Unique {
			idx := fmt.Sprintf(`CREATE UNIQUE INDEX IF NOT EXISTS %q ON %q.%q (%q)`,
				"uidx_"+def.TableName+"_"+c.Name, schema, def.TableName, c.Name)
			if err := db.Exec(idx).Error; err != nil {
				return err
			}
		}
	}
	return nil
}

// enableRLS turns on row-level security and installs a policy that scopes
// every SELECT / UPDATE / DELETE to `current_setting('app.current_org')`.
// Hosts must run `SET LOCAL app.current_org = '<uuid>'` per request — see
// docs/migration-from-ops.md "RLS setup".
func enableRLS(db *gorm.DB, schema, table string) error {
	policy := "rls_org_isolation"
	stmts := []string{
		fmt.Sprintf(`ALTER TABLE %q.%q ENABLE ROW LEVEL SECURITY`, schema, table),
		// DROP POLICY IF EXISTS is not transactional-safe in all Postgres
		// versions, so we attempt CREATE and tolerate "already exists".
		fmt.Sprintf(`DROP POLICY IF EXISTS %q ON %q.%q`, policy, schema, table),
		fmt.Sprintf(
			`CREATE POLICY %q ON %q.%q
             USING ("organization_id" = current_setting('app.current_org', true)::uuid)
             WITH CHECK ("organization_id" = current_setting('app.current_org', true)::uuid)`,
			policy, schema, table),
	}
	for _, s := range stmts {
		if err := db.Exec(s).Error; err != nil {
			return err
		}
	}
	return nil
}

// SyncSchema adds columns the manifest declares but the table is missing.
// DROP and RENAME are not performed here — those require an explicit migration.
func SyncSchema(db *gorm.DB, addonKey string, orgID uuid.UUID, iso Isolation, def manifest.ModelDefinition) error {
	schema := SchemaName(addonKey, orgID, iso)
	existing, err := columnsOf(db, schema, def.TableName)
	if err != nil {
		return err
	}
	for _, c := range def.Columns {
		if _, ok := existing[c.Name]; ok {
			continue
		}
		pgType, err := pgColumnType(c)
		if err != nil {
			return err
		}
		stmt := fmt.Sprintf(`ALTER TABLE %q.%q ADD COLUMN IF NOT EXISTS %q %s`,
			schema, def.TableName, c.Name, pgType)
		if err := db.Exec(stmt).Error; err != nil {
			return fmt.Errorf("add column %s.%s.%s: %w", schema, def.TableName, c.Name, err)
		}
	}
	return nil
}

// DropSchema removes the addon's schema and everything in it.
// Called only on full uninstall after the caller confirms destructive intent.
// For per-tenant addons the caller passes the specific orgID; for shared
// addons this is a global destructive op — the installer gates it on
// "no remaining installations".
func DropSchema(db *gorm.DB, addonKey string, orgID uuid.UUID, iso Isolation) error {
	schema := SchemaName(addonKey, orgID, iso)
	return db.Exec(fmt.Sprintf(`DROP SCHEMA IF EXISTS %q CASCADE`, schema)).Error
}

func columnsOf(db *gorm.DB, schema, table string) (map[string]struct{}, error) {
	rows, err := db.Raw(
		`SELECT column_name FROM information_schema.columns
		 WHERE table_schema = ? AND table_name = ?`, schema, table).Rows()
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]struct{})
	var name string
	for rows.Next() {
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out[name] = struct{}{}
	}
	return out, nil
}

func pgColumnType(c manifest.ColumnDef) (string, error) {
	switch strings.ToLower(c.Type) {
	case "string":
		size := c.Size
		if size == 0 {
			size = 255
		}
		return fmt.Sprintf("varchar(%d)", size), nil
	case "text":
		return "text", nil
	case "uuid":
		return "uuid", nil
	case "int", "integer":
		return "integer", nil
	case "bigint":
		return "bigint", nil
	case "decimal", "numeric", "float", "double":
		return "numeric(18,4)", nil
	case "bool", "boolean":
		return "boolean", nil
	case "timestamp", "datetime":
		return "timestamptz", nil
	case "date":
		return "date", nil
	case "jsonb", "json":
		return "jsonb", nil
	default:
		return "", fmt.Errorf("unknown column type %q", c.Type)
	}
}

// SetRequestOrg binds the per-request org UUID on the current session so RLS
// policies filter correctly. Hosts call this on every DB transaction that
// touches shared-isolation addon tables.
func SetRequestOrg(db *gorm.DB, orgID uuid.UUID) error {
	return db.Exec(`SELECT set_config('app.current_org', ?, true)`, orgID.String()).Error
}

// Ensure we keep database/sql imported for future raw Rows usage.
var _ = sql.ErrNoRows
