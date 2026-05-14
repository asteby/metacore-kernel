package lifecycle

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/asteby/metacore-kernel/manifest"
	"github.com/google/uuid"
)

// recordingDispatcher captures every HookRequest it sees and can be primed
// to return an error — keeps the unit tests below independent from the
// wasm/webhook back-ends the production runner wires.
type recordingDispatcher struct {
	mu    sync.Mutex
	calls []HookRequest
	err   error
}

func (r *recordingDispatcher) Dispatch(_ context.Context, req HookRequest) (HookResponse, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, req)
	return HookResponse{}, r.err
}

func (r *recordingDispatcher) snapshot() []HookRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]HookRequest, len(r.calls))
	copy(out, r.calls)
	return out
}

func newManifestWithHooks(hooks map[string][]manifest.HookDef) manifest.Manifest {
	return manifest.Manifest{
		Key:            "demo",
		Name:           "Demo",
		Version:        "1.0.0",
		LifecycleHooks: hooks,
	}
}

func TestHookRunner_Run_DispatchesEveryDeclaredHook(t *testing.T) {
	rec := &recordingDispatcher{}
	runner := NewHookRunner(nil)
	runner.Register(HookTargetWebhook, rec)

	m := newManifestWithHooks(map[string][]manifest.HookDef{
		HookEventInstall: {
			{Target: manifest.HookTarget{Type: HookTargetWebhook, URL: "https://a.example/hook"}},
			{Target: manifest.HookTarget{Type: HookTargetWebhook, URL: "https://b.example/hook"}, Priority: 5},
		},
	})

	orgID := uuid.New()
	if err := runner.Run(context.Background(), m.Key, orgID, HookEventInstall, m, []byte(`{"event":"install"}`)); err != nil {
		t.Fatalf("Run: %v", err)
	}
	calls := rec.snapshot()
	if len(calls) != 2 {
		t.Fatalf("dispatcher saw %d calls, want 2", len(calls))
	}
	if calls[0].Target.URL != "https://a.example/hook" {
		t.Fatalf("priority-0 hook should fire first, got %q", calls[0].Target.URL)
	}
	if calls[1].Target.URL != "https://b.example/hook" {
		t.Fatalf("priority-5 hook should fire second, got %q", calls[1].Target.URL)
	}
	if calls[0].AddonKey != "demo" || calls[0].OrgID != orgID || calls[0].Event != HookEventInstall {
		t.Fatalf("request metadata wrong: %+v", calls[0])
	}
}

func TestHookRunner_Run_BeforeEventErrorAbortsChain(t *testing.T) {
	rec := &recordingDispatcher{err: errors.New("guest rejected")}
	runner := NewHookRunner(nil)
	runner.Register(HookTargetWebhook, rec)

	m := newManifestWithHooks(map[string][]manifest.HookDef{
		HookEventBeforeCreate: {
			{Target: manifest.HookTarget{Type: HookTargetWebhook, URL: "https://a.example/hook"}},
			// Second hook MUST NOT run after the first errors — chain aborts.
			{Target: manifest.HookTarget{Type: HookTargetWebhook, URL: "https://b.example/hook"}},
		},
	})

	err := runner.Run(context.Background(), m.Key, uuid.New(), HookEventBeforeCreate, m, nil)
	if err == nil {
		t.Fatal("expected error from before_create chain, got nil")
	}
	if len(rec.snapshot()) != 1 {
		t.Fatalf("expected 1 call before abort, got %d", len(rec.snapshot()))
	}
}

func TestHookRunner_Run_AfterEventErrorIsSwallowed(t *testing.T) {
	rec := &recordingDispatcher{err: errors.New("guest crashed")}
	runner := NewHookRunner(nil)
	runner.Register(HookTargetWebhook, rec)

	m := newManifestWithHooks(map[string][]manifest.HookDef{
		HookEventAfterCreate: {
			{Target: manifest.HookTarget{Type: HookTargetWebhook, URL: "https://a.example/hook"}},
			{Target: manifest.HookTarget{Type: HookTargetWebhook, URL: "https://b.example/hook"}},
		},
	})

	if err := runner.Run(context.Background(), m.Key, uuid.New(), HookEventAfterCreate, m, nil); err != nil {
		t.Fatalf("after_create errors must be swallowed, got %v", err)
	}
	// Both hooks run despite the first erroring — after_* hooks log+continue.
	if got := len(rec.snapshot()); got != 2 {
		t.Fatalf("expected both after_create hooks to fire, got %d calls", got)
	}
}

