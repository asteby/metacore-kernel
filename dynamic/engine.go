package dynamic

import (
	"fmt"
	"reflect"
	"strings"

	"github.com/asteby/metacore-kernel/manifest"
	"github.com/google/uuid"
)

// SchemaEngine is the stable, public surface hosts (ops, appliance, tests) use
// to project a manifest.ModelDefinition into a Go reflect.Type and into SQL DDL.
// It is a thin facade: every method delegates to the existing kernel internals
// (BuildStructType, columnDDL, pgColumnType, indexStatements, rlsStatements,
// ValidateColumnType) so there is exactly ONE implementation of the projection.
//
// The zero value is ready to use; NewSchemaEngine is provided for readability.
type SchemaEngine struct{}

// NewSchemaEngine returns a ready-to-use engine. The engine is stateless.
func NewSchemaEngine() SchemaEngine { return SchemaEngine{} }

// ToReflectType assembles the runtime GORM-compatible struct type for a model.
// It is a pass-through to BuildStructType — the single source of truth for the
// struct projection — exposed under the stable facade name hosts consume.
func (SchemaEngine) ToReflectType(def manifest.ModelDefinition) (reflect.Type, error) {
	return BuildStructType(def)
}

// ToReflectTypeWithOptions is ToReflectType with opt-in StructOptions (real GORM
// soft-delete and/or a created_by_id column). With the zero-value opts it is
// identical to ToReflectType. See StructOptions and OpsCompatStructOptions.
func (SchemaEngine) ToReflectTypeWithOptions(def manifest.ModelDefinition, opts StructOptions) (reflect.Type, error) {
	return BuildStructTypeWithOptions(def, opts)
}

// ValidateType reports whether a manifest column type is one the engine can
// materialize. It wraps ValidateColumnType (the single allowlist) so hosts and
// the marketplace gate validate against the same set.
func (SchemaEngine) ValidateType(t string) error {
	return ValidateColumnType(t)
}

// DDLOptions controls how ToDDL projects a model into SQL statements. The ZERO
// value reproduces the kernel's historical behavior EXACTLY: tables land in the
// addon_<key> schema (derived from AddonKey/OrgID/Isolation), timestamps are
// timestamptz, the organization_id column and RLS follow the shared-isolation
// rules, and no created_by_id column is emitted.
//
// Setting the single-schema fields (see SingleSchemaDDLOptions) flips the engine
// into the "public/no-RLS/created_by_id" shape the ops runtime emits today, so
// the kernel can become the one source of truth for that host without a
// migration. Nothing here changes the default install path — it is opt-in.
type DDLOptions struct {
	// Schema overrides the target schema. Empty → derive addon_<key> via
	// SchemaName(AddonKey, OrgID, Isolation) (the historical behavior).
	Schema string

	// AddonKey / OrgID / Isolation are used only to derive the schema name and
	// the shared-isolation RLS decision when Schema is empty.
	AddonKey  string
	OrgID     uuid.UUID
	Isolation Isolation

	// DisableRLS omits the ENABLE ROW LEVEL SECURITY + policy statements even in
	// shared isolation.
	DisableRLS bool

	// IncludeCreatedBy adds a nullable created_by_id uuid column (+ index),
	// matching the framework-managed creator-tracking column ops emits.
	IncludeCreatedBy bool

	// TimestampWithoutZone emits created_at/updated_at/deleted_at as TIMESTAMP
	// (no time zone) instead of timestamptz, matching the ops emitter.
	TimestampWithoutZone bool

	// AlwaysOrgColumn forces an unconditional organization_id uuid NOT NULL
	// column regardless of def.OrgScoped / isolation — matching ops, which scopes
	// every dynamic table by organization_id through the host.
	AlwaysOrgColumn bool

	// AlwaysSoftDelete forces the deleted_at column (+ index) regardless of
	// def.SoftDelete — matching ops, whose runtime struct always carries
	// soft-delete.
	AlwaysSoftDelete bool

	// --- ops-compat DDL divergences (dual-run ops#847) ---
	// The five fields below make ToDDL reproduce, byte-for-byte after
	// normalization, quirks the ops CreateDynamicTable emitter already baked
	// into live prod tables. Each is independently opt-in; the zero value keeps
	// the kernel's historical DDL unchanged.

	// FloatAsDoublePrecision emits float/double/real columns as
	// `double precision` instead of the kernel's default `numeric(18,4)`.
	// decimal/numeric are unaffected. Matches ops.
	FloatAsDoublePrecision bool

	// ImplicitBoolDefaultFalse emits `DEFAULT false` for a bool/boolean column
	// that declares no default, matching ops' implicit boolean default.
	ImplicitBoolDefaultFalse bool

	// ImplicitJsonbDefaultObject emits `DEFAULT '{}'` for a jsonb/json column
	// that declares no default, matching ops' implicit jsonb default.
	ImplicitJsonbDefaultObject bool

	// QuoteBarewordStringDefaults quotes a non-empty string default on a
	// string/text column that manifest.DefaultLiteral would reject for lacking
	// quotes (e.g. default:"draft" → DEFAULT 'draft'). Single quotes in the
	// value are escaped. This is applied only in the DDL layer under this
	// option; DefaultLiteral's global behavior is unchanged.
	QuoteBarewordStringDefaults bool

	// LegacyUniqueIndexName names unique indexes idx_<table>_<col> (matching
	// ops) instead of the kernel's uidx_<table>_<col>.
	LegacyUniqueIndexName bool
}

