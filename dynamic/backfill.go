package dynamic

// backfill.go implements a batched, org-scoped RECOMPUTE (backfill) of the
// declarative compute engine's Tier-1 rollups and Tier-2 formulas (see
// compute.go for the tier definitions and the incremental, per-write hooks
// this reuses). It exists for rows the incremental hooks never saw: data
// imported straight into the tables (e.g. the 7leguas→pitsline migration
// ledger, docs/ETL-BUNDLE.md in ops) bypasses the CRUD hooks entirely, so its
// rollup/formula columns are left at their default (typically 0) until
// something recomputes them from the data that is actually there.
//
// Backfill does NOT duplicate the incremental logic: rollup aggregate SQL
// comes from the same computeAggExpr/rollupBinding types compute.go builds,
// and Tier-2 formulas are evaluated with the same applyFormulas function the
// BeforeCreate/BeforeUpdate hooks call. It only adds the batch/keyset
// traversal and the "write only if changed" comparison around them.
//
// Ordering: Tier-2 formulas run before Tier-1 rollups, mirroring the
// incremental engine's ordering guarantee (RegisterComputeHooks doc comment)
// — a rollup whose `from`/`expr` reads a child column a formula computes
// then sees the corrected value.

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"reflect"
	"time"

	"github.com/asteby/metacore-kernel/manifest"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// DefaultBackfillBatchSize is used when BackfillOptions.BatchSize is <= 0.
const DefaultBackfillBatchSize = 500

// backfillEpsilon is the tolerance used to decide whether a recomputed value
// actually differs from the value already stored (floating point rollups and
// formulas should not cause a write on every backfill run).
const backfillEpsilon = 1e-9

// BackfillOptions configures one Backfill run. OrgID is required — Backfill
// never scans across organizations. ModelKey and Fields narrow the scope;
// left empty they mean "every model" / "every declared rollup and formula
// target".
type BackfillOptions struct {
	OrgID     uuid.UUID
	ModelKey  string
	Fields    []string
	BatchSize int
	DryRun    bool
}

// BackfillModelReport is the outcome of recomputing one (model, tier) unit —
// a model's Tier-2 formulas, or one parent model's Tier-1 rollups for one
// relation. Backfill returns one entry per unit so a caller can tell exactly
// which relation or formula set failed.
type BackfillModelReport struct {
	Model       string        `json:"model"`
	Tier        int           `json:"tier"`
	RowsScanned int           `json:"rows_scanned"`
	RowsUpdated int           `json:"rows_updated"`
	Duration    time.Duration `json:"duration"`
	Errors      []string      `json:"errors,omitempty"`
}

