package dynamic

import (
	"context"
	"fmt"
	"log"
	"strings"

	"github.com/asteby/metacore-kernel/manifest"
	"github.com/asteby/metacore-kernel/manifest/computeexpr"
)

// ModelConstraints is the resolved guard configuration for one model: the flat
// list of declarative predicates (gathered across the model's columns) plus the
// row-locking strategy. A host builds it straight off the installed manifest.
type ModelConstraints struct {
	// Constraints are the guard predicates, evaluated in declared order before
	// every create/update write.
	Constraints []manifest.ConstraintDef
	// Locking is "row" to take a SELECT … FOR UPDATE lock on the target row
	// inside a transaction on Update (so an increment-then-check guard is
	// race-free), or "" for no extra locking.
	Locking string
	// Rules are the model's cross-record rules (manifest Model.rules). Unlike
	// Constraints they need a transaction and the parent row, so Service and the
	// wasm mutation path evaluate them via EvalCrossRecordRules.
	Rules []manifest.CrossRuleDef
}

// ConstraintResolver returns the guard configuration for a model name. Hosts
// wire it from their addon registry; the kernel owns no global model index.
// Returning (nil, false) — or an empty set — disables guards for that model,
// leaving Create/Update unrestricted (the legacy behaviour).
type ConstraintResolver func(ctx context.Context, model string) (*ModelConstraints, bool)

// active reports whether the config actually carries guard predicates. Locking
// alone (no constraints) is inert — there is nothing to serialize on.
func (mc *ModelConstraints) active() bool {
	return mc != nil && (len(mc.Constraints) > 0 || len(mc.Rules) > 0)
}

// locksRows reports whether Update should take a FOR UPDATE row lock. Only
// meaningful together with constraints.
func (mc *ModelConstraints) locksRows() bool {
	return mc.active() && strings.EqualFold(mc.Locking, "row")
}

// resolveConstraints looks up a model's guard config via the host resolver, or
// nil when none is wired / declared.
func (s *Service) resolveConstraints(ctx context.Context, model string) *ModelConstraints {
	if s.constraints == nil {
		return nil
	}
	mc, ok := s.constraints(ctx, model)
	if !ok || !mc.active() {
		return nil
	}
	return mc
}

// EvalRowConstraints is the exported form of evalConstraints for embedders
// that enforce declarative guards on writes that bypass Service (the wasm
// data_mutate/data_batch imports route through it via Host.WithMutationGuard).
func EvalRowConstraints(mc *ModelConstraints, row map[string]any) error {
	if mc == nil {
		return nil
	}
	return evalConstraints(mc, row)
}

// evalConstraints evaluates every guard predicate against the row's numeric
// environment (missing / non-numeric identifiers resolve to 0, matching the
// Tier-2 formula engine). A false predicate aborts with a *ConstraintError
// carrying its ErrorKey + declaration; a malformed predicate is a *400*-class
// input error (invalid manifest reaching runtime) surfaced as a plain error.
//
// Precedence: every guard is evaluated. A failed REJECTING guard always wins
// (the write is refused outright); only when every failure is of the
// `request_approval` kind does the first such failure come back — the caller
// then parks the mutation for a supervisor instead of refusing it. This keeps
// "stock cannot go negative" from being bypassed by "price below minimum
// needs a manager".
func evalConstraints(mc *ModelConstraints, row map[string]any) error {
	return evalConstraintsSkipping(mc, row, "")
}

// evalConstraintsSkipping is evalConstraints with ONE guard exempted by its
// ErrorKey — the replay of an approved request skips exactly the guard the
// supervisor approved and nothing else. skipKey == "" exempts nothing.
func evalConstraintsSkipping(mc *ModelConstraints, row map[string]any, skipKey string) error {
	return evalConstraintsCore(mc, row, skipKey, nil, nil)
}

