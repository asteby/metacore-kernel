package dynamic

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
)

// TestFacetsDistinctValuesAndCounts: facets returns each distinct value once
// with its row count, ordered by count desc then value asc.
func TestFacetsDistinctValuesAndCounts(t *testing.T) {
	db := setupTestDB(t)
	svc := setupService(t, db)
	user := newUser(uuid.New())

	// 3× "Widget", 1× "Gadget" — same org.
	createProduct(t, svc, user, "Widget", 1)
	createProduct(t, svc, user, "Widget", 2)
	createProduct(t, svc, user, "Widget", 3)
	createProduct(t, svc, user, "Gadget", 4)

	got, err := svc.Facets(context.Background(), user, FacetsQuery{
		Model: "test_products",
		Field: "name",
	})
	if err != nil {
		t.Fatalf("facets: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2 (%+v)", len(got), got)
	}
	// count DESC → Widget first.
	if got[0].Value != "Widget" || got[0].Count != 3 {
		t.Fatalf("bucket[0] = %+v, want {Widget,3}", got[0])
	}
	if got[0].Label != "Widget" {
		t.Fatalf("label = %q, want Widget (mirrors value)", got[0].Label)
	}
	if got[1].Value != "Gadget" || got[1].Count != 1 {
		t.Fatalf("bucket[1] = %+v, want {Gadget,1}", got[1])
	}
}

// TestFacetsOrgScoped: two organizations' values never mix.
func TestFacetsOrgScoped(t *testing.T) {
	db := setupTestDB(t)
	svc := setupService(t, db)

	orgA := uuid.New()
	orgB := uuid.New()
	userA := newUser(orgA)
	userB := newUser(orgB)

	createProduct(t, svc, userA, "OnlyA", 1)
	createProduct(t, svc, userA, "OnlyA", 2)
	createProduct(t, svc, userB, "OnlyB", 3)

	got, err := svc.Facets(context.Background(), userA, FacetsQuery{
		Model: "test_products",
		Field: "name",
	})
	if err != nil {
		t.Fatalf("facets: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("tenant scope leaked: got %d buckets, want 1 (%+v)", len(got), got)
	}
	if got[0].Value != "OnlyA" || got[0].Count != 2 {
		t.Fatalf("bucket = %+v, want {OnlyA,2}", got[0])
	}
}

// TestFacetsQFilter: q narrows the returned values (accent/case handled by the
// default LIKE matcher — the test stays ASCII-lowercase to be dialect-safe).
func TestFacetsQFilter(t *testing.T) {
	db := setupTestDB(t)
	svc := setupService(t, db)
	user := newUser(uuid.New())

	createProduct(t, svc, user, "alpha", 1)
	createProduct(t, svc, user, "alphabet", 2)
	createProduct(t, svc, user, "beta", 3)

	got, err := svc.Facets(context.Background(), user, FacetsQuery{
		Model: "test_products",
		Field: "name",
		Q:     "alph",
	})
	if err != nil {
		t.Fatalf("facets: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("q filter: got %d buckets, want 2 (%+v)", len(got), got)
	}
	for _, b := range got {
		if b.Value != "alpha" && b.Value != "alphabet" {
			t.Errorf("unexpected bucket %+v", b)
		}
	}
}

// TestFacetsExcludesEmptyAndNull: NULL and empty-string values are not bucketed.
func TestFacetsExcludesEmptyAndNull(t *testing.T) {
	db := setupTestDB(t)
	svc := setupService(t, db)
	user := newUser(uuid.New())

	createProduct(t, svc, user, "Real", 1)
	// Insert an empty-name and a NULL-name row directly (createProduct always
	// sets a name), scoped to the same org so they pass tenant scoping.
	db.Exec(`INSERT INTO test_products (id, organization_id, name, price) VALUES (?,?,?,?)`,
		uuid.NewString(), user.orgID.String(), "", 2)
	db.Exec(`INSERT INTO test_products (id, organization_id, name, price) VALUES (?,?,NULL,?)`,
		uuid.NewString(), user.orgID.String(), 3)

	got, err := svc.Facets(context.Background(), user, FacetsQuery{
		Model: "test_products",
		Field: "name",
	})
	if err != nil {
		t.Fatalf("facets: %v", err)
	}
	if len(got) != 1 || got[0].Value != "Real" {
		t.Fatalf("empty/null not excluded: %+v", got)
	}
}

// TestFacetsLimit: default clamp and explicit limit are honoured.
func TestFacetsLimit(t *testing.T) {
	db := setupTestDB(t)
	svc := setupService(t, db)
	user := newUser(uuid.New())

	for i := 0; i < 10; i++ {
		createProduct(t, svc, user, "name-"+uuid.NewString(), float64(i))
	}
	got, err := svc.Facets(context.Background(), user, FacetsQuery{
		Model: "test_products",
		Field: "name",
		Limit: 3,
	})
	if err != nil {
		t.Fatalf("facets: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("limit not applied: got %d, want 3", len(got))
	}

	over, _ := svc.Facets(context.Background(), user, FacetsQuery{
		Model: "test_products",
		Field: "name",
		Limit: 10_000,
	})
	if len(over) > MaxOptionsLimit {
		t.Fatalf("clamp failed: got %d, want <= %d", len(over), MaxOptionsLimit)
	}
}

// TestFacetsErrors: validation failures map to the same sentinel errors the
// handler translates to 400/404.
func TestFacetsErrors(t *testing.T) {
	db := setupTestDB(t)
	svc := setupService(t, db)
	user := newUser(uuid.New())

	// Missing field → ErrFieldRequired (400).
	if _, err := svc.Facets(context.Background(), user, FacetsQuery{Model: "test_products"}); err != ErrFieldRequired {
		t.Fatalf("want ErrFieldRequired, got %v", err)
	}

	// Unsafe column → ErrInvalidInput (400).
	if _, err := svc.Facets(context.Background(), user, FacetsQuery{
		Model: "test_products", Field: "name; DROP TABLE",
	}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("want ErrInvalidInput, got %v", err)
	}

	// Nonexistent column → ErrOptionsFieldNotFound (404).
	if _, err := svc.Facets(context.Background(), user, FacetsQuery{
		Model: "test_products", Field: "nonexistent",
	}); err != ErrOptionsFieldNotFound {
		t.Fatalf("want ErrOptionsFieldNotFound, got %v", err)
	}

	// Unknown model → ErrModelNotFound (404).
	if _, err := svc.Facets(context.Background(), user, FacetsQuery{
		Model: "not_registered", Field: "name",
	}); err != ErrModelNotFound {
		t.Fatalf("want ErrModelNotFound, got %v", err)
	}
}
