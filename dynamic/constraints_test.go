package dynamic

import (
	"context"
	"errors"
	"testing"

	"github.com/asteby/metacore-kernel/manifest"
	"github.com/asteby/metacore-kernel/metadata"
	"github.com/asteby/metacore-kernel/modelbase"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

func TestEvalConstraintExpr(t *testing.T) {
	env := map[string]float64{"quantity": 5, "price": 10, "cost": 8}
	cases := []struct {
		expr string
		want bool
	}{
		{"quantity >= 0", true},
		{"quantity >= 10", false},
		{"price >= cost", true},
		{"cost >= price", false},
		{"price - cost >= 0", true},
		{"quantity == 5", true},
		{"quantity != 5", false},
		{"quantity < 10", true},
		{"price <= 10", true},
	}
	for _, c := range cases {
		got, err := evalConstraintExpr(c.expr, env)
		if err != nil {
			t.Fatalf("%q: unexpected error %v", c.expr, err)
		}
		if got != c.want {
			t.Errorf("%q = %v, want %v", c.expr, got, c.want)
		}
	}

	if _, err := evalConstraintExpr("quantity + 1", env); err == nil {
		t.Error("expr without comparison operator should error")
	}
}

// constraintService builds a Service whose ConstraintResolver returns the given
// config for test_products (and nothing for other models).
func constraintService(t *testing.T, db *gorm.DB, mc *ModelConstraints) *Service {
	t.Helper()
	modelbase.Register("test_products", func() modelbase.ModelDefiner { return &TestProduct{} })
	meta := metadata.New(metadata.Config{CacheTTL: -1})
	return New(Config{
		DB:       db,
		Metadata: meta,
		ConstraintResolver: func(_ context.Context, model string) (*ModelConstraints, bool) {
			if model == "test_products" {
				return mc, true
			}
			return nil, false
		},
	})
}

func TestConstraints_CreateRejectsViolation(t *testing.T) {
	db := setupTestDB(t)
	svc := constraintService(t, db, &ModelConstraints{
		Constraints: []manifest.ConstraintDef{{Expr: "price >= 0", ErrorKey: "price.negative"}},
	})
	user := newUser(uuid.New())

	_, err := svc.Create(context.Background(), "test_products", user, map[string]any{"name": "Bad", "price": -1.0})
	if err == nil {
		t.Fatal("expected create to be rejected by the guard")
	}
	if !errors.Is(err, ErrConstraintViolation) {
		t.Fatalf("want ErrConstraintViolation, got %v", err)
	}
	var ce *ConstraintError
	if !errors.As(err, &ce) || ce.ErrorKey != "price.negative" {
		t.Fatalf("want ConstraintError with key price.negative, got %v", err)
	}

	// A valid row passes.
	if _, err := svc.Create(context.Background(), "test_products", user, map[string]any{"name": "Good", "price": 5.0}); err != nil {
		t.Fatalf("valid create should pass: %v", err)
	}
}

func TestConstraints_UpdateRejectsViolation(t *testing.T) {
	db := setupTestDB(t)
	svc := constraintService(t, db, &ModelConstraints{
		Constraints: []manifest.ConstraintDef{{Expr: "price >= 0", ErrorKey: "price.negative"}},
	})
	user := newUser(uuid.New())

	created := createProduct(t, svc, user, "Widget", 9.99)
	id := uuid.MustParse(created["id"].(string))

	_, err := svc.Update(context.Background(), "test_products", user, id, map[string]any{"price": -3.0})
	if !errors.Is(err, ErrConstraintViolation) {
		t.Fatalf("update violating guard should fail with ErrConstraintViolation, got %v", err)
	}

	// The row is unchanged (the guard aborted before the write).
	got, err := svc.Get(context.Background(), "test_products", user, id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got["price"].(float64) != 9.99 {
		t.Fatalf("price should be unchanged 9.99, got %v", got["price"])
	}

	// A valid update passes.
	if _, err := svc.Update(context.Background(), "test_products", user, id, map[string]any{"price": 12.0}); err != nil {
		t.Fatalf("valid update should pass: %v", err)
	}
}

func TestModelConstraints_locksRows(t *testing.T) {
	none := (*ModelConstraints)(nil)
	if none.locksRows() {
		t.Error("nil config must not lock")
	}
	lockingNoConstraints := &ModelConstraints{Locking: "row"}
	if lockingNoConstraints.locksRows() {
		t.Error("locking without constraints is inert")
	}
	full := &ModelConstraints{Locking: "row", Constraints: []manifest.ConstraintDef{{Expr: "quantity >= 0", ErrorKey: "k"}}}
	if !full.locksRows() {
		t.Error("row locking with constraints should lock")
	}
}