// SingleSchemaDDLOptions returns options that make ToDDL emit the same table
// shape the ops runtime materializes today: all tables in a single flat schema
// (typically "public"), no RLS, unconditional organization_id NOT NULL,
// created_at/updated_at/deleted_at as plain TIMESTAMP, plus deleted_at and
// created_by_id present regardless of the manifest. This is the opt-in mode that
// lets a host delegate DDL to the kernel without changing its isolation model.
func SingleSchemaDDLOptions(schema string) DDLOptions {
	return DDLOptions{
		Schema:               schema,
		DisableRLS:           true,
		IncludeCreatedBy:     true,
		TimestampWithoutZone: true,
		AlwaysOrgColumn:      true,
		AlwaysSoftDelete:     true,
		// ops-compat DDL divergences (dual-run ops#847). This preset is THE
		// single ops-compatibility profile, so every divergence is enabled here.
		FloatAsDoublePrecision:      true,
		ImplicitBoolDefaultFalse:    true,
		ImplicitJsonbDefaultObject:  true,
		QuoteBarewordStringDefaults: true,
		LegacyUniqueIndexName:       true,
	}
}

// ToDDL projects a model into the ordered list of SQL statements that
// materialize it: CREATE TABLE, then CREATE INDEX, then (unless disabled) the
// RLS policy. It returns the statements as text instead of executing them, so a
// host can inspect, diff (dual-run), or run them itself.
//
// With the zero-value DDLOptions the output mirrors CreateTable's default path
// byte-for-byte in shape (addon schema, timestamptz, shared-mode org column +
// RLS). With SingleSchemaDDLOptions it mirrors the ops emitter.
func (SchemaEngine) ToDDL(def manifest.ModelDefinition, opts DDLOptions) ([]string, error) {
	return ToDDL(def, opts)
}

// AddColumnDDL generates the `ALTER TABLE <schema>.<table> ADD COLUMN IF NOT
// EXISTS <col> <type ...>` statement for ONE column, reusing the SAME type/DDL
// logic as ToDDL (opts.columnDDL / opts.pgColumnType) so an out-of-band schema
// sync and the CREATE TABLE path never diverge. It lets a host (ops) delegate
// its SyncDynamicTableSchema ADD COLUMN to the kernel instead of maintaining its
// own column-body SQL.
//
// With the zero-value DDLOptions the column body matches the kernel's default
// (numeric(18,4) for float, no implicit defaults); with SingleSchemaDDLOptions
// it applies the ops-compat type divergences (e.g. double precision, implicit
// bool/jsonb defaults). Generated columns are emitted as STORED generated
// columns, which Postgres computes for every existing row on ADD COLUMN.
func (SchemaEngine) AddColumnDDL(schema, table string, col manifest.ColumnDef, opts DDLOptions) (string, error) {
	return opts.addColumnDDL(schema, table, col)
}

// AddColumnsDDL generates one ADD COLUMN statement per column, in order.
func (SchemaEngine) AddColumnsDDL(schema, table string, cols []manifest.ColumnDef, opts DDLOptions) ([]string, error) {
	stmts := make([]string, 0, len(cols))
	for _, c := range cols {
		stmt, err := opts.addColumnDDL(schema, table, c)
		if err != nil {
			return nil, err
		}
		stmts = append(stmts, stmt)
	}
	return stmts, nil
}

// addColumnDDL builds an `ALTER TABLE … ADD COLUMN IF NOT EXISTS <body>`
// statement whose column body is exactly the CREATE TABLE fragment opts.columnDDL
// produces (same type resolution, NOT NULL, DEFAULT and STORED-generated
// handling), so both paths stay in lockstep.
func (opts DDLOptions) addColumnDDL(schema, table string, c manifest.ColumnDef) (string, error) {
	body, err := opts.columnDDL(c)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf(`ALTER TABLE %q.%q ADD COLUMN IF NOT EXISTS %s`, schema, table, body), nil
}

