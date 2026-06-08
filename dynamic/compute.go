package dynamic

// compute.go implements the kernel's DECLARATIVE COMPUTE ENGINE — a
// domain-agnostic interpreter for two manifest-declared computation tiers,
// wired onto the per-model CRUD hooks:
//
//	Tier-1 ROLLUP  — a PARENT column maintained as an aggregate (sum/count/
//	                 avg/min/max) over a relation's child rows. Declared on the
//	                 parent model's relation: relations[].rollups[]. Recomputed
//	                 on every child create/update/delete via one UPDATE.
//
//	Tier-2 FORMULA — a column computed from a pure arithmetic expression over
//	                 the SAME row's other columns. Declared on the model:
//	                 models[].formulas[]. Evaluated before the model's own
//	                 create/update DB write.
//
//	Tier-3 (reserved) — wasm-backed computed fields; not implemented here.
//
// The kernel never knows what a "SalesOrder" is — it only sees rollup/formula
// specs and column/table names resolved from the manifest. All expressions are
// parsed with a STRICT arithmetic allowlist (see manifest/computeexpr);
// identifiers must
// resolve to real columns and are double-quoted when SQL is built, so neither
// tier can inject SQL.

import (
	"context"
	"fmt"
	"strings"

	"github.com/asteby/metacore-kernel/manifest"
	"github.com/asteby/metacore-kernel/manifest/computeexpr"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// rollupBinding is a resolved Tier-1 spec keyed by CHILD model. Recomputing
// fires on the child's CRUD hooks; the binding carries everything needed to
// build the single parent UPDATE without re-walking the manifest.
type rollupBinding struct {
	parentModel string            // owning model key (for diagnostics)
	parentTable string            // physical parent table (org-scoped)
	childModel  string            // relation.Through (child model key)
	childTable  string            // physical child table (NOT org-scoped)
	fk          string            // FK column on the child pointing at the parent
	rollups     []manifest.Rollup // every aggregate target to recompute together
}

// formulaBinding is a resolved Tier-2 spec keyed by the model it computes on.
// Evaluated in declared order against the incoming input (merged with the
// existing row on update).
type formulaBinding struct {
	model    string
	table    string
	formulas []manifest.Formula
}

// ComputeBindings is the precomputed index the hooks consult at runtime.
type ComputeBindings struct {
	// rollupsByChild maps a child model key to the rollup bindings whose
	// children it owns. A child model can feed multiple parents (multiple
	// relations declaring rollups Through the same child), hence a slice.
	rollupsByChild map[string][]rollupBinding
	// formulasByModel maps a model key to its formula binding.
	formulasByModel map[string]formulaBinding
}

// buildModelTableIndex resolves model key -> physical table name, built once
// per manifest from ModelDefinitions.
func buildModelTableIndex(m manifest.Manifest) map[string]string {
	table := make(map[string]string, len(m.ModelDefinitions))
	for _, md := range m.ModelDefinitions {
		table[md.ModelKey] = md.TableName
	}
	return table
}

// BuildComputeBindings walks a manifest and produces the runtime index of
// Tier-1 rollups (keyed by child model) and Tier-2 formulas (keyed by model).
// It is best-effort: a rollup whose parent/child model or table cannot be
// resolved is skipped (validation has already rejected malformed manifests at
// install/publish time, so this only guards against partial manifests fed
// directly to the engine in tests).
func BuildComputeBindings(m manifest.Manifest) ComputeBindings {
	tbl := buildModelTableIndex(m)
	out := ComputeBindings{
		rollupsByChild:  make(map[string][]rollupBinding),
		formulasByModel: make(map[string]formulaBinding),
	}
	for _, md := range m.ModelDefinitions {
		// Tier-1: rollups live on the PARENT model's relations, fire on the
		// CHILD (relation.Through).
		for _, rel := range md.Relations {
			if len(rel.Rollups) == 0 {
				continue
			}
			parentTable := tbl[md.ModelKey]
			childTable := tbl[rel.Through]
			if parentTable == "" || childTable == "" || rel.ForeignKey == "" {
				continue
			}
			out.rollupsByChild[rel.Through] = append(out.rollupsByChild[rel.Through], rollupBinding{
				parentModel: md.ModelKey,
				parentTable: parentTable,
				childModel:  rel.Through,
				childTable:  childTable,
				fk:          rel.ForeignKey,
				rollups:     rel.Rollups,
			})
		}
		// Tier-2: formulas fire on the model that owns them.
		if len(md.Formulas) > 0 {
			out.formulasByModel[md.ModelKey] = formulaBinding{
				model:    md.ModelKey,
				table:    tbl[md.ModelKey],
				formulas: md.Formulas,
			}
		}
	}
	return out
}

// computeAggExpr returns the SQL aggregate expression for a single rollup. For
// count it is COUNT(*); otherwise the function wraps either the From column
// (double-quoted identifier) or the Expr (already validated + rendered with
// double-quoted identifiers). Caller wraps the whole SELECT in COALESCE(...,0).
func computeAggExpr(r manifest.Rollup) (string, error) {
	fn := strings.ToLower(strings.TrimSpace(r.Fn))
	if fn == "" {
		fn = "sum"
	}
	if fn == "count" {
		return "COUNT(*)", nil
	}
	var inner string
	switch {
	case r.From != "":
		if !computeexpr.IdentRe.MatchString(r.From) {
			return "", fmt.Errorf("rollup from %q is not a valid column identifier", r.From)
		}
		inner = `"` + r.From + `"`
	case r.Expr != "":
		rendered, err := computeexpr.RenderSQL(r.Expr)
		if err != nil {
			return "", fmt.Errorf("rollup expr %q: %w", r.Expr, err)
		}
		inner = rendered
	default:
		return "", fmt.Errorf("rollup %q: fn=%s requires from or expr", r.Target, fn)
	}
	switch fn {
	case "sum":
		return "SUM(" + inner + ")", nil
	case "avg":
		return "AVG(" + inner + ")", nil
	case "min":
		return "MIN(" + inner + ")", nil
	case "max":
		return "MAX(" + inner + ")", nil
	default:
		return "", fmt.Errorf("rollup %q: unknown fn %q", r.Target, fn)
	}
}

// recomputeRollups recomputes EVERY rollup target on `binding` for one parent
// row via a single UPDATE. The child subquery is scoped ONLY by the FK (child
// tables are not org-scoped); the parent UPDATE is scoped by id + org. When
// excludeChildID is non-nil an `AND "id" <> ?` is appended to each child
// subquery so a row about to be deleted is excluded from the aggregate
// (BeforeDelete path — the row still exists at recompute time).
//
// An empty child set zeroes the target via COALESCE(...,0), which is the
// correct ERP behaviour (deleting the last line resets the parent total).
func recomputeRollups(ctx context.Context, db *gorm.DB, orgID uuid.UUID, b rollupBinding, parentID string, excludeChildID *string) error {
	if len(b.rollups) == 0 || parentID == "" {
		return nil
	}
	sql, args, err := buildRollupSQL(b, parentID, orgID.String(), excludeChildID)
	if err != nil {
		return err
	}
	return db.WithContext(ctx).Exec(sql, args...).Error
}

// buildRollupSQL constructs the single parent UPDATE that recomputes every
// rollup target on `b`, plus its positional args. Split out from
// recomputeRollups so it can be asserted in a unit test without a DB. The
// child subquery is scoped ONLY by the FK (child tables aren't org-scoped);
// the parent UPDATE is scoped by id + org. When excludeChildID is non-nil each
// child subquery gains `AND "id" <> ?` (BeforeDelete exclusion).
func buildRollupSQL(b rollupBinding, parentID, orgID string, excludeChildID *string) (string, []any, error) {
	var sets []string
	var args []any

	// Defense-in-depth: the FK column comes from the manifest relation and is
	// interpolated into the WHERE clause. It's validated at publish/install, but
	// re-check here so a malformed identifier can never reach raw SQL.
	if !computeexpr.IdentRe.MatchString(b.fk) {
		return "", nil, fmt.Errorf("rollup fk %q is not a valid column identifier", b.fk)
	}

	exclusion := ""
	if excludeChildID != nil {
		exclusion = ` AND "id" <> ?`
	}

	for _, r := range b.rollups {
		if !computeexpr.IdentRe.MatchString(r.Target) {
			return "", nil, fmt.Errorf("rollup target %q is not a valid column identifier", r.Target)
		}
		agg, err := computeAggExpr(r)
		if err != nil {
			return "", nil, err
		}
		sub := fmt.Sprintf(
			`COALESCE((SELECT %s FROM %s WHERE "%s" = ?%s), 0)`,
			agg, quoteIdent(b.childTable), b.fk, exclusion,
		)
		sets = append(sets, fmt.Sprintf(`"%s" = %s`, r.Target, sub))
		args = append(args, parentID)
		if excludeChildID != nil {
			args = append(args, *excludeChildID)
		}
	}

	sql := fmt.Sprintf(
		`UPDATE %s SET %s WHERE "id" = ? AND "organization_id" = ?`,
		quoteIdent(b.parentTable), strings.Join(sets, ", "),
	)
	args = append(args, parentID, orgID)
	return sql, args, nil
}

// applyFormulas evaluates each Tier-2 formula in declared order and writes the
// result into input[target] BEFORE the DB write. The evaluation environment is
// the incoming input merged over existing (existing supplies unreferenced
// columns on update; pass nil on create). Missing / non-numeric identifiers
// resolve to 0. Formulas are a single pass in declared order — if formula B
// references a column formula A sets, A must be declared first.
func applyFormulas(b formulaBinding, input map[string]any, existing map[string]any) error {
	if len(b.formulas) == 0 {
		return nil
	}
	env := make(map[string]float64)
	for k, v := range existing {
		env[k] = computeexpr.ToFloat(v)
	}
	for k, v := range input {
		env[k] = computeexpr.ToFloat(v)
	}
	for _, f := range b.formulas {
		val, err := computeexpr.Eval(f.Expr, env)
		if err != nil {
			return fmt.Errorf("formula %q expr %q: %w", f.Target, f.Expr, err)
		}
		input[f.Target] = val
		// Make the just-computed value visible to subsequent formulas.
		env[f.Target] = val
	}
	return nil
}

// RegisterComputeHooks wires the declarative compute engine onto the dynamic
// HookRegistry for one manifest. It is idempotent per (registry, manifest)
// only insofar as the underlying registry de-dupes by addon — call it once per
// install alongside RegisterManifestHooks. A nil registry is a no-op.
//
// Tier-1 (rollups): on the CHILD model's AfterCreate / AfterUpdate /
// BeforeDelete it recomputes every rollup target on the affected parent. The
// DB handle and org come from the per-request HookContext (hc.DB / hc.User), so
// the recompute runs in the same request/transaction context as the child
// write.
//
// Tier-2 (formulas): on the OWNING model's BeforeCreate / BeforeUpdate it
// mutates the input map in place before the DB write.
//
// ORDERING: the dynamic Service fires before-hooks, then writes, then
// after-hooks. So a child line's Tier-2 formula (BeforeCreate) mutates subtotal
// before the row is written, and the parent's Tier-1 rollup (AfterCreate) then
// SUMs the committed value — self-computing line items roll up correctly within
// a single Create().
func RegisterComputeHooks(reg *HookRegistry, m manifest.Manifest) {
	if reg == nil {
		return
	}
	b := BuildComputeBindings(m)

	// --- Tier-1: rollups, fired on the child model ---
	for child, bindings := range b.rollupsByChild {
		bindings := bindings // capture

		reg.RegisterAfterCreate(child, func(ctx context.Context, hc HookContext, record any) error {
			row := recordToMap(record)
			for _, bd := range bindings {
				parentID := stringField(row, bd.fk)
				if parentID == "" {
					continue
				}
				if err := recomputeRollups(ctx, hc.DB, orgIDFromUser(hc.User), bd, parentID, nil); err != nil {
					return err
				}
			}
			return nil
		})

		reg.RegisterAfterUpdate(child, func(ctx context.Context, hc HookContext, record any) error {
			// TODO(reparenting): if the FK changed on update, the OLD parent is
			// not recomputed here — only the new parent. v1 treats FK reparenting
			// as out of scope. The common case (editing a line's quantity/price)
			// keeps the same FK and recomputes correctly.
			row := recordToMap(record)
			for _, bd := range bindings {
				parentID := stringField(row, bd.fk)
				if parentID == "" {
					continue
				}
				if err := recomputeRollups(ctx, hc.DB, orgIDFromUser(hc.User), bd, parentID, nil); err != nil {
					return err
				}
			}
			return nil
		})

		reg.RegisterBeforeDelete(child, func(ctx context.Context, hc HookContext, id string) error {
			// The row still exists. Load its FK(s) and recompute each parent
			// WITH the about-to-be-deleted child excluded, so we don't need an
			// AfterDelete + state handoff. Tradeoff: if the subsequent delete
			// fails the parent is briefly wrong — rare and self-healing on the
			// next child write.
			for _, bd := range bindings {
				parentID, err := loadChildFK(ctx, hc.DB, bd.childTable, bd.fk, id)
				if err != nil {
					return err
				}
				if parentID == "" {
					continue
				}
				excl := id
				if err := recomputeRollups(ctx, hc.DB, orgIDFromUser(hc.User), bd, parentID, &excl); err != nil {
					return err
				}
			}
			return nil
		})
	}

	// --- Tier-2: formulas, fired on the owning model ---
	for model, fb := range b.formulasByModel {
		fb := fb // capture

		reg.RegisterBeforeCreate(model, func(ctx context.Context, hc HookContext, input map[string]any) error {
			return applyFormulas(fb, input, nil)
		})

		reg.RegisterBeforeUpdate(model, func(ctx context.Context, hc HookContext, id string, input map[string]any) error {
			// Merge with the existing row so a formula referencing a column the
			// caller didn't include in the partial update still sees its value.
			existing := loadRowValues(ctx, hc.DB, fb.table, id)
			return applyFormulas(fb, input, existing)
		})
	}
}

// recordToMap projects a hook record (a *struct or already a map) to a
// map[string]any so the FK can be read uniformly.
func recordToMap(record any) map[string]any {
	if record == nil {
		return nil
	}
	if mp, ok := record.(map[string]any); ok {
		return mp
	}
	return toMap(record)
}

// stringField reads a string-ish field from a row map (uuid FK columns may
// surface as string or fmt.Stringer).
func stringField(row map[string]any, key string) string {
	if row == nil {
		return ""
	}
	v, ok := row[key]
	if !ok || v == nil {
		return ""
	}
	switch x := v.(type) {
	case string:
		return x
	case uuid.UUID:
		if x == uuid.Nil {
			return ""
		}
		return x.String()
	case fmt.Stringer:
		return x.String()
	default:
		return fmt.Sprintf("%v", x)
	}
}

// loadChildFK reads the FK value off a child row by id (used on BeforeDelete,
// where the record map isn't passed to the hook). Returns "" if not found.
func loadChildFK(ctx context.Context, db *gorm.DB, childTable, fk, id string) (string, error) {
	var out map[string]any
	err := db.WithContext(ctx).
		Table(childTable).
		Select(quoteIdent(fk)).
		Where(`"id" = ?`, id).
		Take(&out).Error
	if err != nil {
		if err == gorm.ErrRecordNotFound {
			return "", nil
		}
		return "", err
	}
	return stringField(out, fk), nil
}

// loadRowValues reads an entire row by id as a map for the formula update path.
// A miss returns nil (formulas then evaluate against the input alone).
func loadRowValues(ctx context.Context, db *gorm.DB, table, id string) map[string]any {
	var out map[string]any
	err := db.WithContext(ctx).Table(table).Where(`"id" = ?`, id).Take(&out).Error
	if err != nil {
		return nil
	}
	return out
}

// quoteIdent double-quotes a SQL identifier (table name). Identifiers reaching
// here are validated column/table names; the quote prevents the DDL/DML
// generator's %s from interpolating an unquoted token.
func quoteIdent(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}
