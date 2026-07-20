package events

import (
	"context"
	"testing"

	"github.com/google/uuid"
)

// TestSubscribeEvent_ReceivesEventName is the new capability: a subscriber can
// see WHICH event matched, which is what lets a routing layer dispatch by name
// instead of guessing from the payload's shape.
func TestSubscribeEvent_ReceivesEventName(t *testing.T) {
	b := NewBus(nil)
	var got string
	if err := b.SubscribeEvent("kernel", "pos.*", func(_ context.Context, _ uuid.UUID, event string, _ any) error {
		got = event
		return nil
	}); err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	if err := b.Publish(context.Background(), "kernel", "pos.order_created", uuid.New(), nil); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if got != "pos.order_created" {
		t.Fatalf("handler saw event %q, want pos.order_created", got)
	}
}

// TestSubscribe_LegacyHandlerStillWorks pins the source-compat promise: the
// original payload-only Handler keeps working untouched (ops' EventBus adapter
// depends on it).
func TestSubscribe_LegacyHandlerStillWorks(t *testing.T) {
	b := NewBus(nil)
	var got any
	if err := b.Subscribe("kernel", "pos.order_created", func(_ context.Context, _ uuid.UUID, payload any) error {
		got = payload
		return nil
	}); err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	n, err := b.PublishWithCount(context.Background(), "kernel", "pos.order_created", uuid.New(), "hello")
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if n != 1 {
		t.Fatalf("count = %d, want 1", n)
	}
	if got != "hello" {
		t.Fatalf("payload = %v, want hello", got)
	}
}

// TestRoutingHandler_ContributesItsOwnCount is the honest-count mechanism: a
// router that forwards to N downstream handlers reports N, and a router that
// drops the event reports 0 rather than inflating the total to 1.
func TestRoutingHandler_ContributesItsOwnCount(t *testing.T) {
	tests := []struct {
		name      string
		routed    int
		want      int
		withPlain bool
	}{
		{name: "router drops event", routed: 0, want: 0},
		{name: "router fans out to three", routed: 3, want: 3},
		{name: "router plus a plain subscriber", routed: 2, want: 3, withPlain: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b := NewBus(nil)
			if err := b.SubscribeRouting("kernel", "*", func(_ context.Context, _ uuid.UUID, _ string, _ any) (int, error) {
				return tc.routed, nil
			}); err != nil {
				t.Fatalf("subscribe routing: %v", err)
			}
			if tc.withPlain {
				if err := b.Subscribe("kernel", "pos.order_created", func(_ context.Context, _ uuid.UUID, _ any) error {
					return nil
				}); err != nil {
					t.Fatalf("subscribe: %v", err)
				}
			}

			n, err := b.PublishWithCount(context.Background(), "kernel", "pos.order_created", uuid.New(), nil)
			if err != nil {
				t.Fatalf("publish: %v", err)
			}
			if n != tc.want {
				t.Fatalf("count = %d, want %d", n, tc.want)
			}
		})
	}
}
