package dynamic

import (
	"context"
	"fmt"
	"strings"

	"github.com/asteby/metacore-kernel/manifest"
	"github.com/asteby/metacore-kernel/modelbase"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// Audit-column standard. Every model answers "when and who" for each change,
// whether or not its manifest declares the columns:
//
//	created_at, updated_at, deleted_at, created_by_id, updated_by_id, deleted_by_id
//
// See docs/audit-columns.md. This file is the single source of truth for which
// columns a model gets (AuditColumns) and for the DDL that adds them to a table
// that already exists (EnsureAuditColumns).

// Names of the standard audit columns.
const (
	ColCreatedAt   = "created_at"
	ColUpdatedAt   = "updated_at"
	ColDeletedAt   = "deleted_at"
	ColCreatedByID = "created_by_id"
	ColUpdatedByID = "updated_by_id"
	ColDeletedByID = "deleted_by_id"
)

// auditInputKeys are the keys the runtime owns: Service.Create/Update drop them
// from a caller payload so a client cannot forge who/when.
var auditInputKeys = []string{ColCreatedAt, ColUpdatedAt, ColDeletedAt, ColCreatedByID, ColUpdatedByID, ColDeletedByID}

// stripAuditInput removes the audit keys from a client payload (in place).
func stripAuditInput(input map[string]any) {
	for _, k := range auditInputKeys {
		delete(input, k)
	}
}

// AuditPlan says which standard audit columns a model carries.
type AuditPlan struct {
	CreatedAt, UpdatedAt, DeletedAt bool
	CreatedBy, UpdatedBy, DeletedBy bool
}

// AuditPlanFor resolves the audit plan of def.
//
//   - default: all six columns;
//   - def.AppendOnly: created_at + created_by_id only (a ledger row is never
//     updated or deleted); updated_at is dropped too;
//   - def.NoAudit (v3 `audit: false`): the historical created_at/updated_at pair
//     only, plus deleted_at when def.SoftDelete.
//
// alwaysSoftDelete / includeCreatedBy are the legacy host switches
// (DDLOptions.AlwaysSoftDelete / IncludeCreatedBy, StructOptions.*): they force
// the corresponding column on regardless of the mode and are now redundant with
// the default, but stay honoured.
func AuditPlanFor(def manifest.ModelDefinition, alwaysSoftDelete, includeCreatedBy bool) AuditPlan {
	p := AuditPlan{CreatedAt: true, UpdatedAt: true}
	switch {
	case def.AppendOnly:
		p.UpdatedAt = false
		p.CreatedBy = true
	case def.NoAudit:
		// historical shape
	default:
		p.DeletedAt, p.CreatedBy, p.UpdatedBy, p.DeletedBy = true, true, true, true
	}
	if def.SoftDelete || alwaysSoftDelete {
		p.DeletedAt = true
	}
	if includeCreatedBy {
		p.CreatedBy = true
	}
	return p
}

// declared reports which standard names the manifest itself declares as
// ordinary columns (the manifest declaration wins; the kernel only adds what is
// missing).
func declaredColumns(def manifest.ModelDefinition) map[string]struct{} {
	out := make(map[string]struct{}, len(def.Columns))
	for _, c := range def.Columns {
		out[c.Name] = struct{}{}
	}
	return out
}

// auditColumn is one standard column to materialize.
type auditColumn struct {
	Name string
	// Timestamp reports a timestamp column; otherwise it is a uuid.
	Timestamp bool
	// NotNullNow marks created_at/updated_at (NOT NULL DEFAULT NOW()).
	NotNullNow bool
}

// AuditColumns returns the standard columns def must have that its manifest
// does not already declare, in a stable order. created_at/updated_at are always
// kernel-owned (the manifest cannot redeclare them: FromV3 strips them), the
// deleted_at / *_by_id declared in the manifest are skipped.
func AuditColumns(def manifest.ModelDefinition, alwaysSoftDelete, includeCreatedBy bool) []auditColumn {
	p := AuditPlanFor(def, alwaysSoftDelete, includeCreatedBy)
	decl := declaredColumns(def)
	var out []auditColumn
	add := func(on bool, c auditColumn) {
		if !on {
			return
		}
		if _, ok := decl[c.Name]; ok {
			return
		}
		out = append(out, c)
	}
	add(p.CreatedAt, auditColumn{Name: ColCreatedAt, Timestamp: true, NotNullNow: true})
	add(p.UpdatedAt, auditColumn{Name: ColUpdatedAt, Timestamp: true, NotNullNow: true})
	add(p.DeletedAt, auditColumn{Name: ColDeletedAt, Timestamp: true})
	add(p.CreatedBy, auditColumn{Name: ColCreatedByID})
	add(p.UpdatedBy, auditColumn{Name: ColUpdatedByID})
	add(p.DeletedBy, auditColumn{Name: ColDeletedByID})
	return out
}

// ddl renders the column body for CREATE TABLE / ADD COLUMN.
func (c auditColumn) ddl(tsType string) string {
	switch {
	case c.NotNullNow:
		return fmt.Sprintf(`%q %s NOT NULL DEFAULT NOW()`, c.Name, tsType)
	case c.Timestamp:
		return fmt.Sprintf(`%q %s`, c.Name, tsType)
	default:
		return fmt.Sprintf(`%q uuid`, c.Name)
	}
}

// auditIndexStatements returns the indexes that accompany the audit columns the
// model carries (declared or kernel-added): deleted_at (+ org/deleted_at when
// the table is org-scoped) and created_by_id. updated_by_id / deleted_by_id are
// deliberately unindexed (write-amplification for a rarely-filtered column).
func auditIndexStatements(schema string, def manifest.ModelDefinition, plan AuditPlan, hasOrg bool) []string {
	var stmts []string
	if plan.DeletedAt {
		stmts = append(stmts, fmt.Sprintf(`CREATE INDEX IF NOT EXISTS %q ON %q.%q ("deleted_at")`,
			"idx_"+def.TableName+"_deleted", schema, def.TableName))
		if hasOrg {
			stmts = append(stmts, fmt.Sprintf(`CREATE INDEX IF NOT EXISTS %q ON %q.%q ("organization_id", "deleted_at")`,
				"idx_"+def.TableName+"_org_deleted", schema, def.TableName))
		}
	}
	if plan.CreatedBy {
		stmts = append(stmts, fmt.Sprintf(`CREATE INDEX IF NOT EXISTS %q ON %q.%q ("created_by_id")`,
			"idx_"+def.TableName+"_created_by", schema, def.TableName))
	}
	return stmts
}

// AuditColumnsDDL returns the idempotent statements that bring an existing
// table up to the audit standard: one `ALTER TABLE … ADD COLUMN IF NOT EXISTS`
// per missing standard column (existing = the table's current column names; nil
// means "assume none exist" and emits all of them), followed by the audit
// indexes. Adding a nullable column without a default — and created_at /
// updated_at with the STABLE default NOW() — is a catalog-only change on
// PostgreSQL 11+ (no table rewrite), so it is cheap on large tables; the index
// builds are the only cost proportional to table size. Hosts that own their DDL
// (ops' SyncDynamicTableSchema) call this to delegate the audit step.
func (SchemaEngine) AuditColumnsDDL(schema string, def manifest.ModelDefinition, existing map[string]struct{}, opts DDLOptions) []string {
	tsType := "timestamptz"
	if opts.TimestampWithoutZone {
		tsType = "timestamp"
	}
	return auditColumnsDDL(schema, def, existing, tsType, opts.AlwaysSoftDelete, opts.IncludeCreatedBy, opts.AlwaysOrgColumn || def.OrgScoped || opts.Isolation == IsolationShared)
}

func auditColumnsDDL(schema string, def manifest.ModelDefinition, existing map[string]struct{}, tsType string, alwaysSoft, includeCreatedBy, hasOrg bool) []string {
	var stmts []string
	for _, c := range AuditColumns(def, alwaysSoft, includeCreatedBy) {
		if _, ok := existing[c.Name]; ok {
			continue
		}
		stmts = append(stmts, fmt.Sprintf(`ALTER TABLE %q.%q ADD COLUMN IF NOT EXISTS %s`, schema, def.TableName, c.ddl(tsType)))
	}
	plan := AuditPlanFor(def, alwaysSoft, includeCreatedBy)
	// Index only the columns that exist after the ALTERs: a declared-by-manifest
	// deleted_at / created_by_id is indexed too (as ToDDL always did), but a
	// declared column of a foreign type is left alone.
	stmts = append(stmts, auditIndexStatements(schema, def, plan, hasOrg && hasColumn(existing, "organization_id", true))...)
	return stmts
}

func hasColumn(existing map[string]struct{}, name string, whenNil bool) bool {
	if existing == nil {
		return whenNil
	}
	_, ok := existing[name]
	return ok
}

// EnsureAuditColumns brings an ALREADY-MATERIALIZED table in `schema` up to the
// audit standard: adds the missing audit columns and their indexes. It is a
// no-op when the table does not exist in that schema (a host serving the models
// from another schema calls it once per schema). Idempotent. The timestamp type
// of kernel-added columns follows the table's own created_at (timestamp vs
// timestamptz) so a host-managed `public` table stays homogeneous.
func EnsureAuditColumns(db *gorm.DB, schema string, def manifest.ModelDefinition) error {
	// The columns the runtime stamps are about to change: drop cached sets.
	defer InvalidateTableColumns()
	types, err := columnTypesOf(db, schema, def.TableName)
	if err != nil {
		return err
	}
	if len(types) == 0 {
		return nil // table not materialized in this schema
	}
	existing := make(map[string]struct{}, len(types))
	for n := range types {
		existing[n] = struct{}{}
	}
	tsType := "timestamptz"
	if types[ColCreatedAt] == "timestamp without time zone" {
		tsType = "timestamp"
	}
	hasOrg := hasColumn(existing, "organization_id", false)
	for _, stmt := range auditColumnsDDL(schema, def, existing, tsType, false, false, hasOrg) {
		if err := db.Exec(stmt).Error; err != nil {
			return fmt.Errorf("audit columns %s.%s: %w", schema, def.TableName, err)
		}
	}
	return nil
}

// columnTypesOf maps a table's column names to their information_schema
// data_type. Empty when the table does not exist.
func columnTypesOf(db *gorm.DB, schema, table string) (map[string]string, error) {
	rows, err := db.Raw(
		`SELECT column_name, data_type FROM information_schema.columns
		 WHERE table_schema = ? AND table_name = ?`, schema, table).Rows()
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]string)
	for rows.Next() {
		var name, typ string
		if err := rows.Scan(&name, &typ); err != nil {
			return nil, err
		}
		out[name] = strings.ToLower(typ)
	}
	return out, rows.Err()
}