// asyncFlagDispatcher signals a channel when invoked so the async branch
// can be exercised without timing assumptions.
type asyncFlagDispatcher struct {
	hits int32
	done chan struct{}
}

func (a *asyncFlagDispatcher) Dispatch(_ context.Context, _ HookRequest) (HookResponse, error) {
	atomic.AddInt32(&a.hits, 1)
	close(a.done)
	return HookResponse{}, nil
}

func TestHookRunner_Run_AsyncAfterHookDoesNotBlock(t *testing.T) {
	disp := &asyncFlagDispatcher{done: make(chan struct{})}
	runner := NewHookRunner(nil)
	runner.Register(HookTargetWebhook, disp)

	m := newManifestWithHooks(map[string][]manifest.HookDef{
		HookEventAfterUpdate: {
			{
				Target: manifest.HookTarget{Type: HookTargetWebhook, URL: "https://a.example/hook"},
				Async:  true,
			},
		},
	})

	if err := runner.Run(context.Background(), m.Key, uuid.New(), HookEventAfterUpdate, m, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	// Goroutine must complete within a reasonable bound for a test to be
	// reliable on CI — the dispatcher is synchronous itself, so the only
	// scheduling delay is the goroutine startup.
	select {
	case <-disp.done:
	case <-time.After(2 * time.Second):
		t.Fatalf("async hook did not run within 2s")
	}
	if atomic.LoadInt32(&disp.hits) != 1 {
		t.Fatalf("hits = %d, want 1", disp.hits)
	}
}

func TestHookRunner_Run_NoDispatcher_BeforeReturnsError(t *testing.T) {
	runner := NewHookRunner(nil)
	m := newManifestWithHooks(map[string][]manifest.HookDef{
		HookEventBeforeCreate: {
			{Target: manifest.HookTarget{Type: HookTargetWebhook, URL: "https://a.example/hook"}},
		},
	})

	err := runner.Run(context.Background(), m.Key, uuid.New(), HookEventBeforeCreate, m, nil)
	if err == nil {
		t.Fatal("expected error when no dispatcher is registered for a before-hook")
	}
	if !errors.Is(err, ErrNoDispatcher) {
		t.Fatalf("err = %v, want ErrNoDispatcher", err)
	}
}

func TestHookRunner_Run_NoDispatcher_AfterIsSwallowed(t *testing.T) {
	runner := NewHookRunner(nil)
	m := newManifestWithHooks(map[string][]manifest.HookDef{
		HookEventAfterCreate: {
			{Target: manifest.HookTarget{Type: HookTargetWebhook, URL: "https://a.example/hook"}},
		},
	})

	if err := runner.Run(context.Background(), m.Key, uuid.New(), HookEventAfterCreate, m, nil); err != nil {
		t.Fatalf("after_create must swallow missing dispatcher, got %v", err)
	}
}

func TestHookRunner_Run_NoHooksDeclared_IsNoop(t *testing.T) {
	rec := &recordingDispatcher{}
	runner := NewHookRunner(nil)
	runner.Register(HookTargetWebhook, rec)

	m := manifest.Manifest{Key: "demo", Version: "1.0.0"}
	if err := runner.Run(context.Background(), m.Key, uuid.New(), HookEventInstall, m, nil); err != nil {
		t.Fatalf("empty manifest must not error, got %v", err)
	}
	if got := len(rec.snapshot()); got != 0 {
		t.Fatalf("expected zero dispatcher calls, got %d", got)
	}
}

func TestHookRunner_Nil_IsNoop(t *testing.T) {
	var runner *HookRunner
	m := newManifestWithHooks(map[string][]manifest.HookDef{
		HookEventInstall: {{Target: manifest.HookTarget{Type: HookTargetWebhook, URL: "x"}}},
	})
	if err := runner.Run(context.Background(), m.Key, uuid.New(), HookEventInstall, m, nil); err != nil {
		t.Fatalf("nil runner must be a no-op, got %v", err)
	}
}

func TestIsBeforeEvent(t *testing.T) {
	cases := map[string]bool{
		HookEventInstall:      true,
		HookEventEnable:       true,
		HookEventDisable:      true,
		HookEventUninstall:    true,
		HookEventUpgrade:      true,
		HookEventBeforeCreate: true,
		HookEventBeforeUpdate: true,
		HookEventBeforeDelete: true,
		HookEventAfterCreate:  false,
		HookEventAfterUpdate:  false,
		HookEventAfterDelete:  false,
	}
	for ev, want := range cases {
		if got := IsBeforeEvent(ev); got != want {
			t.Errorf("IsBeforeEvent(%q) = %v, want %v", ev, got, want)
		}
	}
}
