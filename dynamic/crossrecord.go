package dynamic

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/asteby/metacore-kernel/manifest"
	"github.com/asteby/metacore-kernel/manifest/computeexpr"
)

var crossIdent = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// crossSumEpsilon absorbs float64 noise when comparing a decimal sum to its cap.
const crossSumEpsilon = 1e-9

// EvalCrossRecordRules evaluates a model's cross-record rules (manifest
// Model.rules) for one write. tx MUST be the transaction the write runs in (or
// will run in): sum_lte locks the parent row FOR UPDATE on it, so two concurrent
// writers against the same parent serialize and cannot both slip under the cap.
//
// row is the row the write settles on (the merged post-update row on update);
// before is the prior row on update, nil on create. The row may or may not
// already be in the table: sibling sums always exclude row["id"] and add the
// row's own contribution, so the pre-write (dynamic.Service) and post-write
// (wasm data_mutate) call sites are equivalent. parentTable maps a parent model
// key to its physical table. A violation returns a *ConstraintError
// (errors.Is ErrConstraintViolation → 422 with the rule's error_key).
func EvalCrossRecordRules(ctx context.Context, tx *gorm.DB, rules []manifest.CrossRuleDef, ownTable string, orgID uuid.UUID, row, before map[string]any, parentTable func(model string) (string, error)) error {
	for _, r := range rules {
		if err := evalCrossRule(ctx, tx, r, ownTable, orgID, row, before, parentTable); err != nil {
			return err
		}
	}
	return nil
}

func crossViolation(r manifest.CrossRuleDef, expr string, values map[string]any) error {
	return &ConstraintError{ErrorKey: r.ErrorKey, Expr: expr, Def: manifest.ConstraintDef{ErrorKey: r.ErrorKey}, Values: values}
}

func sameCol(before, row map[string]any, cols ...string) bool {
	for _, c := range cols {
		if fmt.Sprint(before[c]) != fmt.Sprint(row[c]) {
			return false
		}
	}
	return true
}

func evalCrossRule(ctx context.Context, tx *gorm.DB, r manifest.CrossRuleDef, ownTable string, orgID uuid.UUID, row, before map[string]any, parentTable func(string) (string, error)) error {
	if !crossIdent.MatchString(r.Ref) || !crossIdent.MatchString(r.Sum) && r.Sum != "" || !crossIdent.MatchString(r.Max) && r.Max != "" {
		return fmt.Errorf("%w: cross rule %q: invalid identifier", ErrInvalidInput, r.ErrorKey)
	}
	refV, ok := row[r.Ref]
	if !ok || refV == nil || fmt.Sprint(refV) == "" {
		return nil // optional FK left empty: nothing to relate to
	}
	refStr := fmt.Sprint(refV)

	switch r.Kind {
	case "ref_state":
		if before != nil && sameCol(before, row, r.Ref) {
			return nil // untouched relation: a later edit must not be blocked by a state change
		}
	case "sum_lte":
		if before != nil && sameCol(before, row, append([]string{r.Ref, r.Sum}, keysOf(r.Where)...)...) {
			return nil
		}
	default:
		return fmt.Errorf("%w: cross rule %q: unknown kind %q", ErrInvalidInput, r.ErrorKey, r.Kind)
	}

	pt, err := parentTable(r.Parent)
	if err != nil {
		return fmt.Errorf("%w: cross rule %q: parent %q: %v", ErrInvalidInput, r.ErrorKey, r.Parent, err)
	}
	q := scopedTable(tx.WithContext(ctx), pt, orgID).Where("id = ?", refStr)
	if r.Kind == "sum_lte" {
		q = q.Clauses(clause.Locking{Strength: "UPDATE"})
	}
	parent := map[string]any{}
	if err := q.Take(&parent).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			return crossViolation(r, r.Kind+" "+r.Ref, map[string]any{r.Ref: refStr, "parent": "not_found"})
		}
		return err
	}

	if r.Kind == "ref_state" {
		for col, want := range r.Require {
			if !crossIdent.MatchString(col) {
				return fmt.Errorf("%w: cross rule %q: invalid identifier %q", ErrInvalidInput, r.ErrorKey, col)
			}
			if !crossAccepts(want, parent[col]) {
				return crossViolation(r, fmt.Sprintf("ref_state %s.%s", r.Parent, col), map[string]any{col: crossStr(parent[col])})
			}
		}
		return nil
	}

	// sum_lte
	mine := 0.0
	if crossMatches(r.Where, row) {
		mine = computeexpr.ToFloat(row[r.Sum])
	}
	sq := scopedTable(tx.WithContext(ctx), ownTable, orgID).Where(r.Ref+" = ?", refStr)
	if id := fmt.Sprint(row["id"]); row["id"] != nil && id != "" {
		sq = sq.Where("id <> ?", id)
	}
	for col, want := range r.Where {
		if !crossIdent.MatchString(col) {
			return fmt.Errorf("%w: cross rule %q: invalid identifier %q", ErrInvalidInput, r.ErrorKey, col)
		}
		sq = sq.Where(col+" IN ?", crossList(want))
	}
	var others float64
	if err := sq.Select("COALESCE(SUM(" + r.Sum + "), 0)").Scan(&others).Error; err != nil {
		return err
	}
	limit := computeexpr.ToFloat(parent[r.Max])
	if others+mine > limit+crossSumEpsilon {
		return crossViolation(r, fmt.Sprintf("sum(%s) <= %s.%s", r.Sum, r.Parent, r.Max),
			map[string]any{"sum": others + mine, "max": limit})
	}
	return nil
}