// Backfill recomputes Tier-2 formulas then Tier-1 rollups, batched with
// keyset pagination on `id`, for every model (or just opts.ModelKey) declared
// in m, scoped to opts.OrgID. It writes only rows whose recomputed value
// differs from what is stored (beyond backfillEpsilon) — a fully-consistent
// org backfills to zero writes. opts.DryRun runs the same comparison without
// writing, so RowsUpdated reports what WOULD change.
//
// reg supplies the Tier-3 (wasm) FormulaInvoker if one is configured
// (RegisterComputeHooks/SetFormulaInvoker); nil is fine — Tier-3 formulas are
// then skipped exactly as the incremental engine skips them with no invoker
// wired.
//
// No global lock is taken: each batch runs its own short transaction, so a
// large backfill does not block concurrent writes for its whole duration. A
// row updated by a concurrent write between the read and the write of its
// batch is self-healing — the next backfill run (or the next incremental
// write) settles it.
func Backfill(ctx context.Context, db *gorm.DB, reg *HookRegistry, m manifest.Manifest, opts BackfillOptions) ([]BackfillModelReport, error) {
	if db == nil {
		return nil, fmt.Errorf("compute.Backfill: db is required")
	}
	if opts.OrgID == uuid.Nil {
		return nil, fmt.Errorf("compute.Backfill: OrgID is required")
	}
	if opts.BatchSize <= 0 {
		opts.BatchSize = DefaultBackfillBatchSize
	}

	bindings := BuildComputeBindings(m)
	tbl := buildModelTableIndex(m)

	var invoker FormulaInvoker
	if reg != nil {
		invoker = reg.getFormulaInvoker()
	}

	var reports []BackfillModelReport

	// Tier-2 first: formulas, per owning model.
	for _, md := range m.ModelDefinitions {
		if opts.ModelKey != "" && md.ModelKey != opts.ModelKey {
			continue
		}
		fb, ok := bindings.formulasByModel[md.ModelKey]
		if !ok {
			continue
		}
		fb.formulas = filterFormulas(fb.formulas, opts.Fields)
		if len(fb.formulas) == 0 {
			continue
		}
		reports = append(reports, backfillFormulas(ctx, db, invoker, opts, md, fb))
	}

	// Tier-1: rollups, per parent model / relation.
	for _, md := range m.ModelDefinitions {
		if opts.ModelKey != "" && md.ModelKey != opts.ModelKey {
			continue
		}
		for _, rel := range md.Relations {
			if len(rel.Rollups) == 0 {
				continue
			}
			rollups := filterRollups(rel.Rollups, opts.Fields)
			if len(rollups) == 0 {
				continue
			}
			parentTable := tbl[md.ModelKey]
			childTable := tbl[rel.Through]
			if parentTable == "" || childTable == "" || rel.ForeignKey == "" {
				continue
			}
			b := rollupBinding{
				parentModel: md.ModelKey,
				parentTable: parentTable,
				childModel:  rel.Through,
				childTable:  childTable,
				fk:          rel.ForeignKey,
				rollups:     rollups,
			}
			reports = append(reports, backfillRollup(ctx, db, opts, md.OrgScoped, md.SoftDelete, b))
		}
	}

	return reports, nil
}

func filterFormulas(all []manifest.Formula, fields []string) []manifest.Formula {
	if len(fields) == 0 {
		return all
	}
	want := toSet(fields)
	out := make([]manifest.Formula, 0, len(all))
	for _, f := range all {
		if _, ok := want[f.Target]; ok {
			out = append(out, f)
		}
	}
	return out
}

func filterRollups(all []manifest.Rollup, fields []string) []manifest.Rollup {
	if len(fields) == 0 {
		return all
	}
	want := toSet(fields)
	out := make([]manifest.Rollup, 0, len(all))
	for _, r := range all {
		if _, ok := want[r.Target]; ok {
			out = append(out, r)
		}
	}
	return out
}

func toSet(ss []string) map[string]struct{} {
	m := make(map[string]struct{}, len(ss))
	for _, s := range ss {
		m[s] = struct{}{}
	}
	return m
}

// backfillFormulas recomputes fb.formulas for every row of md, batched by id.
func backfillFormulas(ctx context.Context, db *gorm.DB, invoker FormulaInvoker, opts BackfillOptions, md manifest.ModelDefinition, fb formulaBinding) BackfillModelReport {
	rep := BackfillModelReport{Model: md.ModelKey, Tier: 2}
	started := time.Now()
	defer func() { rep.Duration = time.Since(started) }()

	var after *uuid.UUID
	for {
		rows, err := fetchBatch(ctx, db, md.TableName, opts.OrgID, md.OrgScoped, md.SoftDelete, after, opts.BatchSize)
		if err != nil {
			rep.Errors = append(rep.Errors, err.Error())
			return rep
		}
		if len(rows) == 0 {
			break
		}
		for _, row := range rows {
			rep.RowsScanned++
			id, ok := rowID(row)
			if !ok {
				continue
			}
			input := map[string]any{}
			if err := applyFormulas(ctx, invoker, fb, input, row); err != nil {
				rep.Errors = append(rep.Errors, fmt.Sprintf("id=%s: %v", id, err))
				continue
			}
			changed := diffValues(row, input)
			if len(changed) == 0 {
				continue
			}
			rep.RowsUpdated++
			if opts.DryRun {
				continue
			}
			if err := updateRow(ctx, db, md.TableName, id, opts.OrgID, md.OrgScoped, changed); err != nil {
				rep.Errors = append(rep.Errors, fmt.Sprintf("id=%s: %v", id, err))
			}
		}
		last, _ := rowID(rows[len(rows)-1])
		after = &last
		if len(rows) < opts.BatchSize {
			break
		}
	}
	return rep
}

