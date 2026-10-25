package events

import (
	"context"
	"testing"

	"github.com/google/uuid"
)

func TestEstimateSubscribers(t *testing.T) {
	b := NewBus(nil)
	org := uuid.New()
	ctx := context.Background()
	noop := func(context.Context, uuid.UUID, any) error { return nil }
	if err := b.SubscribeTap("kernel", "*", noop); err != nil {
		t.Fatal(err)
	}
	if n := b.EstimateSubscribers(ctx, org, "a.b"); n != 0 {
		t.Fatalf("tap only: %d, want 0", n)
	}
	_ = b.Subscribe("x", "a.*", noop)
	if n := b.EstimateSubscribers(ctx, org, "a.b"); n != 1 {
		t.Fatalf("one plain subscriber: %d, want 1", n)
	}
	_ = b.SubscribeRoutingEstimated("kernel", "*",
		func(context.Context, uuid.UUID, string, any) (int, error) { return 0, nil },
		func(_ context.Context, o uuid.UUID, _ string) (int, bool) { return 3, o == org })
	if n := b.EstimateSubscribers(ctx, org, "a.b"); n != 4 {
		t.Fatalf("plain + router(3): %d, want 4", n)
	}
	if n := b.EstimateSubscribers(ctx, uuid.New(), "a.b"); n != -1 {
		t.Fatalf("estimator cannot tell: %d, want -1", n)
	}
	_ = b.SubscribeRouting("kernel", "*", func(context.Context, uuid.UUID, string, any) (int, error) { return 0, nil })
	if n := b.EstimateSubscribers(ctx, org, "a.b"); n != -1 {
		t.Fatalf("router without estimator: %d, want -1", n)
	}
}