// ruleStillRelevant decides whether an UPDATE re-triggers an inverted guard (a
// rule). A rule that already held on the persisted row and whose columns the
// patch does not touch was approved (or grandfathered) before: a PUT that only
// renames the record must not ask for the same approval again. It fires when
// the row did NOT trigger before (the update moves it into the triggered
// state) or when a column the predicate reads changes.
func ruleStillRelevant(c manifest.ConstraintDef, before, input map[string]any) bool {
	env := make(map[string]float64, len(before))
	for k, v := range before {
		env[k] = computeexpr.ToFloat(v)
	}
	triggeredBefore, err := evalConstraintExpr(c.Expr, env)
	if err != nil || !triggeredBefore {
		return true
	}
	for _, id := range exprIdentRe.FindAllString(c.Expr, -1) {
		if raw, present := input[id]; present && !unchangedFromPersisted(raw, before, id) {
			return true
		}
	}
	return false
}

// evalConstraintsCore is evalConstraintsSkipping plus the update context:
// before/input are the persisted row and the patch (nil on create).
func evalConstraintsCore(mc *ModelConstraints, row map[string]any, skipKey string, before, input map[string]any) error {
	if !mc.active() {
		return nil
	}
	env := make(map[string]float64, len(row))
	for k, v := range row {
		env[k] = computeexpr.ToFloat(v)
	}
	var pending *ConstraintError
	for _, c := range mc.Constraints {
		if skipKey != "" && c.ErrorKey == skipKey && c.RequestsApproval() {
			continue
		}
		if c.Invert && before != nil && !ruleStillRelevant(c, before, input) {
			continue
		}
		ok, err := evalConstraintExpr(c.Expr, env)
		if err != nil {
			return fmt.Errorf("%w: constraint %q: %v", ErrInvalidInput, c.Expr, err)
		}
		if c.Invert {
			ok = !ok // a rule: the predicate triggers the guard when TRUE
		}
		if ok {
			continue
		}
		ce := &ConstraintError{ErrorKey: c.ErrorKey, Expr: c.Expr, Def: c, Values: exprValues(c.Expr, env)}
		if !c.RequestsApproval() {
			return ce
		}
		if pending == nil {
			pending = ce
		}
	}
	if pending != nil {
		return pending
	}
	return nil
}

// evalConstraintsCtx is the Create/Update entry point: it honours an approval
// replay marker on ctx (skipping the approved guard for the replayed model)
// and otherwise behaves like evalConstraints.
func evalConstraintsCtx(ctx context.Context, model string, mc *ModelConstraints, row map[string]any) error {
	return evalConstraintsUpdateCtx(ctx, model, mc, row, nil, nil)
}

// evalConstraintsUpdateCtx is evalConstraintsCtx for an UPDATE: before is the
// persisted row and input the patch, so rule guards can tell whether the write
// touches what they watch.
func evalConstraintsUpdateCtx(ctx context.Context, model string, mc *ModelConstraints, row, before, input map[string]any) error {
	skip := ""
	if r, ok := ApprovalReplayFromContext(ctx); ok && r.Kind == ApprovalKindConstraint && r.matchesModel(model) {
		skip = r.ConstraintKey
	}
	return evalConstraintsCore(mc, row, skip, before, input)
}

// comparison operators, longest-match first so ">=" is not mis-split as ">".
var constraintOps = []string{">=", "<=", "!=", "==", ">", "<", "="}