// backfillRollup recomputes b.rollups for every parent row of the model
// owning b, batched by id. Within a batch it computes every parent's targets
// with a single grouped SELECT over the child table instead of one query per
// parent row.
func backfillRollup(ctx context.Context, db *gorm.DB, opts BackfillOptions, parentOrgScoped, parentSoftDelete bool, b rollupBinding) BackfillModelReport {
	rep := BackfillModelReport{Model: b.parentModel, Tier: 1}
	started := time.Now()
	defer func() { rep.Duration = time.Since(started) }()

	targets := make([]string, 0, len(b.rollups))
	for _, r := range b.rollups {
		targets = append(targets, r.Target)
	}

	var after *uuid.UUID
	for {
		parents, err := fetchParentBatch(ctx, db, b.parentTable, targets, opts.OrgID, parentOrgScoped, parentSoftDelete, after, opts.BatchSize)
		if err != nil {
			rep.Errors = append(rep.Errors, err.Error())
			return rep
		}
		if len(parents) == 0 {
			break
		}
		ids := make([]uuid.UUID, 0, len(parents))
		for _, p := range parents {
			if id, ok := rowID(p); ok {
				ids = append(ids, id)
			}
		}
		computed, err := computeRollupBatch(ctx, db, b, ids)
		if err != nil {
			rep.Errors = append(rep.Errors, err.Error())
		} else {
			for _, p := range parents {
				rep.RowsScanned++
				id, ok := rowID(p)
				if !ok {
					continue
				}
				vals, ok := computed[id]
				if !ok {
					vals = zeroRollupValues(b.rollups)
				}
				changed := diffValues(p, vals)
				if len(changed) == 0 {
					continue
				}
				rep.RowsUpdated++
				if opts.DryRun {
					continue
				}
				if err := updateRow(ctx, db, b.parentTable, id, opts.OrgID, parentOrgScoped, changed); err != nil {
					rep.Errors = append(rep.Errors, fmt.Sprintf("id=%s: %v", id, err))
				}
			}
		}
		last, _ := rowID(parents[len(parents)-1])
		after = &last
		if len(parents) < opts.BatchSize {
			break
		}
	}
	return rep
}

// zeroRollupValues is the recomputed value for a parent with no (remaining)
// child rows — COALESCE(...,0) semantics, matching recomputeRollups.
func zeroRollupValues(rollups []manifest.Rollup) map[string]any {
	out := make(map[string]any, len(rollups))
	for _, r := range rollups {
		out[r.Target] = 0.0
	}
	return out
}

// fetchBatch reads one page (id > after, ORDER BY id, LIMIT batchSize) of a
// table's full rows as maps, org-scoped when orgScoped and excluding
// soft-deleted rows when softDelete.
func fetchBatch(ctx context.Context, db *gorm.DB, table string, orgID uuid.UUID, orgScoped, softDelete bool, after *uuid.UUID, batchSize int) ([]map[string]any, error) {
	q := db.WithContext(ctx).Table(table)
	if orgScoped {
		q = q.Where(`"organization_id" = ?`, orgID)
	}
	if softDelete {
		q = q.Where(`"deleted_at" IS NULL`)
	}
	if after != nil {
		q = q.Where(`"id" > ?`, *after)
	}
	rows, err := q.Order(`"id" ASC`).Limit(batchSize).Rows()
	if err != nil {
		return nil, fmt.Errorf("backfill: scan %s: %w", table, err)
	}
	defer rows.Close()
	return scanRowsAsMaps(db, rows)
}