// scopedTable narrows a physical table to the tenant and drops soft-deleted
// rows, each only when the table actually has that column (child tables have
// no organization_id of their own).
func scopedTable(db *gorm.DB, table string, orgID uuid.UUID) *gorm.DB {
	q := db.Table(table)
	if orgID != uuid.Nil && db.Migrator().HasColumn(table, "organization_id") {
		q = q.Where("organization_id = ?", orgID)
	}
	if db.Migrator().HasColumn(table, "deleted_at") {
		q = q.Where("deleted_at IS NULL")
	}
	return q
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func crossStr(v any) string {
	if b, ok := v.([]byte); ok {
		return string(b)
	}
	return fmt.Sprint(v)
}

func crossList(want any) []any {
	if l, ok := want.([]any); ok {
		return l
	}
	return []any{want}
}

func crossAccepts(want, got any) bool {
	g := strings.TrimSpace(crossStr(got))
	for _, w := range crossList(want) {
		if fmt.Sprint(w) == g {
			return true
		}
	}
	return false
}

func crossMatches(where map[string]any, row map[string]any) bool {
	for col, want := range where {
		if !crossAccepts(want, row[col]) {
			return false
		}
	}
	return true
}

// parentTableFn resolves a parent model key to its physical table through the
// same registry Service uses for its own models.
func (s *Service) parentTableFn(ctx context.Context) func(string) (string, error) {
	return func(model string) (string, error) {
		inst, _, err := s.resolveModel(ctx, model)
		if err != nil {
			return "", err
		}
		return s.tableNameFor(ctx, model, inst)
	}
}

// CrossRecordCompute adapts EvalCrossRecordRules to the wasm host's
// MutationComputeFn signature so an embedder enforces Model.rules on
// data_mutate / data_batch by chaining it (before or after its rollup pass) in
// Host.WithMutationCompute. rulesFor returns a logical table's rules;
// parentTable maps a parent model key to its physical table. Deletes are
// exempt: rules predicate over a resulting row.
func CrossRecordCompute(rulesFor func(logicalTable string) []manifest.CrossRuleDef, ownTable func(logicalTable string) string, parentTable func(model string) (string, error)) func(ctx context.Context, tx *gorm.DB, orgID uuid.UUID, logicalTable, action string, row map[string]any) error {
	return func(ctx context.Context, tx *gorm.DB, orgID uuid.UUID, logicalTable, action string, row map[string]any) error {
		if action == "deleted" {
			return nil
		}
		rules := rulesFor(logicalTable)
		if len(rules) == 0 {
			return nil
		}
		// The wasm path has no `before`: ref_state/sum_lte re-check on updates.
		return EvalCrossRecordRules(ctx, tx, rules, ownTable(logicalTable), orgID, row, nil, parentTable)
	}
}
