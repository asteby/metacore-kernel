package compiled

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/asteby/metacore-kernel/dynamic"
	"github.com/asteby/metacore-kernel/lifecycle"
	"github.com/asteby/metacore-kernel/manifest"
	"github.com/asteby/metacore-kernel/runtime/apiroute"
)

type fakeInvoker struct {
	calls   int
	addon   string
	fn      string
	org     uuid.UUID
	payload []byte
	out     string
	err     error
}

func (f *fakeInvoker) InvokeForInTx(_ context.Context, _ *gorm.DB, org, _ uuid.UUID, addon, fn string, payload []byte, _ map[string]string) ([]byte, error) {
	f.calls++
	f.addon, f.fn, f.org, f.payload = addon, fn, org, payload
	return []byte(f.out), f.err
}

func TestTarget(t *testing.T) {
	for in, want := range map[string]string{"compiled:Do": "Do", "wasm:do_it": "do_it", "Bare": "Bare"} {
		got, err := Target(in)
		if err != nil || got != want {
			t.Fatalf("Target(%q) = %q, %v", in, got, err)
		}
	}
	for _, bad := range []string{"", "compiled:", "webhook:x", "compiled:1abc", "compiled:a-b", "compiled:a b"} {
		if _, err := Target(bad); !errors.Is(err, ErrInvalidHandler) {
			t.Fatalf("Target(%q) must be ErrInvalidHandler, got %v", bad, err)
		}
	}
}

func TestDecode(t *testing.T) {
	r, err := Decode(nil)
	if err != nil || !r.Success {
		t.Fatalf("empty output is a success: %+v %v", r, err)
	}
	r, _ = Decode([]byte(`{"success":false,"error":{"code":"x","message":"no"}}`))
	if r.Success || r.Error == nil || r.Error.Code != "x" {
		t.Fatalf("decline not decoded: %+v", r)
	}
	r, _ = Decode([]byte(`{"foo":1}`))
	if !r.Success || string(r.Data) != `{"foo":1}` {
		t.Fatalf("no success key => whole doc is data: %+v", r)
	}
	r, _ = Decode([]byte(`{"success":true,"data":{"a":1},"meta":{"m":2},"message":"ok"}`))
	if !r.Success || r.Meta["m"] != float64(2) || r.Message != "ok" {
		t.Fatalf("%+v", r)
	}
	if _, err := Decode([]byte(`not json`)); !errors.Is(err, ErrBadResult) {
		t.Fatalf("want ErrBadResult, got %v", err)
	}
	if _, err := Decode([]byte(`{"success":"yes"}`)); !errors.Is(err, ErrBadResult) {
		t.Fatalf("non-bool success must be ErrBadResult, got %v", err)
	}
}

func TestFailClosed_NoExecutor(t *testing.T) {
	var nilD *Dispatcher
	owner := func(context.Context, string) string { return "a" }
	for name, d := range map[string]*Dispatcher{"nil": nilD, "zero": {}, "noinv": New(nil, nil)} {
		if d.Ready() {
			t.Fatalf("%s must not be ready", name)
		}
		if _, err := d.CallRaw(context.Background(), nil, uuid.New(), "a", "Fn", nil); !errors.Is(err, ErrNoExecutor) {
			t.Fatalf("%s CallRaw: %v", name, err)
		}
		if _, err := d.Actions(owner).Dispatch(context.Background(), dynamic.ActionRequest{Model: "m", ActionKey: "Fn"}); !errors.Is(err, ErrNoExecutor) {
			t.Fatalf("%s Actions: %v", name, err)
		}
		if err := d.Schedule(Fixed("a")).Dispatch(context.Background(), uuid.New(), "Fn", nil); !errors.Is(err, ErrNoExecutor) {
			t.Fatalf("%s Schedule: %v", name, err)
		}
		if _, err := d.APIRoute().Invoke(context.Background(), uuid.New(), "Fn", apiroute.Request{Addon: "a"}); !errors.Is(err, ErrNoExecutor) {
			t.Fatalf("%s APIRoute: %v", name, err)
		}
		if _, err := d.Hook().Dispatch(context.Background(), lifecycle.HookRequest{AddonKey: "a", Target: manifest.HookTarget{Type: "compiled", Function: "Fn"}}); !errors.Is(err, ErrNoExecutor) {
			t.Fatalf("%s Hook: %v", name, err)
		}
		if _, err := d.Formula(owner, nil)(context.Background(), "m", "compiled:Fn", nil); !errors.Is(err, ErrNoExecutor) {
			t.Fatalf("%s Formula: %v", name, err)
		}
	}
}

func TestNotInstalledNeverRuns(t *testing.T) {
	inv := &fakeInvoker{out: `{"success":true}`}
	d := New(inv, func(context.Context, uuid.UUID, string) (Installation, error) { return Installation{}, ErrNotInstalled })
	if _, err := d.CallRaw(context.Background(), nil, uuid.New(), "a", "Fn", nil); !errors.Is(err, ErrNotInstalled) {
		t.Fatalf("got %v", err)
	}
	if inv.calls != 0 {
		t.Fatal("handler of a non-installed addon must not run")
	}
}

func TestUnresolvedOwnerIsAnError(t *testing.T) {
	inv := &fakeInvoker{out: `{"success":true}`}
	d := New(inv, nil)
	if _, err := d.Actions(func(context.Context, string) string { return "" }).Dispatch(context.Background(), dynamic.ActionRequest{Model: "m", ActionKey: "Fn"}); !errors.Is(err, ErrNoOwner) {
		t.Fatalf("got %v", err)
	}
	if err := d.Schedule(Fixed("")).Dispatch(context.Background(), uuid.New(), "Fn", nil); !errors.Is(err, ErrNoOwner) {
		t.Fatalf("got %v", err)
	}
	if inv.calls != 0 {
		t.Fatal("no call without an owner")
	}
}

