package entityscope

import (
	"fmt"
	"strings"
	"testing"

	"github.com/asteby/metacore-kernel/dynamic"
)

const (
	b1 = "11111111-1111-1111-1111-111111111111"
	b2 = "22222222-2222-2222-2222-222222222222"
	bX = "99999999-9999-9999-9999-999999999999" // a branch of ANOTHER entity
)

var withBranch = map[string]bool{"id": true, "organization_id": true, "branch_id": true}

func scopeA() dynamic.EntityScope {
	return dynamic.EntityScope{EntityID: "A", BranchIDs: []string{b2, b1, b1}, Restricted: true}
}

func TestParseMode(t *testing.T) {
	for in, want := range map[string]Mode{"": Off, "off": Off, "LOG": Log, " enforce ": Enforce, "enforced": Off, "x": Off} {
		if got := ParseMode(in); got != want {
			t.Errorf("ParseMode(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestClause_EnforceNarrowsToTheEntitysBranches(t *testing.T) {
	frag, args := Clause(scopeA(), Enforce, withBranch, 2)
	if frag != "branch_id IN ($2, $3)" {
		t.Fatalf("frag = %q", frag)
	}
	if len(args) != 2 || args[0] != b1 || args[1] != b2 {
		t.Fatalf("args = %v (want sorted, deduplicated)", args)
	}
}

func TestClause_IncludeUnassignedAdmitsNullBranch(t *testing.T) {
	s := scopeA()
	s.IncludeUnassigned = true
	frag, _ := Clause(s, Enforce, withBranch, 2)
	if frag != "(branch_id IN ($2, $3) OR branch_id IS NULL)" {
		t.Fatalf("frag = %q", frag)
	}
}

func TestClause_NoWallWhenNothingToEnforce(t *testing.T) {
	cases := map[string]struct {
		scope dynamic.EntityScope
		mode  Mode
		cols  map[string]bool
	}{
		"off mode":             {scopeA(), Off, withBranch},
		"log mode only logs":   {scopeA(), Log, withBranch},
		"unrestricted scope":   {dynamic.EntityScope{}, Enforce, withBranch},
		"table without branch": {scopeA(), Enforce, map[string]bool{"id": true, "organization_id": true}},
	}
	for name, c := range cases {
		if frag, args := Clause(c.scope, c.mode, c.cols, 2); frag != "" || args != nil {
			t.Errorf("%s: got %q %v, want no clause", name, frag, args)
		}
	}
}

func TestClause_ClosedWhenTheEntityHasNoBranches(t *testing.T) {
	empty := dynamic.EntityScope{EntityID: "A", Restricted: true}
	if frag, _ := Clause(empty, Enforce, withBranch, 2); frag != noRows {
		t.Fatalf("empty entity must see nothing, got %q", frag)
	}
	empty.IncludeUnassigned = true
	if frag, _ := Clause(empty, Enforce, withBranch, 2); frag != "branch_id IS NULL" {
		t.Fatalf("default entity with no branches sees only unassigned rows, got %q", frag)
	}
}

func TestClause_DropsMalformedIDsNeverWidens(t *testing.T) {
	s := dynamic.EntityScope{Restricted: true, BranchIDs: []string{"not-a-uuid", "x'; DROP TABLE t;--", b1}}
	frag, args := Clause(s, Enforce, withBranch, 5)
	if frag != "branch_id IN ($5)" || len(args) != 1 || args[0] != b1 {
		t.Fatalf("got %q %v", frag, args)
	}
	if strings.Contains(frag, "DROP") {
		t.Fatal("guest-controlled text must never reach the SQL")
	}
	junk := dynamic.EntityScope{Restricted: true, BranchIDs: []string{"junk"}}
	if frag, _ := Clause(junk, Enforce, withBranch, 2); frag != noRows {
		t.Fatalf("only junk ids must close the wall, got %q", frag)
	}
}

func TestClause_UnresolvedScopeFailsClosed(t *testing.T) {
	s := dynamic.EntityScope{Unresolved: true}
	if frag, _ := Clause(s, Enforce, withBranch, 2); frag != noRows {
		t.Fatalf("unresolved must not fall open, got %q", frag)
	}
	s.IncludeUnassigned = true
	if frag, _ := Clause(s, Enforce, withBranch, 2); frag != "branch_id IS NULL" {
		t.Fatalf("got %q", frag)
	}
	if frag, _ := Clause(s, Log, withBranch, 2); frag != "" {
		t.Fatal("log mode never filters")
	}
}

func TestClause_TooManyBranchesClosesInsteadOfTruncating(t *testing.T) {
	ids := make([]string, 0, maxBranches+1)
	for i := 0; i <= maxBranches; i++ {
		ids = append(ids, fmt.Sprintf("00000000-0000-4000-8000-%012d", i))
	}
	if frag, _ := Clause(dynamic.EntityScope{Restricted: true, BranchIDs: ids}, Enforce, withBranch, 2); frag != noRows {
		t.Fatalf("got %q", frag)
	}
}

func TestOutOfScope_CountsOnlyRowsOfOtherEntities(t *testing.T) {
	rows := []map[string]any{
		{"branch_id": b1}, {"branch_id": b2}, {"branch_id": bX}, {"branch_id": bX}, {"branch_id": nil}, {"x": 1},
	}
	// bX, bX and the NULL one (A is not the default entity); the row without a
	// branch_id key is a projection, not judged.
	if got := OutOfScope(rows, scopeA()); got != 3 {
		t.Fatalf("got %d, want 3", got)
	}
	s := scopeA()
	s.IncludeUnassigned = true
	if got := OutOfScope(rows, s); got != 2 {
		t.Fatalf("default entity keeps the unassigned row: got %d, want 2", got)
	}
	if got := OutOfScope(rows, dynamic.EntityScope{}); got != 0 {
		t.Fatalf("unrestricted scope sees everything: got %d", got)
	}
}

func TestCheckWrite(t *testing.T) {
	cases := []struct {
		name string
		mode Mode
		data map[string]any
		want error
	}{
		{"enforce: own branch", Enforce, map[string]any{"branch_id": b1}, nil},
		{"enforce: other entity's branch", Enforce, map[string]any{"branch_id": bX}, ErrOutOfScope},
		{"enforce: no branch named (host stamps the caller's)", Enforce, map[string]any{"name": "x"}, nil},
		{"enforce: explicit NULL into a non-default entity", Enforce, map[string]any{"branch_id": nil}, ErrOutOfScope},
		{"log: would violate but never refuses", Log, map[string]any{"branch_id": bX}, nil},
		{"off: inert", Off, map[string]any{"branch_id": bX}, nil},
	}
	for _, c := range cases {
		if got := CheckWrite(scopeA(), c.mode, c.data); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
	if !WriteViolates(scopeA(), map[string]any{"branch_id": bX}) {
		t.Error("WriteViolates must still report in log mode so the caller can log it")
	}
}
