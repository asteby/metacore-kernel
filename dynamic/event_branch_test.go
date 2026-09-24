package dynamic

import (
	"context"
	"testing"

	"github.com/google/uuid"
)

// EventBranchID: the document's own branch (after, else before) beats the
// caller's active branch; non-uuid / zero values count as absent.
func TestEventBranchID(t *testing.T) {
	doc, prev, active := uuid.New(), uuid.New(), uuid.New()
	ctx := WithBranchID(context.Background(), active.String())

	cases := []struct {
		name          string
		ctx           context.Context
		before, after map[string]any
		want          string
	}{
		{"after string", ctx, nil, map[string]any{"branch_id": doc.String()}, doc.String()},
		{"after uuid", ctx, nil, map[string]any{"branch_id": doc}, doc.String()},
		{"after *uuid", ctx, nil, map[string]any{"branch_id": &doc}, doc.String()},
		{"delete keeps before", ctx, map[string]any{"branch_id": prev.String()}, nil, prev.String()},
		{"after beats before", ctx, map[string]any{"branch_id": prev.String()}, map[string]any{"branch_id": doc.String()}, doc.String()},
		{"blank row falls to ctx", ctx, nil, map[string]any{"branch_id": ""}, active.String()},
		{"zero uuid falls to ctx", ctx, nil, map[string]any{"branch_id": uuid.Nil}, active.String()},
		{"no column falls to ctx", ctx, nil, map[string]any{"name": "x"}, active.String()},
		{"nothing", context.Background(), nil, map[string]any{"branch_id": nil}, ""},
	}
	for _, tc := range cases {
		if got := EventBranchID(tc.ctx, tc.before, tc.after); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

// withUserBranch: the host ctx wins; else the user's GetBranchID; a
// branch-less user adds nothing.
func TestWithUserBranch(t *testing.T) {
	host, own := uuid.New(), uuid.New()
	u := branchUser{fakeUser: newUser(uuid.New()), branch: own}
	if got := BranchIDFromContext(withUserBranch(context.Background(), u)); got != own.String() {
		t.Fatalf("user branch = %q, want %s", got, own)
	}
	if got := BranchIDFromContext(withUserBranch(WithBranchID(context.Background(), host.String()), u)); got != host.String() {
		t.Fatalf("host ctx lost: %q", got)
	}
	if got := BranchIDFromContext(withUserBranch(context.Background(), newUser(uuid.New()))); got != "" {
		t.Fatalf("branch-less user stamped %q", got)
	}
}