type user struct{ id, org uuid.UUID }

func (u user) GetID() uuid.UUID             { return u.id }
func (u user) GetOrganizationID() uuid.UUID { return u.org }
func (u user) GetEmail() string             { return "" }
func (u user) GetRole() string              { return "" }
func (u user) GetPasswordHash() string      { return "" }
func (u user) SetEmail(string)              {}
func (u user) SetName(string)               {}
func (u user) SetPasswordHash(string)       {}
func (u user) SetRole(string)               {}
func (u user) SetOrganizationID(uuid.UUID)  {}

func TestActions_EnvelopeAndDecline(t *testing.T) {
	inv := &fakeInvoker{out: `{"success":true,"data":{"ok":1},"meta":{"k":"v"}}`}
	d := New(inv, nil)
	u := user{uuid.New(), uuid.New()}
	disp := d.Actions(func(_ context.Context, m string) string { return "mod_" + m })
	resp, err := disp.Dispatch(context.Background(), dynamic.ActionRequest{
		Model: "orders", ActionKey: "pay", Row: map[string]any{"id": "r1"}, Payload: map[string]any{"x": 1}, User: u,
		Trigger: &manifest.ActionTrigger{Type: "wasm", Export: "OnPay"},
	})
	if err != nil || !resp.Success || resp.Meta["k"] != "v" {
		t.Fatalf("%+v %v", resp, err)
	}
	if inv.addon != "mod_orders" || inv.fn != "OnPay" || inv.org != u.org {
		t.Fatalf("routed wrong: %+v", inv)
	}
	var env map[string]any
	_ = json.Unmarshal(inv.payload, &env)
	if env["id"] != "r1" || env["user_id"] != u.id.String() || env["org_id"] != u.org.String() {
		t.Fatalf("envelope: %v", env)
	}
	// Stage hooks carry the symbol in ActionKey.
	if _, err := disp.Dispatch(context.Background(), dynamic.ActionRequest{Model: "orders", ActionKey: "OnHook", User: u, Trigger: &manifest.ActionTrigger{Type: "compiled"}}); err != nil || inv.fn != "OnHook" {
		t.Fatalf("hook symbol: %v %q", err, inv.fn)
	}
	inv.out = `{"success":false,"error":{"code":"nope","message":"declined"}}`
	resp, err = disp.Dispatch(context.Background(), dynamic.ActionRequest{Model: "orders", ActionKey: "pay", User: u})
	if err != nil || resp.Success || resp.Error == nil || resp.Error.Code != "nope" {
		t.Fatalf("decline must be carried, not an error: %+v %v", resp, err)
	}
}

func TestScheduleAndHooks_DeclineIsError(t *testing.T) {
	inv := &fakeInvoker{out: `{"success":false,"error":{"code":"c","message":"m"}}`}
	d := New(inv, nil)
	if err := d.Schedule(Fixed("a")).Dispatch(context.Background(), uuid.New(), "Fn", map[string]any{"a": 1}); !errors.Is(err, ErrDeclined) {
		t.Fatalf("schedule decline: %v", err)
	}
	_, err := d.Hook().Dispatch(context.Background(), lifecycle.HookRequest{AddonKey: "a", OrgID: uuid.New(), Target: manifest.HookTarget{Type: "compiled", Function: "Fn"}})
	if !errors.Is(err, ErrDeclined) {
		t.Fatalf("hook decline must veto: %v", err)
	}
	inv.out = `{"success":true,"data":{"v":1}}`
	out, err := d.Schedule(Fixed("a")).DispatchResult(context.Background(), uuid.New(), "Fn", nil)
	if err != nil || !strings.Contains(string(out), `"v":1`) {
		t.Fatalf("%s %v", out, err)
	}
}

func TestAPIRoute(t *testing.T) {
	inv := &fakeInvoker{out: `{"status":201,"body":{"a":1}}`}
	d := New(inv, nil)
	resp, err := d.APIRoute().Invoke(context.Background(), uuid.New(), "Fn", apiroute.Request{Addon: "shop", Method: "POST"})
	if err != nil || resp.Status != 201 || inv.addon != "shop" {
		t.Fatalf("%+v %v %s", resp, err, inv.addon)
	}
	inv.out = `{"weird":true}`
	if _, err := d.APIRoute().Invoke(context.Background(), uuid.New(), "Fn", apiroute.Request{Addon: "shop"}); !errors.Is(err, apiroute.ErrBadResponse) {
		t.Fatalf("malformed route output must be ErrBadResponse: %v", err)
	}
}

func TestFormula(t *testing.T) {
	inv := &fakeInvoker{out: `{"success":true,"data":{"value":12.5}}`}
	d := New(inv, nil)
	f := d.Formula(func(context.Context, string) string { return "a" }, func(context.Context) uuid.UUID { return uuid.Nil })
	v, err := f(context.Background(), "m", "compiled:Price", map[string]any{"q": 2})
	if err != nil || v != 12.5 || inv.fn != "Price" {
		t.Fatalf("%v %v %q", v, err, inv.fn)
	}
	inv.out = `{"success":true,"data":7}`
	if v, _ := f(context.Background(), "m", "wasm:Price", nil); v != float64(7) {
		t.Fatalf("bare data: %v", v)
	}
	inv.out = `{"success":false,"error":{"message":"x"}}`
	if _, err := f(context.Background(), "m", "compiled:Price", nil); !errors.Is(err, ErrDeclined) {
		t.Fatalf("decline aborts: %v", err)
	}
}