// evalConstraintExpr evaluates a single boolean guard `<arith> <op> <arith>`.
// Each side is evaluated with the strict Tier-2 arithmetic allowlist
// (computeexpr.Eval), then compared. This is the "arithmetic + comparison"
// allowlist the guards contract calls for: the comparison layer lives here and
// the arithmetic layer is the exact same evaluator the formula engine uses, so
// a guard can never inject SQL or call a function.
func evalConstraintExpr(expr string, env map[string]float64) (bool, error) {
	op, idx := findComparisonOp(expr)
	if op == "" {
		return false, fmt.Errorf("must contain a comparison operator (>= <= > < == !=)")
	}
	lhs := strings.TrimSpace(expr[:idx])
	rhs := strings.TrimSpace(expr[idx+len(op):])
	if lhs == "" || rhs == "" {
		return false, fmt.Errorf("comparison is missing an operand")
	}
	l, err := computeexpr.Eval(lhs, env)
	if err != nil {
		return false, fmt.Errorf("left side %q: %w", lhs, err)
	}
	r, err := computeexpr.Eval(rhs, env)
	if err != nil {
		return false, fmt.Errorf("right side %q: %w", rhs, err)
	}
	switch op {
	case ">=":
		return l >= r, nil
	case "<=":
		return l <= r, nil
	case ">":
		return l > r, nil
	case "<":
		return l < r, nil
	case "==", "=":
		return l == r, nil
	case "!=":
		return l != r, nil
	}
	return false, fmt.Errorf("unknown operator %q", op)
}

// findComparisonOp returns the first top-level comparison operator in expr and
// its byte index. Because both sides are pure arithmetic (no comparison bytes),
// the first comparison byte encountered is unambiguously THE operator; we scan
// left to right and match the longest operator at that position.
func findComparisonOp(expr string) (string, int) {
	for i := 0; i < len(expr); i++ {
		c := expr[i]
		if c != '>' && c != '<' && c != '=' && c != '!' {
			continue
		}
		for _, op := range constraintOps {
			if strings.HasPrefix(expr[i:], op) {
				return op, i
			}
		}
	}
	return "", -1
}

// BuildModelConstraints folds a model definition's guards into the
// ModelConstraints a host's ConstraintResolver returns: every column
// Constraint, the pre-write rule guards projected from contributions.rules[]
// (P5, then.kind request_approval), the cross-record rules and the locking
// strategy. Returns (nil, false) when the model declares none.
func BuildModelConstraints(md manifest.ModelDefinition) (*ModelConstraints, bool) {
	mc := &ModelConstraints{Locking: md.Locking, Rules: md.Rules}
	for _, c := range md.Columns {
		mc.Constraints = append(mc.Constraints, c.Constraints...)
	}
	mc.Constraints = append(mc.Constraints, md.RuleGuards...)
	if !mc.active() {
		return nil, false
	}
	return mc, true
}

// UnwiredRuleGuards lists the rule guards (contributions.rules[] with
// then.kind request_approval, ModelDefinition.RuleGuards) that the host's
// ConstraintResolver does NOT return for their model: a host that assembles
// ModelConstraints by hand instead of calling BuildModelConstraints silently
// loses them (the rule is declared but never enforced: fail-open). Each entry
// is "<model>: <errorKey>". Call it after installing/upgrading an addon; it
// logs one `rule_guards.unwired` line per finding.
func (s *Service) UnwiredRuleGuards(ctx context.Context, defs []manifest.ModelDefinition) []string {
	var out []string
	for _, md := range defs {
		if len(md.RuleGuards) == 0 {
			continue
		}
		have := map[string]struct{}{}
		for _, name := range []string{md.ModelKey, md.TableName} {
			if name == "" || s.constraints == nil {
				continue
			}
			if mc, ok := s.constraints(ctx, name); ok && mc != nil {
				for _, c := range mc.Constraints {
					have[c.ErrorKey] = struct{}{}
				}
			}
		}
		for _, g := range md.RuleGuards {
			if _, ok := have[g.ErrorKey]; !ok {
				out = append(out, md.ModelKey+": "+g.ErrorKey)
				log.Printf("dynamic: rule_guards.unwired model=%s rule=%s: the ConstraintResolver does not return it (use dynamic.BuildModelConstraints); the rule is NOT enforced", md.ModelKey, g.ErrorKey)
			}
		}
	}
	return out
}