// fetchParentBatch is fetchBatch narrowed to id + the rollup target columns
// (the only columns backfillRollup needs to compare against).
func fetchParentBatch(ctx context.Context, db *gorm.DB, table string, targets []string, orgID uuid.UUID, orgScoped, softDelete bool, after *uuid.UUID, batchSize int) ([]map[string]any, error) {
	cols := []string{`"id"`}
	for _, t := range targets {
		cols = append(cols, quoteIdent(t))
	}
	q := db.WithContext(ctx).Table(table).Select(cols)
	if orgScoped {
		q = q.Where(`"organization_id" = ?`, orgID)
	}
	if softDelete {
		q = q.Where(`"deleted_at" IS NULL`)
	}
	if after != nil {
		q = q.Where(`"id" > ?`, *after)
	}
	rows, err := q.Order(`"id" ASC`).Limit(batchSize).Rows()
	if err != nil {
		return nil, fmt.Errorf("backfill: scan %s: %w", table, err)
	}
	defer rows.Close()
	return scanRowsAsMaps(db, rows)
}

// computeRollupBatch computes every rollup target for a batch of parent ids
// with one grouped SELECT over the child table (fk IN (ids...) GROUP BY fk),
// returning a map keyed by parent id. Parents absent from the result have no
// (remaining) children — the caller applies zeroRollupValues for those.
func computeRollupBatch(ctx context.Context, db *gorm.DB, b rollupBinding, ids []uuid.UUID) (map[uuid.UUID]map[string]any, error) {
	out := make(map[uuid.UUID]map[string]any, len(ids))
	if len(ids) == 0 || len(b.rollups) == 0 {
		return out, nil
	}
	var selects []string
	for _, r := range b.rollups {
		agg, err := computeAggExpr(r)
		if err != nil {
			return nil, fmt.Errorf("backfill: rollup %q: %w", r.Target, err)
		}
		selects = append(selects, fmt.Sprintf("%s AS %s", agg, quoteIdent(r.Target)))
	}
	sql := fmt.Sprintf(
		`SELECT %s AS %s, %s FROM %s WHERE %s IN ? GROUP BY %s`,
		quoteIdent(b.fk), quoteIdent("__parent_id"), joinStrings(selects, ", "),
		quoteIdent(b.childTable), quoteIdent(b.fk), quoteIdent(b.fk),
	)
	rows, err := db.WithContext(ctx).Raw(sql, ids).Rows()
	if err != nil {
		return nil, fmt.Errorf("backfill: rollup group query on %s: %w", b.childTable, err)
	}
	defer rows.Close()
	maps, err := scanRowsAsMaps(db, rows)
	if err != nil {
		return nil, err
	}
	for _, m := range maps {
		pid, ok := m["__parent_id"]
		if !ok {
			continue
		}
		id, err := parseUUIDish(pid)
		if err != nil {
			continue
		}
		delete(m, "__parent_id")
		for k, v := range m {
			m[k] = numericValue(v)
		}
		out[id] = m
	}
	return out, nil
}

func joinStrings(ss []string, sep string) string {
	out := ""
	for i, s := range ss {
		if i > 0 {
			out += sep
		}
		out += s
	}
	return out
}

// scanRowsAsMaps drains a *sql.Rows into one map[string]any per row using
// gorm's ScanRows (so driver-specific value conversion matches the rest of
// the codebase's map-scan call sites, e.g. loadRowValues). Values come back
// dereferenced — ScanRows on a schema-less Table()/Raw() query can surface
// pointer-typed driver values (e.g. *float64), and every downstream consumer
// here (computeexpr.ToFloat via applyFormulas, diffValues/numericValue,
// row/arg values) expects plain scalars.
func scanRowsAsMaps(db *gorm.DB, rows *sql.Rows) ([]map[string]any, error) {
	var out []map[string]any
	for rows.Next() {
		row := map[string]any{}
		if err := db.ScanRows(rows, &row); err != nil {
			return nil, fmt.Errorf("backfill: scan row: %w", err)
		}
		out = append(out, dereferenceRow(row))
	}
	return out, rows.Err()
}

