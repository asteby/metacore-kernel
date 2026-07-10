package dynamic

import (
	"context"
	"fmt"
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
}

// ConstraintResolver returns the guard configuration for a model name. Hosts
// wire it from their addon registry; the kernel owns no global model index.
// Returning (nil, false) — or an empty set — disables guards for that model,
// leaving Create/Update unrestricted (the legacy behaviour).
type ConstraintResolver func(ctx context.Context, model string) (*ModelConstraints, bool)

// active reports whether the config actually carries guard predicates. Locking
// alone (no constraints) is inert — there is nothing to serialize on.
func (mc *ModelConstraints) active() bool {
	return mc != nil && len(mc.Constraints) > 0
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

// evalConstraints evaluates every guard predicate against the row's numeric
// environment (missing / non-numeric identifiers resolve to 0, matching the
// Tier-2 formula engine). The FIRST false predicate aborts with a
// *ConstraintError carrying its ErrorKey; a malformed predicate is a *400*-class
// input error (invalid manifest reaching runtime) surfaced as a plain error.
func evalConstraints(mc *ModelConstraints, row map[string]any) error {
	if !mc.active() {
		return nil
	}
	env := make(map[string]float64, len(row))
	for k, v := range row {
		env[k] = computeexpr.ToFloat(v)
	}
	for _, c := range mc.Constraints {
		ok, err := evalConstraintExpr(c.Expr, env)
		if err != nil {
			return fmt.Errorf("%w: constraint %q: %v", ErrInvalidInput, c.Expr, err)
		}
		if !ok {
			return &ConstraintError{ErrorKey: c.ErrorKey, Expr: c.Expr}
		}
	}
	return nil
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
