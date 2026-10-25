package events

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
)

func TestPublishPropagatesDurableRouterError(t *testing.T) {
	b := NewBus(nil)
	var sibling int
	boom := errors.New("ledger down")
	_ = b.SubscribeDurableRouting("kernel", "*", func(context.Context, uuid.UUID, string, any) (int, error) { return 0, boom }, nil)
	_ = b.SubscribeEvent("a", "x.y", func(context.Context, uuid.UUID, string, any) error { sibling++; return nil })

	n, err := b.PublishWithCount(context.Background(), "kernel", "x.y", uuid.New(), 1)
	if !errors.Is(err, ErrDurableDelivery) || !errors.Is(err, boom) {
		t.Fatalf("err = %v, want ErrDurableDelivery wrapping the router error", err)
	}
	if sibling != 1 || n != 1 {
		t.Fatalf("siblings must still run: sibling=%d n=%d", sibling, n)
	}
	if err := b.Publish(context.Background(), "kernel", "x.y", uuid.New(), 1); err == nil {
		t.Fatal("Publish must surface the durable error too")
	}
}

func TestPlainHandlerAndRouterErrorsStaySwallowed(t *testing.T) {
	b := NewBus(nil)
	_ = b.SubscribeEvent("a", "*", func(context.Context, uuid.UUID, string, any) error { return errors.New("x") })
	_ = b.SubscribeRouting("a", "*", func(context.Context, uuid.UUID, string, any) (int, error) { return 0, errors.New("y") })
	_ = b.SubscribeTap("a", "*", func(context.Context, uuid.UUID, any) error { return errors.New("z") })
	if _, err := b.PublishWithCount(context.Background(), "kernel", "x.y", uuid.New(), 1); err != nil {
		t.Fatalf("non-durable errors must not block publication: %v", err)
	}
}

func TestDurableRouterSuccessIsUnchanged(t *testing.T) {
	b := NewBus(nil)
	_ = b.SubscribeDurableRouting("kernel", "*", func(context.Context, uuid.UUID, string, any) (int, error) { return 2, nil }, nil)
	n, err := b.PublishWithCount(context.Background(), "kernel", "x.y", uuid.New(), 1)
	if err != nil || n != 2 {
		t.Fatalf("n=%d err=%v", n, err)
	}
}
