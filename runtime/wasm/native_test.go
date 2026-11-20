package wasm

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/asteby/metacore-kernel/events"
	"github.com/asteby/metacore-kernel/security"
	"github.com/google/uuid"
)

func newNativeHost(t *testing.T) *Host {
	t.Helper()
	ctx := context.Background()
	h, err := NewHost(ctx, security.Compile("modx", nil), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.Close(ctx) })
	return h
}

// A native export is dispatched through every Invoke* entry point with the
// payload in and the result out, like a wasm export.
func TestNative_InvokeAllEntryPoints(t *testing.T) {
	h := newNativeHost(t)
	err := h.LoadNative("modx", map[string]NativeFunc{
		"echo": func(_ context.Context, c *NativeCall, in []byte) ([]byte, error) {
			v, _ := c.Setting("k")
			return []byte(`{"in":` + string(in) + `,"org":"` + c.OrgID().String() + `","k":"` + v + `","addon":"` + c.AddonKey() + `"}`), nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	org, inst := uuid.New(), uuid.New()
	settings := map[string]string{"k": "v"}
	for name, call := range map[string]func() ([]byte, error){
		"Invoke":        func() ([]byte, error) { return h.Invoke(ctx, inst, "modx", "echo", []byte(`1`), settings) },
		"InvokeFor":     func() ([]byte, error) { return h.InvokeFor(ctx, org, inst, "modx", "echo", []byte(`1`), settings) },
		"InvokeInTx":    func() ([]byte, error) { return h.InvokeInTx(ctx, nil, inst, "modx", "echo", []byte(`1`), settings) },
		"InvokeForInTx": func() ([]byte, error) { return h.InvokeForInTx(ctx, nil, org, inst, "modx", "echo", []byte(`1`), settings) },
	} {
		out, err := call()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		var got struct {
			In    int    `json:"in"`
			Org   string `json:"org"`
			K     string `json:"k"`
			Addon string `json:"addon"`
		}
		if err := json.Unmarshal(out, &got); err != nil {
			t.Fatalf("%s: %v (%s)", name, err, out)
		}
		if got.In != 1 || got.K != "v" || got.Addon != "modx" {
			t.Fatalf("%s: unexpected %+v", name, got)
		}
		if name == "InvokeFor" || name == "InvokeForInTx" {
			if got.Org != org.String() {
				t.Fatalf("%s: org not bound: %s", name, got.Org)
			}
		}
	}
	if !h.IsCompiled("modx") || !h.HasNative("modx", "echo") || h.HasNative("modx", "nope") {
		t.Fatal("IsCompiled/HasNative disagree with registration")
	}
	if ks := h.CompiledKeys(); len(ks) != 1 || ks[0] != "modx" {
		t.Fatalf("CompiledKeys = %v", ks)
	}
	if ex := h.NativeExports("modx"); len(ex) != 1 || ex[0] != "echo" {
		t.Fatalf("NativeExports = %v", ex)
	}
}

// An export the module does not implement keeps the host's existing "missing
// export" semantics, so callers that map it to UI-only/strict errors behave
// the same for native and wasm.
func TestNative_MissingExportIsNotLoaded(t *testing.T) {
	h := newNativeHost(t)
	_ = h.LoadNative("modx", map[string]NativeFunc{"a": func(context.Context, *NativeCall, []byte) ([]byte, error) { return nil, nil }})
	_, err := h.Invoke(context.Background(), uuid.New(), "modx", "b", nil, nil)
	var nl *AddonNotLoadedError
	if !errors.As(err, &nl) {
		t.Fatalf("want AddonNotLoadedError, got %v", err)
	}
	if !strings.Contains(err.Error(), "not loaded") {
		t.Fatalf("message must keep the 'not loaded' marker callers match on: %v", err)
	}
}

func TestNative_PanicIsAFaultNotACrash(t *testing.T) {
	h := newNativeHost(t)
	_ = h.LoadNative("modx", map[string]NativeFunc{"boom": func(context.Context, *NativeCall, []byte) ([]byte, error) { panic("kaput") }})
	_, err := h.Invoke(context.Background(), uuid.New(), "modx", "boom", nil, nil)
	if err == nil || !strings.Contains(err.Error(), "panicked") {
		t.Fatalf("want a panic fault, got %v", err)
	}
}

func TestNative_LoadValidation(t *testing.T) {
	h := newNativeHost(t)
	if err := h.LoadNative("", nil); err == nil {
		t.Fatal("empty key must fail")
	}
	if err := h.LoadNative("modx", map[string]NativeFunc{"x": nil}); err == nil {
		t.Fatal("nil func must fail")
	}
	_ = h.LoadNative("modx", map[string]NativeFunc{"x": func(context.Context, *NativeCall, []byte) ([]byte, error) { return nil, nil }})
	h.UnloadNative("modx")
	if h.IsCompiled("modx") {
		t.Fatal("UnloadNative must drop the registration")
	}
}

// The host imports a native handler calls are the SAME functions the wasm
// closures call: with no bus wired event_emit answers the same typed envelope.
func TestNative_EmitEventSharesTheGuestPath(t *testing.T) {
	h := newNativeHost(t)
	_ = h.LoadNative("modx", map[string]NativeFunc{"emit": func(_ context.Context, c *NativeCall, _ []byte) ([]byte, error) {
		return c.EmitEvent("ev.fired", []byte(`{"a":1}`)), nil
	}})
	out, err := h.InvokeFor(context.Background(), uuid.New(), uuid.New(), "modx", "emit", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "bus_unavailable") {
		t.Fatalf("want bus_unavailable envelope, got %s", out)
	}
}

// Emit-on-commit applies to native handlers too: events staged by a handler
// that then fails are dropped; those of a handler that succeeds are published.
func TestNative_EmitIsStagedUntilSuccess(t *testing.T) {
	h := newNativeHost(t)
	bus := events.NewBus(nil)
	fired := 0
	_ = bus.SubscribeEvent("kernel", "ev.fired", func(context.Context, uuid.UUID, string, any) error { fired++; return nil })
	h.WithBus(bus)
	emit := func(fail bool) NativeFunc {
		return func(_ context.Context, c *NativeCall, _ []byte) ([]byte, error) {
			env := c.EmitEvent("ev.fired", []byte(aggPayload))
			if !strings.Contains(string(env), `"success":true`) {
				t.Errorf("emit not accepted: %s", env)
			}
			if fail {
				return nil, errors.New("handler failed")
			}
			return []byte(`{"success":true}`), nil
		}
	}
	_ = h.LoadNative("modx", map[string]NativeFunc{"bad": emit(true), "good": emit(false)})
	org := uuid.New()
	if _, err := h.InvokeFor(context.Background(), org, uuid.New(), "modx", "bad", nil, nil); err == nil {
		t.Fatal("want handler error")
	}
	if fired != 0 {
		t.Fatalf("event of a failed handler was published (%d)", fired)
	}
	if _, err := h.InvokeFor(context.Background(), org, uuid.New(), "modx", "good", nil, nil); err != nil {
		t.Fatal(err)
	}
	if fired != 1 {
		t.Fatalf("event of a successful handler not published (%d)", fired)
	}
}

// Tenant scope never comes from the payload: data_query without an org bound
// to the invocation is refused by the shared implementation.
func TestNative_DataImportsEnforceSharedGuards(t *testing.T) {
	h := newNativeHost(t)
	_ = h.LoadNative("modx", map[string]NativeFunc{"q": func(_ context.Context, c *NativeCall, _ []byte) ([]byte, error) {
		return c.DataQuery([]byte(`{"table":"products","organization_id":"` + uuid.NewString() + `"}`)), nil
	}})
	out, err := h.Invoke(context.Background(), uuid.New(), "modx", "q", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	var env struct {
		Success bool `json:"success"`
	}
	if err := json.Unmarshal(out, &env); err != nil || env.Success {
		t.Fatalf("data_query without org/capability must not succeed: %s", out)
	}
}
