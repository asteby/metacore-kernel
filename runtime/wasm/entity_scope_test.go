package wasm

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/asteby/metacore-kernel/dynamic"
	"github.com/asteby/metacore-kernel/runtime/entityscope"
)

const (
	testBranchA = "11111111-1111-1111-1111-111111111111"
	testBranchB = "99999999-9999-9999-9999-999999999999" // a branch of another entity
)

// fakeEntityResolver records what the host asked for and answers from a table.
type fakeEntityResolver struct {
	calls   int
	gotOrg  uuid.UUID
	gotEnt  string
	gotBr   string
	scope   dynamic.EntityScope
	failure error
}

func (f *fakeEntityResolver) ResolveEntityScope(_ context.Context, org uuid.UUID, entityID, branchID string) (dynamic.EntityScope, error) {
	f.calls++
	f.gotOrg, f.gotEnt, f.gotBr = org, entityID, branchID
	return f.scope, f.failure
}

func scopeOfA() dynamic.EntityScope {
	return dynamic.EntityScope{EntityID: "A", BranchIDs: []string{testBranchA}, Restricted: true}
}

func TestWithEntityScope_OffAndMissingPiecesAreInert(t *testing.T) {
	org := uuid.New()
	ctx := dynamic.WithBranchID(context.Background(), testBranchA)

	r := &fakeEntityResolver{scope: scopeOfA()}
	h := (&Host{}).WithEntityScope(r, entityscope.Off)
	if _, ok := dynamic.EntityScopeFromContext(h.withEntityScope(ctx, org)); ok || r.calls != 0 {
		t.Fatalf("mode off must not resolve anything (calls=%d)", r.calls)
	}

	h = (&Host{}).WithEntityScope(nil, entityscope.Enforce)
	if _, ok := dynamic.EntityScopeFromContext(h.withEntityScope(ctx, org)); ok {
		t.Fatal("no resolver: nothing to enforce with")
	}

	h = (&Host{}).WithEntityScope(r, entityscope.Enforce)
	if _, ok := dynamic.EntityScopeFromContext(h.withEntityScope(ctx, uuid.Nil)); ok || r.calls != 0 {
		t.Fatalf("legacy callers without a tenant are untouched (calls=%d)", r.calls)
	}
}

func TestWithEntityScope_NoEntityNoBranchMeansWholeOrganization(t *testing.T) {
	r := &fakeEntityResolver{scope: scopeOfA()}
	h := (&Host{}).WithEntityScope(r, entityscope.Enforce)
	if _, ok := dynamic.EntityScopeFromContext(h.withEntityScope(context.Background(), uuid.New())); ok || r.calls != 0 {
		t.Fatal("nothing to derive from: the call keeps seeing the whole organization, exactly as before")
	}
}

func TestWithEntityScope_DerivesFromBranchAndEntity(t *testing.T) {
	org := uuid.New()
	r := &fakeEntityResolver{scope: scopeOfA()}
	h := (&Host{}).WithEntityScope(r, entityscope.Enforce)

	ctx := dynamic.WithEntityID(dynamic.WithBranchID(context.Background(), testBranchA), "A")
	got, ok := dynamic.EntityScopeFromContext(h.withEntityScope(ctx, org))
	if !ok || !got.Restricted || got.EntityID != "A" {
		t.Fatalf("scope not stamped: %+v %v", got, ok)
	}
	if r.gotOrg != org || r.gotEnt != "A" || r.gotBr != testBranchA {
		t.Fatalf("resolver asked for the wrong thing: org=%v ent=%q branch=%q", r.gotOrg, r.gotEnt, r.gotBr)
	}

	// A branch alone is enough: the resolver derives the entity from it. This is
	// the path an event replay takes (dispatch sets the event's branch on ctx).
	r2 := &fakeEntityResolver{scope: scopeOfA()}
	h2 := (&Host{}).WithEntityScope(r2, entityscope.Log)
	if _, ok := dynamic.EntityScopeFromContext(h2.withEntityScope(dynamic.WithBranchID(context.Background(), testBranchA), org)); !ok || r2.gotEnt != "" || r2.gotBr != testBranchA {
		t.Fatalf("branch-only call must reach the resolver with an empty entity (ent=%q br=%q)", r2.gotEnt, r2.gotBr)
	}
}