// dereferenceRow replaces every pointer-typed value in row with the value it
// points to (nil pointer -> nil), in place.
func dereferenceRow(row map[string]any) map[string]any {
	for k, v := range row {
		rv := reflect.ValueOf(v)
		if rv.Kind() != reflect.Ptr {
			continue
		}
		if rv.IsNil() {
			row[k] = nil
			continue
		}
		row[k] = rv.Elem().Interface()
	}
	return row
}

// rowID extracts and parses the "id" column of a scanned row map.
func rowID(row map[string]any) (uuid.UUID, bool) {
	v, ok := row["id"]
	if !ok || v == nil {
		return uuid.UUID{}, false
	}
	id, err := parseUUIDish(v)
	if err != nil {
		return uuid.UUID{}, false
	}
	return id, true
}

func parseUUIDish(v any) (uuid.UUID, error) {
	switch x := v.(type) {
	case uuid.UUID:
		return x, nil
	case string:
		return uuid.Parse(x)
	case []byte:
		return uuid.Parse(string(x))
	case fmt.Stringer:
		return uuid.Parse(x.String())
	default:
		return uuid.UUID{}, fmt.Errorf("backfill: id column is not uuid-shaped: %T", v)
	}
}

// diffValues compares candidate's keys against current's, returning only the
// keys whose recomputed value differs by more than backfillEpsilon (rollups
// and Tier-2 formulas are always numeric).
func diffValues(current map[string]any, candidate map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range candidate {
		cur, existed := current[k]
		newF := numericValue(v)
		if !existed {
			out[k] = newF
			continue
		}
		curF := numericValue(cur)
		if math.Abs(curF-toFloat(newF)) > backfillEpsilon {
			out[k] = newF
		}
	}
	return out
}

// numericValue normalizes a scanned/computed value to float64 for comparison
// and for the eventual UPDATE arg (rollup/formula target columns are
// numeric). Raw aggregate SELECTs scanned via gorm's ScanRows can surface
// pointer-typed driver values (e.g. *float64) instead of the dereferenced
// scalar, hence the reflect fallback.
func numericValue(v any) float64 {
	switch x := v.(type) {
	case nil:
		return 0
	case float64:
		return x
	case float32:
		return float64(x)
	case int64:
		return float64(x)
	case int32:
		return float64(x)
	case int:
		return float64(x)
	case []byte:
		var f float64
		fmt.Sscanf(string(x), "%f", &f)
		return f
	case string:
		var f float64
		fmt.Sscanf(x, "%f", &f)
		return f
	default:
		rv := reflect.ValueOf(v)
		for rv.Kind() == reflect.Ptr {
			if rv.IsNil() {
				return 0
			}
			rv = rv.Elem()
		}
		if !rv.IsValid() {
			return 0
		}
		switch rv.Kind() {
		case reflect.Float32, reflect.Float64:
			return rv.Float()
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
			return float64(rv.Int())
		default:
			return 0
		}
	}
}

func toFloat(v any) float64 {
	if f, ok := v.(float64); ok {
		return f
	}
	return numericValue(v)
}

// updateRow writes `changed` onto one row by id (+ org, when orgScoped).
func updateRow(ctx context.Context, db *gorm.DB, table string, id uuid.UUID, orgID uuid.UUID, orgScoped bool, changed map[string]any) error {
	if len(changed) == 0 {
		return nil
	}
	q := db.WithContext(ctx).Table(table).Where(`"id" = ?`, id)
	if orgScoped {
		q = q.Where(`"organization_id" = ?`, orgID)
	}
	return q.Updates(changed).Error
}
