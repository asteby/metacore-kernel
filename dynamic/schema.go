package dynamic

import (
	"database/sql"
	"fmt"
	"regexp"
	"strings"

	"github.com/asteby/metacore-kernel/manifest"
	"github.com/asteby/metacore-kernel/manifest/computeexpr"
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
		line, err := columnDDL(c)
		if err != nil {
			return err
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
	// Self-heal: a table created by an earlier code path — or before this model
	// became org-scoped — may pre-exist WITHOUT organization_id. CREATE TABLE IF
	// NOT EXISTS won't add the missing column, and createIndexes / enableRLS
	// below would then fail with "column organization_id does not exist"
	// (SQLSTATE 42703). Add it idempotently. Nullable here because NOT NULL
	// can't be applied to a populated table; freshly-created tables above
	// already declare it NOT NULL.
	if needsOrgColumn {
		alter := fmt.Sprintf(`ALTER TABLE %q.%q ADD COLUMN IF NOT EXISTS "organization_id" uuid`,
			schema, def.TableName)
		if err := db.Exec(alter).Error; err != nil {
			return fmt.Errorf("ensure organization_id on %s.%s: %w", schema, def.TableName, err)
		}
	}
	// Self-heal, generalized: on upgrade the table already exists and CREATE
	// TABLE IF NOT EXISTS is a no-op, so any column the NEW manifest adds is
	// missing — and createIndexes below would 42703 on it (seen with
	// pos_payment_methods.currency_code). SyncSchema adds the manifest-declared
	// columns the live table lacks; additive only, idempotent.
	if err := SyncSchema(db, addonKey, orgID, iso, def); err != nil {
		return err
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

// indexStatements builds the CREATE INDEX fragments for a table's org column
// and any indexed/unique manifest columns. It is the single source of truth for
// index DDL, shared by createIndexes (which executes them) and ToDDL (which
// returns them as text).
func indexStatements(schema string, def manifest.ModelDefinition, hasOrg bool) []string {
	var stmts []string
	if hasOrg {
		stmts = append(stmts, fmt.Sprintf(`CREATE INDEX IF NOT EXISTS %q ON %q.%q ("organization_id")`,
			"idx_"+def.TableName+"_org", schema, def.TableName))
	}
	for _, c := range def.Columns {
		if c.Index && !c.Unique {
			stmts = append(stmts, fmt.Sprintf(`CREATE INDEX IF NOT EXISTS %q ON %q.%q (%q)`,
				"idx_"+def.TableName+"_"+c.Name, schema, def.TableName, c.Name))
		}
		if c.Unique {
			stmts = append(stmts, fmt.Sprintf(`CREATE UNIQUE INDEX IF NOT EXISTS %q ON %q.%q (%q)`,
				"uidx_"+def.TableName+"_"+c.Name, schema, def.TableName, c.Name))
		}
	}
	return stmts
}

func createIndexes(db *gorm.DB, schema string, def manifest.ModelDefinition, hasOrg bool) error {
	for _, idx := range indexStatements(schema, def, hasOrg) {
		if err := db.Exec(idx).Error; err != nil {
			return err
		}
	}
	return nil
}

// columnDDL builds the per-column fragment for a CREATE TABLE. A column with a
// Generated expression is emitted as a Postgres STORED generated column
// (`GENERATED ALWAYS AS (<expr>) STORED`) and carries neither NOT NULL nor
// DEFAULT (Postgres rejects both on a generated column); every other column
// carries its optional NOT NULL / DEFAULT as before.
func columnDDL(c manifest.ColumnDef) (string, error) {
	pgType, err := pgColumnType(c)
	if err != nil {
		return "", err
	}
	if c.Generated != "" {
		sqlExpr, err := computeexpr.RenderSQL(c.Generated)
		if err != nil {
			return "", fmt.Errorf("generated column %q: %w", c.Name, err)
		}
		return fmt.Sprintf(`%q %s GENERATED ALWAYS AS (%s) STORED`, c.Name, pgType, sqlExpr), nil
	}
	line := fmt.Sprintf(`%q %s`, c.Name, pgType)
	if c.Required {
		line += " NOT NULL"
	}
	if lit, ok := manifest.DefaultLiteral(c.Default); ok && lit != "" {
		line += " DEFAULT " + lit
	}
	return line, nil
}

// addColumnDDL builds the ALTER TABLE … ADD COLUMN IF NOT EXISTS statement for a
// single column. A Generated column becomes a STORED generated column, which
// Postgres computes for every existing row on ADD COLUMN.
func addColumnDDL(schema, table string, c manifest.ColumnDef) (string, error) {
	pgType, err := pgColumnType(c)
	if err != nil {
		return "", err
	}
	if c.Generated != "" {
		sqlExpr, err := computeexpr.RenderSQL(c.Generated)
		if err != nil {
			return "", fmt.Errorf("generated column %q: %w", c.Name, err)
		}
		return fmt.Sprintf(`ALTER TABLE %q.%q ADD COLUMN IF NOT EXISTS %q %s GENERATED ALWAYS AS (%s) STORED`,
			schema, table, c.Name, pgType, sqlExpr), nil
	}
	return fmt.Sprintf(`ALTER TABLE %q.%q ADD COLUMN IF NOT EXISTS %q %s`,
		schema, table, c.Name, pgType), nil
}

// enableRLS turns on row-level security and installs a policy that scopes
// every SELECT / UPDATE / DELETE to `current_setting('app.current_org')`.
// Hosts must run `SET LOCAL app.current_org = '<uuid>'` per request.
// rlsStatements builds the ROW LEVEL SECURITY DDL for a shared-isolation table.
// Shared by enableRLS (executes) and ToDDL (returns as text) so the two paths
// never diverge.
func rlsStatements(schema, table string) []string {
	policy := "rls_org_isolation"
	return []string{
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
}

func enableRLS(db *gorm.DB, schema, table string) error {
	for _, s := range rlsStatements(schema, table) {
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
		stmt, err := addColumnDDL(schema, def.TableName, c)
		if err != nil {
			return err
		}
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
	case "timestamp", "timestamptz", "datetime", "timestamp with time zone":
		return "timestamptz", nil
	case "date":
		return "date", nil
	case "jsonb", "json":
		return "jsonb", nil
	case "vector":
		// pgvector embedding column. The dimension may be declared verbatim as
		// vector(768) (handled by parameterizedColumnType) or via ColumnDef.Size;
		// a bare "vector" leaves the dimension unconstrained. Requires the
		// pgvector extension (host.NewApp with EnableVectorStore, or an explicit
		// CREATE EXTENSION vector).
		if c.Size > 0 {
			return fmt.Sprintf("vector(%d)", c.Size), nil
		}
		return "vector", nil
	default:
		if sqlType, ok := parameterizedColumnType(c.Type); ok {
			return sqlType, nil
		}
		return "", fmt.Errorf("unknown column type %q", c.Type)
	}
}

// paramColumnTypeRe matches Postgres-native parameterized type forms that v3
// manifests may declare verbatim, e.g. "numeric(6,2)", "decimal(10,2)" or
// "varchar(120)". The inner parameters are constrained to digits and a single
// comma so the value is safe to splice into DDL.
var paramColumnTypeRe = regexp.MustCompile(`^([a-zA-Z][a-zA-Z ]*)\((\d{1,4}(,\d{1,4})?)\)$`)

// parameterizedColumnType normalizes a parameterized column type into the SQL
// type the kernel emits. It returns ok=false for anything it does not
// explicitly recognize, so the caller still rejects genuinely unknown types.
func parameterizedColumnType(raw string) (string, bool) {
	m := paramColumnTypeRe.FindStringSubmatch(strings.TrimSpace(raw))
	if m == nil {
		return "", false
	}
	base := strings.ToLower(strings.TrimSpace(m[1]))
	params := m[2]
	switch base {
	case "numeric", "decimal":
		return "numeric(" + params + ")", true
	case "varchar", "char", "character", "character varying":
		return "varchar(" + params + ")", true
	case "vector":
		// pgvector dimension declared verbatim, e.g. vector(768). The regex only
		// allows a single integer parameter for vectors (a comma form would be
		// rejected upstream as it is not a valid vector modifier).
		return "vector(" + params + ")", true
	default:
		return "", false
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
