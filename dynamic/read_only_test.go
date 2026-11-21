package dynamic

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/asteby/metacore-kernel/metadata"
	"github.com/asteby/metacore-kernel/modelbase"
)

// A read-only (view-backed) model refuses every write and leaves reads alone.
func TestReadOnly_RefusesWrites(t *testing.T) {
	modelbase.Register("test_products", func() modelbase.ModelDefiner { return &TestProduct{} })
	db := setupTestDB(t)
	user := newUser(uuid.New())
	ctx := context.Background()
	open := setupService(t, db)
	created := createProduct(t, open, user, "Item", 3)
	id := uuid.MustParse(created["id"].(string))

	svc := New(Config{
		AllowUnconfiguredPermissions: true,
		DB:                           db,
		Metadata:                     metadata.New(metadata.Config{CacheTTL: -1}),
		ReadOnlyResolver:             func(_ context.Context, m string) bool { return m == "test_products" },
	})
	if _, err := svc.Create(ctx, "test_products", user, map[string]any{"name": "x", "price": 1.0}); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("create: %v", err)
	}
	if _, err := svc.Update(ctx, "test_products", user, id, map[string]any{"price": 9.0}); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("update: %v", err)
	}
	if err := svc.Delete(ctx, "test_products", user, id); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("delete: %v", err)
	}
	var re *ReadOnlyError
	if _, err := svc.Restore(ctx, "test_products", user, id); !errors.As(err, &re) {
		t.Fatalf("restore: %v", err)
	}
	if _, err := svc.Get(ctx, "test_products", user, id); err != nil {
		t.Fatalf("read must work: %v", err)
	}
}
