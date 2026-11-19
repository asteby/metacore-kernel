package dynamic

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/asteby/metacore-kernel/metadata"
	"github.com/asteby/metacore-kernel/modelbase"
	"github.com/asteby/metacore-kernel/query"
)

// SEG-01: a filter that cannot be applied must fail loudly, not widen the
// result set.
func TestSEG01_UnknownFilterIsRejected(t *testing.T) {
	db := setupTestDB(t)
	svc := setupService(t, db)
	user := newUser(uuid.New())
	createProduct(t, svc, user, "A", 1)
	createProduct(t, svc, user, "B", 2)
	ctx := context.Background()

	bad := []query.Params{
		{Page: 1, PerPage: 10, Filters: map[string]query.Filter{"no_such_col": {Op: query.OpEq, Value: "x"}}},
		{Page: 1, PerPage: 10, Filters: map[string]query.Filter{"name": {Op: query.OpIn, Value: "not-a-list"}}},
		{Page: 1, PerPage: 10, Filters: map[string]query.Filter{"name": {Op: query.FilterOp("bogus"), Value: "x"}}},
		{Page: 1, PerPage: 10, Filters: map[string]query.Filter{"name; DROP": {Op: query.OpEq, Value: "x"}}},
	}
	for i, p := range bad {
		if _, _, err := svc.List(ctx, "test_products", user, p); !errors.Is(err, query.ErrUnknownFilter) {
			t.Errorf("case %d List: want ErrUnknownFilter, got %v", i, err)
		}
		if _, err := svc.Aggregate(ctx, "test_products", user, p, []string{"price"}); !errors.Is(err, query.ErrUnknownFilter) {
			t.Errorf("case %d Aggregate: want ErrUnknownFilter, got %v", i, err)
		}
	}

	// Valid filter and empty value still work.
	items, _, err := svc.List(ctx, "test_products", user, query.Params{Page: 1, PerPage: 10,
		Filters: map[string]query.Filter{"name": {Op: query.OpEq, Value: "A"}}})
	if err != nil || len(items) != 1 {
		t.Fatalf("valid filter: items=%d err=%v", len(items), err)
	}
	if _, _, err := svc.List(ctx, "test_products", user, query.Params{Page: 1, PerPage: 10,
		Filters: map[string]query.Filter{"name": {Op: query.OpEq, Value: ""}}}); err != nil {
		t.Fatalf("empty value is no-filter, got %v", err)
	}
}

func TestSEG01_LenientFiltersDropsAndLists(t *testing.T) {
	db := setupTestDB(t)
	modelbase.Register("test_products", func() modelbase.ModelDefiner { return &TestProduct{} })
	svc := New(Config{AllowUnconfiguredPermissions: true, DB: db,
		Metadata: metadata.New(metadata.Config{CacheTTL: -1}), LenientFilters: true})
	user := newUser(uuid.New())
	createProduct(t, svc, user, "A", 1)
	createProduct(t, svc, user, "B", 2)

	items, _, err := svc.List(context.Background(), "test_products", user, query.Params{Page: 1, PerPage: 10,
		Filters: map[string]query.Filter{"no_such_col": {Op: query.OpEq, Value: "x"}}})
	if err != nil || len(items) != 2 {
		t.Fatalf("lenient: want 2 rows, nil err; got %d, %v", len(items), err)
	}
}
