// Package entityscope turns a dynamic.EntityScope into the predicates the WASM
// host splices into data_query / data_mutate (RFC 0001 — Entidad y alcance).
//
// It is deliberately free of database and wazero imports: everything here is
// pure string/set work, so the wall between two entities of one organization is
// unit-testable in milliseconds and reviewable in one screen. The host glue
// (runtime/wasm/entity_scope.go) only decides WHEN to call it.
//
// # Modes
//
//	off      nothing happens. The default; the host behaves exactly as before.
//	log      nothing is filtered, but every read that WOULD have been narrowed
//	         (and every write that WOULD have been refused) is logged by the
//	         caller. Run this for a while before enforcing, to see what breaks.
//	enforce  the wall is exact: reads are narrowed, cross-entity writes refused.
//
// # What the wall covers
//
// Only tables that carry a `branch_id` column: the entity of such a row is the
// entity of its branch (RFC §3.3). A table without `branch_id` is scoped by
// organization alone — that is a documented policy per table, not an accident;
// a model that needs entity isolation must carry the branch.
//
// A row whose branch_id is NULL belongs to the organization's default entity
// (RFC §3.3): it is visible only when the scope sets IncludeUnassigned.
package entityscope

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/google/uuid"

	"github.com/asteby/metacore-kernel/dynamic"
)

// Mode is how strictly the host applies the entity wall.
type Mode string

// Modes, from inert to exact.
const (
	Off     Mode = "off"
	Log     Mode = "log"
	Enforce Mode = "enforce"
)

// ParseMode reads a flag value. Anything unrecognised is Off: a typo must never
// switch the wall on (or off) by surprise, and Off is the legacy behaviour.
func ParseMode(s string) Mode {
	switch Mode(strings.ToLower(strings.TrimSpace(s))) {
	case Log:
		return Log
	case Enforce:
		return Enforce
	default:
		return Off
	}
}

// maxBranches bounds the IN list. A scope past it is refused (closed) rather
// than truncated silently into a different, smaller wall.
const maxBranches = 5000

// ErrOutOfScope is the write refusal: the row names a branch that belongs to
// another entity than the one the call acts for.
var ErrOutOfScope = errors.New("branch_id belongs to another entity than the one this call acts for")

const noRows = "1 = 0"

// Clause returns the WHERE fragment (and its positional args, numbered from
// `first`) that narrows a read to the scope, or "" when no wall applies:
// off/log mode, an unrestricted scope, or a table without `branch_id`.
//
// `cols` is the table's column set (lowercase names), the same probe the host
// already runs for tenant scoping.
func Clause(scope dynamic.EntityScope, mode Mode, cols map[string]bool, first int) (string, []any) {
	if mode != Enforce || !cols["branch_id"] {
		return "", nil
	}
	if !scope.Restricted && !scope.Unresolved {
		return "", nil
	}
	// A scope the embedder failed to compute must not fall open to the whole
	// organization: only the unassigned rows of the default entity stay visible.
	if scope.Unresolved {
		if scope.IncludeUnassigned {
			return "branch_id IS NULL", nil
		}
		return noRows, nil
	}
	ids := cleanIDs(scope.BranchIDs)
	if len(ids) == 0 || len(ids) > maxBranches {
		if scope.IncludeUnassigned && len(ids) == 0 {
			return "branch_id IS NULL", nil
		}
		return noRows, nil
	}
	ph := make([]string, len(ids))
	args := make([]any, len(ids))
	for i, id := range ids {
		ph[i] = fmt.Sprintf("$%d", first+i)
		args[i] = id
	}
	in := "branch_id IN (" + strings.Join(ph, ", ") + ")"
	if scope.IncludeUnassigned {
		return "(" + in + " OR branch_id IS NULL)", args
	}
	return in, args
}

// cleanIDs keeps well-formed uuids only, deduplicated and sorted, so the SQL is
// deterministic (query-plan cache friendly) and a malformed id can never reach
// a uuid cast. Dropping an id only ever shrinks the wall's window: safe.
func cleanIDs(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, raw := range in {
		u, err := uuid.Parse(strings.TrimSpace(raw))
		if err != nil {
			continue
		}
		s := u.String()
		if _, dup := seen[s]; dup {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// branchOf normalises a decoded row value into uuid text ("" for NULL/absent).
func branchOf(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return strings.TrimSpace(t)
	case []byte:
		return strings.TrimSpace(string(t))
	default:
		return strings.TrimSpace(fmt.Sprint(t))
	}
}

// OutOfScope counts the rows that fall outside the scope. It is the `log` mode
// probe: rows are read unfiltered and this says how many an enforced wall would
// have hidden. A row without a `branch_id` key (aggregates, projections) is not
// counted: there is nothing to judge it by.
func OutOfScope(rows []map[string]any, scope dynamic.EntityScope) int {
	if !scope.Restricted && !scope.Unresolved {
		return 0
	}
	n := 0
	for _, r := range rows {
		v, has := r["branch_id"]
		if !has {
			continue
		}
		if !scope.Allows(branchOf(v)) {
			n++
		}
	}
	return n
}

// WriteViolates reports whether a create/update payload names a branch outside
// the scope. A payload that does not name a branch is fine here: the host
// stamps the caller's own active branch on creates, which the scope already
// contains.
func WriteViolates(scope dynamic.EntityScope, data map[string]any) bool {
	if !scope.Restricted && !scope.Unresolved {
		return false
	}
	v, has := data["branch_id"]
	if !has {
		return false
	}
	return !scope.Allows(branchOf(v))
}

// CheckWrite is WriteViolates as the enforce-mode refusal: nil in off/log mode
// (the caller logs the would-be violation), ErrOutOfScope in enforce.
func CheckWrite(scope dynamic.EntityScope, mode Mode, data map[string]any) error {
	if mode == Enforce && WriteViolates(scope, data) {
		return ErrOutOfScope
	}
	return nil
}
