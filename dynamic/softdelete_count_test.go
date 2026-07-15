package dynamic

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/asteby/metacore-kernel/query"
)

// TestListTotalExcludesSoftDeleted: for compiled models (gorm.DeletedAt) the
// COUNT behind meta.Total must apply the same soft-delete filter Find does —
// otherwise total > len(rows) and paginated clients chase pages that never
// arrive (infinite-scroll sentinels loop on empty pages).
func TestListTotalExcludesSoftDeleted(t *testing.T) {
	db := setupTestDB(t)
	svc := setupService(t, db)
	user := newUser(uuid.New())

	createProduct(t, svc, user, "Keep", 1)
	gone := createProduct(t, svc, user, "Gone", 2)
	goneID, err := uuid.Parse(gone["id"].(string))
	if err != nil {
		t.Fatalf("parse id: %v (%+v)", err, gone)
	}
	if err := svc.Delete(context.Background(), "test_products", user, goneID); err != nil {
		t.Fatalf("delete: %v", err)
	}

	items, meta, err := svc.List(context.Background(), "test_products", user, query.Params{Page: 1, PerPage: 10})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("len(items) = %d, want 1", len(items))
	}
	if meta.Total != 1 {
		t.Fatalf("meta.Total = %d, want 1 (soft-deleted row counted)", meta.Total)
	}

	// Facets must agree with the list too.
	buckets, err := svc.Facets(context.Background(), user, FacetsQuery{Model: "test_products", Field: "name"})
	if err != nil {
		t.Fatalf("facets: %v", err)
	}
	for _, bkt := range buckets {
		if bkt.Value == "Gone" {
			t.Fatalf("facets include soft-deleted value: %+v", buckets)
		}
	}
}
