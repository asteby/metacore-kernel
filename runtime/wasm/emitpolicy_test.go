package wasm

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/asteby/metacore-kernel/events"
	"github.com/asteby/metacore-kernel/manifest"
	"github.com/asteby/metacore-kernel/security"
	"github.com/google/uuid"
)

// emitPolicyHarness loads the event_emit fixture (it always emits "ev.fired"
// with the invocation input as payload) for addon "testaddon" in an org where
// an addon keyed "ev" is installed, so "ev.fired" is a foreign namespace.
type emitPolicyHarness struct {
	h          *Host
	mu         sync.Mutex
	fired      int
	emitters   []string
	violations []security.EmitViolation
}

func newEmitPolicyHarness(t *testing.T, mode security.EmitMode) *emitPolicyHarness {
	t.Helper()
	ctx := context.Background()
	x := &emitPolicyHarness{}
	bus := events.NewBus(nil)
	_ = bus.SubscribeEvent("kernel", "ev.fired", func(ctx context.Context, _ uuid.UUID, _ string, _ any) error {
		x.mu.Lock()
		defer x.mu.Unlock()
		x.fired++
		x.emitters = append(x.emitters, events.EmitterFromContext(ctx))
		return nil
	})
	_ = bus.Subscribe("kernel", security.EmitViolationEvent, func(_ context.Context, _ uuid.UUID, p any) error {
		x.mu.Lock()
		defer x.mu.Unlock()
		v, _ := p.(security.EmitViolation)
		x.violations = append(x.violations, v)
		return nil
	})
	policy := &security.EmitPolicy{Source: security.EmitFactsFunc(func(context.Context, uuid.UUID, string) (security.EmitFacts, error) {
		return security.EmitFacts{Installed: []string{"ev", "testaddon"}}, nil
	})}
	policy.SetMode(security.EmitGroupReserved, mode)
	policy.SetMode(security.EmitGroupDeclared, mode)

	h, err := NewHost(ctx, security.Compile("testaddon", nil), nil)
	if err != nil {
		t.Fatalf("NewHost: %v", err)
	}
	t.Cleanup(func() { h.Close(ctx) })
	h.WithBus(bus).WithEmitPolicy(policy.Check)
	spec := &manifest.BackendSpec{Runtime: "wasm", Entry: "backend.wasm", Exports: []string{"emit_test"}, MemoryLimitMB: 4, TimeoutMs: 2000}
	if err := h.Load(ctx, "testaddon", eventEmitWasm(), spec); err != nil {
		t.Fatalf("Load: %v", err)
	}
	x.h = h
	return x
}

func (x *emitPolicyHarness) emit(t *testing.T, payload string) map[string]any {
	t.Helper()
	out, err := x.h.InvokeFor(context.Background(), uuid.New(), uuid.New(), "testaddon", "emit_test", []byte(payload), nil)
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	return decodeEnvelope(t, out)
}

func TestEventEmit_PolicyEnforceDenies(t *testing.T) {
	x := newEmitPolicyHarness(t, security.EmitModeEnforce)
	env := x.emit(t, `{"a":1}`)
	if ok, _ := env["success"].(bool); ok {
		t.Fatalf("want forbidden, got %v", env)
	}
	if code := env["error"].(map[string]any)["code"]; code != "forbidden" {
		t.Fatalf("code = %v", code)
	}
	if x.fired != 0 {
		t.Fatalf("denied event reached %d subscribers", x.fired)
	}
	if len(x.violations) != 1 || x.violations[0].Rule != security.EmitRuleForeign || !x.violations[0].Denied || x.violations[0].Owner != "ev" {
		t.Fatalf("violation event = %+v", x.violations)
	}
}

func TestEventEmit_PolicyWarnPublishesAndReports(t *testing.T) {
	x := newEmitPolicyHarness(t, security.EmitModeWarn)
	// The guest claims to be someone else; the subscriber must still see the
	// invoking addon as emitter.
	env := x.emit(t, `{"emitter":"ev"}`)
	if ok, _ := env["success"].(bool); !ok {
		t.Fatalf("warn must publish, got %v", env)
	}
	if x.fired != 1 || x.emitters[0] != "testaddon" {
		t.Fatalf("fired=%d emitters=%v, want 1 from testaddon", x.fired, x.emitters)
	}
	if len(x.violations) != 1 || x.violations[0].Denied || x.violations[0].Mode != "warn" {
		t.Fatalf("violation event = %+v", x.violations)
	}
	b, _ := json.Marshal(x.violations[0])
	for _, k := range []string{`"addon":"testaddon"`, `"event":"ev.fired"`, `"rule":"foreign_namespace"`} {
		if !strings.Contains(string(b), k) {
			t.Fatalf("violation payload %s lacks %s", b, k)
		}
	}
}

func TestEventEmit_NoPolicyUnchanged(t *testing.T) {
	x := newEmitPolicyHarness(t, security.EmitModeOff)
	if env := x.emit(t, ``); env["success"] != true || x.fired != 1 || len(x.violations) != 0 {
		t.Fatalf("off: env=%v fired=%d violations=%v", env, x.fired, x.violations)
	}
}
