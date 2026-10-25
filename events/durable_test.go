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

func TestPublishDurableOnlyRunsOnlyDurableRouters(t *testing.T) {
	b := NewBus(nil)
	var plain, tap, router, durable int
	_ = b.SubscribeEvent("a", "*", func(context.Context, uuid.UUID, string, any) error { plain++; return nil })
	_ = b.SubscribeTap("a", "*", func(context.Context, uuid.UUID, any) error { tap++; return nil })
	_ = b.SubscribeRouting("a", "*", func(context.Context, uuid.UUID, string, any) (int, error) { router++; return 1, nil })
	fail := true
	_ = b.SubscribeDurableRouting("kernel", "*", func(context.Context, uuid.UUID, string, any) (int, error) {
		durable++
		if fail {
			return 0, errors.New("ledger down")
		}
		return 1, nil
	}, nil)

	err := b.Publish(context.Background(), "kernel", "x.y", uuid.New(), 1)
	var d interface{ SiblingsRan() bool }
	if !errors.Is(err, ErrDurableDelivery) || !errors.As(err, &d) || !d.SiblingsRan() {
		t.Fatalf("full publish err = %v, want ErrDurableDelivery with SiblingsRan()=true", err)
	}
	err = b.PublishDurableOnly(context.Background(), "kernel", "x.y", uuid.New(), 1)
	if !errors.Is(err, ErrDurableDelivery) || !errors.As(err, &d) || d.SiblingsRan() {
		t.Fatalf("durable-only err = %v, want ErrDurableDelivery with SiblingsRan()=false", err)
	}
	fail = false
	if err := b.PublishDurableOnly(context.Background(), "kernel", "x.y", uuid.New(), 1); err != nil {
		t.Fatal(err)
	}
	if plain != 1 || tap != 1 || router != 1 || durable != 3 {
		t.Fatalf("plain=%d tap=%d router=%d durable=%d, want 1/1/1/3", plain, tap, router, durable)
	}
}

func TestPublishDurableOnlyKeepsCapabilityAndNameChecks(t *testing.T) {
	b := NewBus(nil)
	if err := b.PublishDurableOnly(context.Background(), "kernel", "", uuid.New(), 1); err == nil {
		t.Fatal("empty event name must be rejected")
	}
}
