package dynamic

import (
	"context"
	"testing"
)

func TestEntityScope_Allows(t *testing.T) {
	const a, b = "11111111-1111-1111-1111-111111111111", "22222222-2222-2222-2222-222222222222"
	cases := []struct {
		name   string
		scope  EntityScope
		branch string
		want   bool
	}{
		{"zero value is the legacy org-wide view", EntityScope{}, b, true},
		{"restricted: own branch", EntityScope{Restricted: true, BranchIDs: []string{a}}, a, true},
		{"restricted: another entity's branch", EntityScope{Restricted: true, BranchIDs: []string{a}}, b, false},
		{"restricted: no branch, not the default entity", EntityScope{Restricted: true, BranchIDs: []string{a}}, "", false},
		{"restricted: no branch, default entity", EntityScope{Restricted: true, BranchIDs: []string{a}, IncludeUnassigned: true}, "", true},
		{"unresolved never sees a branch", EntityScope{Unresolved: true, IncludeUnassigned: true}, a, false},
		{"unresolved, default entity: unassigned only", EntityScope{Unresolved: true, IncludeUnassigned: true}, "", true},
	}
	for _, c := range cases {
		if got := c.scope.Allows(c.branch); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

func TestEntityContextCarriers(t *testing.T) {
	ctx := context.Background()
	if EntityIDFromContext(ctx) != "" {
		t.Fatal("empty by default")
	}
	if _, ok := EntityScopeFromContext(ctx); ok {
		t.Fatal("no scope by default: callers must read it as 'no wall'")
	}
	if WithEntityID(ctx, "") != ctx {
		t.Fatal("an empty id returns ctx unchanged")
	}
	ctx = WithEntityScope(WithEntityID(ctx, "E1"), EntityScope{EntityID: "E1", Restricted: true})
	if EntityIDFromContext(ctx) != "E1" {
		t.Fatal("entity id lost")
	}
	if s, ok := EntityScopeFromContext(ctx); !ok || s.EntityID != "E1" || !s.Restricted {
		t.Fatalf("scope lost: %+v %v", s, ok)
	}
}