func TestWithEntityScope_ScopeAlreadyOnTheContextWins(t *testing.T) {
	r := &fakeEntityResolver{scope: scopeOfA()}
	h := (&Host{}).WithEntityScope(r, entityscope.Enforce)
	upstream := dynamic.EntityScope{EntityID: "UP", Restricted: true, BranchIDs: []string{testBranchB}}
	ctx := dynamic.WithEntityScope(dynamic.WithBranchID(context.Background(), testBranchA), upstream)
	got, _ := dynamic.EntityScopeFromContext(h.withEntityScope(ctx, uuid.New()))
	if got.EntityID != "UP" || r.calls != 0 {
		t.Fatalf("an embedder that resolved upstream must win: %+v calls=%d", got, r.calls)
	}
}

func TestWithEntityScope_ResolverFailureNeverWidensTheWall(t *testing.T) {
	r := &fakeEntityResolver{failure: errors.New("locations down")}
	h := (&Host{}).WithEntityScope(r, entityscope.Enforce)
	ctx := dynamic.WithEntityID(dynamic.WithBranchID(context.Background(), testBranchA), "A")
	got, ok := dynamic.EntityScopeFromContext(h.withEntityScope(ctx, uuid.New()))
	if !ok || !got.Unresolved || got.EntityID != "A" {
		t.Fatalf("a failed resolution must leave an Unresolved scope, not no scope: %+v %v", got, ok)
	}
	// Unresolved + enforce = a branch-bearing read sees nothing, never everything.
	cols := map[string]bool{"branch_id": true}
	if frag, _ := entityscope.Clause(got, entityscope.Enforce, cols, 2); frag == "" {
		t.Fatal("an unresolved scope must still close the wall in enforce mode")
	}
}

func TestCheckEntityWrite(t *testing.T) {
	ctxWith := func() context.Context { return dynamic.WithEntityScope(context.Background(), scopeOfA()) }
	own := map[string]any{"branch_id": testBranchA}
	foreign := map[string]any{"branch_id": testBranchB}
	org := uuid.New()

	enforce := &invocation{entityMode: entityscope.Enforce}
	if code, err := checkEntityWrite(ctxWith(), enforce, "sales", "orders", org, own); err != nil || code != "" {
		t.Fatalf("own branch must pass: %q %v", code, err)
	}
	if code, err := checkEntityWrite(ctxWith(), enforce, "sales", "orders", org, foreign); !errors.Is(err, entityscope.ErrOutOfScope) || code != "forbidden" {
		t.Fatalf("another entity's branch must be forbidden: %q %v", code, err)
	}

	logging := &invocation{entityMode: entityscope.Log}
	if code, err := checkEntityWrite(ctxWith(), logging, "sales", "orders", org, foreign); err != nil || code != "" {
		t.Fatalf("log mode observes but never refuses: %q %v", code, err)
	}

	off := &invocation{entityMode: entityscope.Off}
	if _, err := checkEntityWrite(ctxWith(), off, "sales", "orders", org, foreign); err != nil {
		t.Fatalf("off is inert: %v", err)
	}

	// No scope on the context (a call that carried neither entity nor branch):
	// the wall is not engaged even in enforce mode.
	if _, err := checkEntityWrite(context.Background(), enforce, "sales", "orders", org, foreign); err != nil {
		t.Fatalf("no scope = whole organization, as before: %v", err)
	}
	var nilInv *invocation
	if _, err := checkEntityWrite(ctxWith(), nilInv, "sales", "orders", org, foreign); err != nil {
		t.Fatalf("a nil invocation is inert: %v", err)
	}
}