// columnDDL builds the per-column CREATE TABLE fragment applying the opts'
// ops-compat divergences. With the zero-value options it is identical to the
// package-level columnDDL (the executing path), so default mode is unchanged.
func (opts DDLOptions) columnDDL(c manifest.ColumnDef) (string, error) {
	pgType, err := opts.pgColumnType(c)
	if err != nil {
		return "", err
	}
	if c.Generated != "" {
		// Generated columns carry neither NOT NULL nor DEFAULT — delegate to the
		// shared helper (its type override does not matter for these).
		return columnDDL(c)
	}
	line := fmt.Sprintf(`%q %s`, c.Name, pgType)
	if c.Required {
		line += " NOT NULL"
	}
	if def := opts.defaultClause(c); def != "" {
		line += " DEFAULT " + def
	}
	return line, nil
}

// pgColumnType resolves the SQL type for a column, applying the
// FloatAsDoublePrecision override before falling back to the shared resolver.
func (opts DDLOptions) pgColumnType(c manifest.ColumnDef) (string, error) {
	if opts.FloatAsDoublePrecision {
		switch strings.ToLower(c.Type) {
		case "float", "double", "real":
			return "double precision", nil
		}
	}
	return pgColumnType(c)
}

// defaultClause returns the DEFAULT literal (without the "DEFAULT " keyword) for
// a column, or "" for none. It preserves DefaultLiteral's decisions and only
// layers the opts' ops-compat implicit/quoted defaults on top.
func (opts DDLOptions) defaultClause(c manifest.ColumnDef) string {
	lit, ok := manifest.DefaultLiteral(c.Default)
	if ok && lit != "" {
		return lit
	}
	if !ok {
		// DefaultLiteral rejected the value. The only compat recovery is a
		// bareword string default on a string/text column.
		if opts.QuoteBarewordStringDefaults {
			if s, isStr := c.Default.(string); isStr && s != "" {
				switch strings.ToLower(c.Type) {
				case "string", "text":
					return "'" + strings.ReplaceAll(s, "'", "''") + "'"
				}
			}
		}
		return ""
	}
	// ok && lit == "" → no declared default; apply implicit type defaults.
	switch strings.ToLower(c.Type) {
	case "bool", "boolean":
		if opts.ImplicitBoolDefaultFalse {
			return "false"
		}
	case "jsonb", "json":
		if opts.ImplicitJsonbDefaultObject {
			return "'{}'"
		}
	}
	return ""
}

// ToDDL is the package-level entry point behind SchemaEngine.ToDDL. See that
// method for the contract.
func ToDDL(def manifest.ModelDefinition, opts DDLOptions) ([]string, error) {
	schema := opts.Schema
	if schema == "" {
		schema = SchemaName(opts.AddonKey, opts.OrgID, opts.Isolation)
	}

	tsType := "timestamptz"
	if opts.TimestampWithoutZone {
		tsType = "timestamp"
	}

	needsOrgColumn := opts.AlwaysOrgColumn || def.OrgScoped || opts.Isolation == IsolationShared
	softDelete := opts.AlwaysSoftDelete || def.SoftDelete

	cols := []string{`"id" uuid PRIMARY KEY DEFAULT gen_random_uuid()`}
	if needsOrgColumn {
		cols = append(cols, `"organization_id" uuid NOT NULL`)
	}
	for _, c := range def.Columns {
		line, err := opts.columnDDL(c)
		if err != nil {
			return nil, err
		}
		cols = append(cols, line)
	}
	cols = append(cols,
		fmt.Sprintf(`"created_at" %s NOT NULL DEFAULT NOW()`, tsType),
		fmt.Sprintf(`"updated_at" %s NOT NULL DEFAULT NOW()`, tsType),
	)
	if softDelete {
		cols = append(cols, fmt.Sprintf(`"deleted_at" %s`, tsType))
	}
	if opts.IncludeCreatedBy {
		cols = append(cols, `"created_by_id" uuid`)
	}

	stmts := []string{
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %q.%q (%s)`,
			schema, def.TableName, strings.Join(cols, ", ")),
	}
	uniquePrefix := "uidx_"
	if opts.LegacyUniqueIndexName {
		uniquePrefix = "idx_"
	}
	stmts = append(stmts, indexStatementsWithPrefix(schema, def, needsOrgColumn, uniquePrefix)...)
	if softDelete {
		stmts = append(stmts, fmt.Sprintf(`CREATE INDEX IF NOT EXISTS %q ON %q.%q ("deleted_at")`,
			"idx_"+def.TableName+"_deleted", schema, def.TableName))
	}
	if opts.IncludeCreatedBy {
		stmts = append(stmts, fmt.Sprintf(`CREATE INDEX IF NOT EXISTS %q ON %q.%q ("created_by_id")`,
			"idx_"+def.TableName+"_created_by", schema, def.TableName))
	}
	if opts.Isolation == IsolationShared && needsOrgColumn && !opts.DisableRLS {
		stmts = append(stmts, rlsStatements(schema, def.TableName)...)
	}
	return stmts, nil
}
