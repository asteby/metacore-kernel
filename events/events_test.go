package events

import (
	"bytes"
	"context"
	"log"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/asteby/metacore-sdk/pkg/manifest"
	"github.com/asteby/metacore-kernel/security"
	"github.com/google/uuid"
)

// newEnforcer builds a tiny in-memory enforcer for tests. The addon "tickets"
// may emit/subscribe under the "ticket.*" namespace; any other caller will
// trip a violation.
func newEnforcer(t *testing.T, mode security.Mode) *security.Enforcer {
	t.Helper()
	caps := security.Compile("tickets", []manifest.Capability{
		{Kind: "event:emit", Target: "ticket.*"},
		{Kind: "event:subscribe", Target: "ticket.*"},
	})
	e := security.NewEnforcer(func(key string) *security.Capabilities {
		if key == "tickets" {
			return caps
		}
		return nil
	})
	e.SetMode(mode)
	return e
}

// captureLog returns a (*log.Logger, *bytes.Buffer) pair so tests can assert
// on violation lines routed through the bus.
func captureLog() (*log.Logger, *bytes.Buffer) {
	buf := &bytes.Buffer{}
	return log.New(buf, "", 0), buf
}

func TestBus_PublishSubscribeHappy(t *testing.T) {
	bus := NewBus(newEnforcer(t, security.ModeEnforce))
	org := uuid.New()

	var got int32
	if err := bus.Subscribe("tickets", "ticket.created", func(_ context.Context, o uuid.UUID, p any) error {
		if o != org {
			t.Errorf("wrong org: %v", o)
		}
		if s, _ := p.(string); s != "hello" {
			t.Errorf("wrong payload: %v", p)
		}
		atomic.AddInt32(&got, 1)
		return nil
	}); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if err := bus.Publish(context.Background(), "tickets", "ticket.created", org, "hello"); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if atomic.LoadInt32(&got) != 1 {
		t.Fatalf("handler not invoked")
	}
}

func TestBus_Wildcard(t *testing.T) {
	bus := NewBus(newEnforcer(t, security.ModeEnforce))
	var got int32
	if err := bus.Subscribe("tickets", "ticket.*", func(context.Context, uuid.UUID, any) error {
		atomic.AddInt32(&got, 1)
		return nil
	}); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if err := bus.Publish(context.Background(), "tickets", "ticket.created", uuid.New(), nil); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if atomic.LoadInt32(&got) != 1 {
		t.Fatalf("wildcard did not match ticket.created (got=%d)", got)
	}
}

func TestBus_NoMatchNoCall(t *testing.T) {
	bus := NewBus(newEnforcer(t, security.ModeEnforce))
	var got int32
	if err := bus.Subscribe("tickets", "ticket.created", func(context.Context, uuid.UUID, any) error {
		atomic.AddInt32(&got, 1)
		return nil
	}); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	// Publish a *different* event the addon is also allowed to emit.
	if err := bus.Publish(context.Background(), "tickets", "ticket.resolved", uuid.New(), nil); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if atomic.LoadInt32(&got) != 0 {
		t.Fatalf("handler fired for non-matching event")
	}
}

func TestBus_CapabilityDeniedShadow(t *testing.T) {
	logger, buf := captureLog()
	enf := newEnforcer(t, security.ModeShadow)
	enf.Logger = logger
	bus := NewBus(enf).WithLogger(logger)

	// "order.created" is outside the tickets addon's event:emit scope — in
	// shadow mode Publish must succeed but a violation must be logged.
	if err := bus.Publish(context.Background(), "tickets", "order.created", uuid.New(), nil); err != nil {
		t.Fatalf("shadow must not error: %v", err)
	}
	if !strings.Contains(buf.String(), "metacore.capability.violation") {
		t.Fatalf("expected capability violation log, got:\n%s", buf.String())
	}
}

func TestBus_CapabilityDeniedEnforce(t *testing.T) {
	bus := NewBus(newEnforcer(t, security.ModeEnforce))
	err := bus.Publish(context.Background(), "tickets", "order.created", uuid.New(), nil)
	if err == nil {
		t.Fatalf("expected capability rejection in enforce mode")
	}
}

func TestBus_UnsubscribeAddon(t *testing.T) {
	bus := NewBus(newEnforcer(t, security.ModeEnforce))
	var got int32
	h := func(context.Context, uuid.UUID, any) error {
		atomic.AddInt32(&got, 1)
		return nil
	}
	if err := bus.Subscribe("tickets", "ticket.created", h); err != nil {
		t.Fatalf("subscribe exact: %v", err)
	}
	if err := bus.Subscribe("tickets", "ticket.*", h); err != nil {
		t.Fatalf("subscribe wildcard: %v", err)
	}
	bus.Unsubscribe("tickets")
	if err := bus.Publish(context.Background(), "tickets", "ticket.created", uuid.New(), nil); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if atomic.LoadInt32(&got) != 0 {
		t.Fatalf("handler fired after Unsubscribe (got=%d)", got)
	}
}

func TestEventMatches(t *testing.T) {
	cases := []struct {
		pattern, event string
		want           bool
	}{
		{"ticket.created", "ticket.created", true},
		{"ticket.created", "ticket.resolved", false},
		{"ticket.*", "ticket.created", true},
		{"ticket.*", "ticket", true},
		{"ticket.*", "tickets.bulk", false},
		{"*", "anything", true},
	}
	for _, c := range cases {
		if got := eventMatches(c.pattern, c.event); got != c.want {
			t.Errorf("eventMatches(%q,%q)=%v want %v", c.pattern, c.event, got, c.want)
		}
	}
}
