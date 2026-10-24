package wasm

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/google/uuid"

	"github.com/asteby/metacore-kernel/dynamic"
	"github.com/asteby/metacore-kernel/manifest"
	"github.com/asteby/metacore-kernel/security"
)

type fakeResolver struct {
	calls  int
	org    uuid.UUID
	addon  string
	branch string
	base   map[string]string
	out    map[string]string
	err    error
}

func (f *fakeResolver) ResolveSettings(_ context.Context, org uuid.UUID, addon, branch string, base map[string]string) (map[string]string, error) {
	f.calls++
	f.org, f.addon, f.branch, f.base = org, addon, branch, base
	return f.out, f.err
}

// Regression: a Host without a resolver serves the caller's map untouched.
func TestResolveSettings_NoResolverIsIdentity(t *testing.T) {
	h := &Host{}
	in := map[string]string{"a": "1"}
	got := h.resolveSettings(context.Background(), uuid.New(), "x", in)
	if !reflect.DeepEqual(got, in) {
		t.Fatalf("got %v, want %v", got, in)
	}
	if h.resolveSettings(context.Background(), uuid.New(), "x", nil) != nil {
		t.Fatal("nil map must stay nil")
	}
}

func TestResolveSettings_UsesBranchFromContext(t *testing.T) {
	org, branch := uuid.New(), uuid.New().String()
	f := &fakeResolver{out: map[string]string{"k": "branch"}}
	h := (&Host{}).WithSettingsResolver(f)
	ctx := dynamic.WithBranchID(context.Background(), branch)
	got := h.resolveSettings(ctx, org, "addon", map[string]string{"k": "legacy"})
	if got["k"] != "branch" || f.branch != branch || f.org != org || f.addon != "addon" || f.base["k"] != "legacy" {
		t.Fatalf("unexpected: got=%v call=%+v", got, f)
	}
}

func TestResolveSettings_SkipsWithoutTenant(t *testing.T) {
	f := &fakeResolver{out: map[string]string{"k": "x"}}
	h := (&Host{}).WithSettingsResolver(f)
	in := map[string]string{"k": "legacy"}
	if got := h.resolveSettings(context.Background(), uuid.Nil, "a", in); got["k"] != "legacy" || f.calls != 0 {
		t.Fatalf("resolver must not run without org: got=%v calls=%d", got, f.calls)
	}
}

func TestResolveSettings_ErrorFallsBackToLegacy(t *testing.T) {
	f := &fakeResolver{err: errors.New("boom")}
	h := (&Host{}).WithSettingsResolver(f)
	in := map[string]string{"k": "legacy"}
	if got := h.resolveSettings(context.Background(), uuid.New(), "a", in); got["k"] != "legacy" {
		t.Fatalf("got %v", got)
	}
}

// The hook is reached from the real invoke path (InvokeFor), and Invoke
// (no org) never consults it.
func TestInvoke_ConsultsResolverOnlyWithOrg(t *testing.T) {
	ctx := context.Background()
	h, err := NewHost(ctx, security.Compile("testaddon", nil), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close(ctx)
	spec := &manifest.BackendSpec{Runtime: "wasm", Entry: "backend.wasm", Exports: []string{"echo"}, MemoryLimitMB: 4, TimeoutMs: 2000}
	if err := h.Load(ctx, "testaddon", echoWasm(), spec); err != nil {
		t.Fatal(err)
	}
	f := &fakeResolver{out: map[string]string{}}
	h.WithSettingsResolver(f)
	inst := uuid.New()
	if _, err := h.Invoke(ctx, inst, "testaddon", "echo", []byte("x"), nil); err != nil {
		t.Fatal(err)
	}
	if f.calls != 0 {
		t.Fatalf("Invoke without org consulted the resolver (%d)", f.calls)
	}
	out, err := h.InvokeFor(ctx, uuid.New(), inst, "testaddon", "echo", []byte("y"), nil)
	if err != nil || string(out) != "y" {
		t.Fatalf("InvokeFor: %q %v", out, err)
	}
	if f.calls != 1 {
		t.Fatalf("InvokeFor must consult the resolver once, got %d", f.calls)
	}
}