// ActorOrSystem returns actorID when it is a valid uuid, else the reserved
// SystemActorID (rendered). It is the single rule every write path uses to fill
// created_by_id / updated_by_id / deleted_by_id: a request with a user stamps
// the user; a schedule, webhook, connector or any invocation with no actor
// stamps SystemActorID, so the column answers "who" even for unattended work.
func ActorOrSystem(actorID string) string {
	if id, err := uuid.Parse(actorID); err == nil && id != uuid.Nil {
		return id.String()
	}
	return SystemActorID.String()
}

// ActorFromContext is ActorOrSystem over the ctx actor (WithActorID).
func ActorFromContext(ctx context.Context) string {
	return ActorOrSystem(ActorIDFromContext(ctx))
}

// AuditMetaFor projects the audit columns a model carries onto the served
// metadata (modelbase.AuditMeta): each field names the row key a UI reads.
func AuditMetaFor(def manifest.ModelDefinition, alwaysSoftDelete, includeCreatedBy bool) *modelbase.AuditMeta {
	p := AuditPlanFor(def, alwaysSoftDelete, includeCreatedBy)
	m := &modelbase.AuditMeta{}
	if p.CreatedAt {
		m.CreatedAt = ColCreatedAt
	}
	if p.UpdatedAt {
		m.UpdatedAt = ColUpdatedAt
	}
	if p.DeletedAt {
		m.DeletedAt = ColDeletedAt
	}
	if p.CreatedBy {
		m.CreatedBy = ColCreatedByID
	}
	if p.UpdatedBy {
		m.UpdatedBy = ColUpdatedByID
	}
	if p.DeletedBy {
		m.DeletedBy = ColDeletedByID
	}
	return m
}
