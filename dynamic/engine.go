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
		line, err := columnDDL(c)
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
	stmts = append(stmts, indexStatements(schema, def, needsOrgColumn)...)
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
