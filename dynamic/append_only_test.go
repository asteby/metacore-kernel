package dynamic

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/asteby/metacore-kernel/metadata"
	"github.com/asteby/metacore-kernel/modelbase"
)

func appendOnlyService(t *testing.T, models ...string) *Service {
	t.Helper()
	modelbase.Register("test_products", func() modelbase.ModelDefiner { return &TestProduct{} })
	set := map[string]bool{}
	for _, m := range models {
		set[m] = true
	}
	return New(Config{
		DB:                 setupTestDB(t),
		Metadata:           metadata.New(metadata.Config{CacheTTL: -1}),
		AppendOnlyResolver: func(_ context.Context, model string) bool { return set[model] },
	})
}

// A ledger row can be created, read and listed, but Update and Delete are
// refused with ErrAppendOnly and leave the row untouched.
func TestAppendOnly_RefusesUpdateAndDelete(t *testing.T) {
	svc := appendOnlyService(t, "test_products")
	user := newUser(uuid.New())
	ctx := context.Background()

	created := createProduct(t, svc, user, "Abono", 100)
	id := uuid.MustParse(created["id"].(string))

	_, err := svc.Update(ctx, "test_products", user, id, map[string]any{"price": 1.0})
	if !errors.Is(err, ErrAppendOnly) {
		t.Fatalf("update: want ErrAppendOnly, got %v", err)
	}
	var ae *AppendOnlyError
	if !errors.As(err, &ae) || ae.Op != "update" || ae.Model != "test_products" {
		t.Fatalf("update: want AppendOnlyError{update,test_products}, got %#v", err)
	}
	if err := svc.Delete(ctx, "test_products", user, id); !errors.Is(err, ErrAppendOnly) {
		t.Fatalf("delete: want ErrAppendOnly, got %v", err)
	}

	got, err := svc.Get(ctx, "test_products", user, id)
	if err != nil {
		t.Fatalf("row must survive the refused writes: %v", err)
	}
	if got["price"].(float64) != 100 {
		t.Fatalf("price changed to %v", got["price"])
	}
}

// Models the resolver does not flag, and a Service with no resolver, keep the
// legacy behaviour.
func TestAppendOnly_OtherModelsStayWritable(t *testing.T) {
	svc := appendOnlyService(t, "something_else")
	user := newUser(uuid.New())
	created := createProduct(t, svc, user, "Widget", 5)
	id := uuid.MustParse(created["id"].(string))
	if _, err := svc.Update(context.Background(), "test_products", user, id, map[string]any{"price": 6.0}); err != nil {
		t.Fatalf("update on a normal model: %v", err)
	}
	if err := svc.Delete(context.Background(), "test_products", user, id); err != nil {
		t.Fatalf("delete on a normal model: %v", err)
	}

	plain := setupService(t, setupTestDB(t))
	c2 := createProduct(t, plain, user, "Plain", 1)
	if _, err := plain.Update(context.Background(), "test_products", user, uuid.MustParse(c2["id"].(string)), map[string]any{"price": 2.0}); err != nil {
		t.Fatalf("no resolver: %v", err)
	}
}
