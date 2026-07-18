package dynamic

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/asteby/metacore-kernel/metadata"
	"github.com/asteby/metacore-kernel/modelbase"
)

// The relation harness reuses TestProduct as the OWNER model and adds a plain
// pivot table `product_tags(product_id, tag_id)` — a classic many_to_many join.
// The pivot has no schema qualifier here (sqlite has no schemas) so PivotTable
// is the bare table name; in production the host passes the schema-qualified
// form.

func setupRelationDB(t *testing.T) *gorm.DB {
	t.Helper()
	db := setupTestDB(t)
	db.Exec(`CREATE TABLE IF NOT EXISTS product_tags (
		product_id TEXT NOT NULL,
		tag_id     TEXT NOT NULL
	)`)
	return db
}

func setupRelationService(t *testing.T, db *gorm.DB, rels []Relation) *Service {
	t.Helper()
	modelbase.Register("test_products", func() modelbase.ModelDefiner { return &TestProduct{} })
	meta := metadata.New(metadata.Config{CacheTTL: -1})
	return New(Config{
		DB:       db,
		Metadata: meta,
		RelationResolver: func(ctx context.Context, model string) ([]Relation, bool) {
			if model == "test_products" {
				return rels, true
			}
			return nil, false
		},
	})
}

var productTagsRel = Relation{
	Name:         "tags",
	Kind:         "many_to_many",
	PivotTable:   "product_tags",
	OwnerColumn:  "product_id",
	TargetColumn: "tag_id",
}

func pivotTagIDs(t *testing.T, db *gorm.DB, ownerID uuid.UUID) []string {
	t.Helper()
	var ids []string
	if err := db.Table("product_tags").
		Where("product_id = ?", ownerID).
		Order("tag_id").
		Pluck("tag_id", &ids).Error; err != nil {
		t.Fatalf("read pivot: %v", err)
	}
	return ids
}

func TestCreateSetsM2MPivot(t *testing.T) {
	db := setupRelationDB(t)
	svc := setupRelationService(t, db, []Relation{productTagsRel})
	user := newUser(uuid.New())

	tagA, tagB := uuid.New(), uuid.New()
	out, err := svc.Create(context.Background(), "test_products", user, map[string]any{
		"name": "Tagged",
		"tags": []any{tagA.String(), tagB.String()},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	ownerID, _ := uuid.Parse(out["id"].(string))

	got := pivotTagIDs(t, db, ownerID)
	if len(got) != 2 {
		t.Fatalf("pivot rows = %d, want 2 (%v)", len(got), got)
	}
	want := map[string]bool{tagA.String(): true, tagB.String(): true}
	for _, id := range got {
		if !want[id] {
			t.Fatalf("unexpected tag_id %s in pivot", id)
		}
	}
}

func TestCreateNoRelationKeyLeavesPivotEmpty(t *testing.T) {
	db := setupRelationDB(t)
	svc := setupRelationService(t, db, []Relation{productTagsRel})
	user := newUser(uuid.New())

	out := createProduct(t, svc, user, "Untagged", 1)
	ownerID, _ := uuid.Parse(out["id"].(string))
	if got := pivotTagIDs(t, db, ownerID); len(got) != 0 {
		t.Fatalf("expected no pivot rows, got %v", got)
	}
}

func TestUpdateReplacesM2MPivot(t *testing.T) {
	db := setupRelationDB(t)
	svc := setupRelationService(t, db, []Relation{productTagsRel})
	user := newUser(uuid.New())

	tagA, tagB, tagC := uuid.New(), uuid.New(), uuid.New()
	out, err := svc.Create(context.Background(), "test_products", user, map[string]any{
		"name": "P",
		"tags": []any{tagA.String(), tagB.String()},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	ownerID, _ := uuid.Parse(out["id"].(string))

	// Replace {A,B} with {C}.
	if _, err := svc.Update(context.Background(), "test_products", user, ownerID, map[string]any{
		"tags": []any{tagC.String()},
	}); err != nil {
		t.Fatalf("update: %v", err)
	}
	got := pivotTagIDs(t, db, ownerID)
	if len(got) != 1 || got[0] != tagC.String() {
		t.Fatalf("pivot after replace = %v, want [%s]", got, tagC.String())
	}
}

func TestUpdateWithoutRelationKeyLeavesPivotUntouched(t *testing.T) {
	db := setupRelationDB(t)
	svc := setupRelationService(t, db, []Relation{productTagsRel})
	user := newUser(uuid.New())

	tagA := uuid.New()
	out, err := svc.Create(context.Background(), "test_products", user, map[string]any{
		"name": "P",
		"tags": []any{tagA.String()},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	ownerID, _ := uuid.Parse(out["id"].(string))

	// PATCH a scalar only — the relation key is absent, so the pivot stays.
	if _, err := svc.Update(context.Background(), "test_products", user, ownerID, map[string]any{
		"name": "Renamed",
	}); err != nil {
		t.Fatalf("update: %v", err)
	}
	if got := pivotTagIDs(t, db, ownerID); len(got) != 1 || got[0] != tagA.String() {
		t.Fatalf("pivot should be untouched, got %v", got)
	}
}

func TestUpdateEmptyRelationClearsPivot(t *testing.T) {
	db := setupRelationDB(t)
	svc := setupRelationService(t, db, []Relation{productTagsRel})
	user := newUser(uuid.New())

	tagA := uuid.New()
	out, err := svc.Create(context.Background(), "test_products", user, map[string]any{
		"name": "P",
		"tags": []any{tagA.String()},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	ownerID, _ := uuid.Parse(out["id"].(string))

	if _, err := svc.Update(context.Background(), "test_products", user, ownerID, map[string]any{
		"tags": []any{},
	}); err != nil {
		t.Fatalf("update: %v", err)
	}
	if got := pivotTagIDs(t, db, ownerID); len(got) != 0 {
		t.Fatalf("expected cleared pivot, got %v", got)
	}
}

func TestDeleteCleansM2MPivot(t *testing.T) {
	db := setupRelationDB(t)
	svc := setupRelationService(t, db, []Relation{productTagsRel})
	user := newUser(uuid.New())

	tagA, tagB := uuid.New(), uuid.New()
	out, err := svc.Create(context.Background(), "test_products", user, map[string]any{
		"name": "Doomed",
		"tags": []any{tagA.String(), tagB.String()},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	ownerID, _ := uuid.Parse(out["id"].(string))
	if got := pivotTagIDs(t, db, ownerID); len(got) != 2 {
		t.Fatalf("precondition: expected 2 pivot rows, got %v", got)
	}

	if err := svc.Delete(context.Background(), "test_products", user, ownerID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if got := pivotTagIDs(t, db, ownerID); len(got) != 0 {
		t.Fatalf("pivot should be cleaned after delete, got %v", got)
	}
}

// A model with NO wired relations must behave exactly as before — the pivot
// codepath is entirely inert.
func TestNoRelationResolverIsInert(t *testing.T) {
	db := setupRelationDB(t)
	svc := setupService(t, db) // no RelationResolver
	user := newUser(uuid.New())

	out, err := svc.Create(context.Background(), "test_products", user, map[string]any{
		"name": "Plain",
		"tags": []any{uuid.New().String()}, // ignored — no resolver
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	ownerID, _ := uuid.Parse(out["id"].(string))
	if got := pivotTagIDs(t, db, ownerID); len(got) != 0 {
		t.Fatalf("expected no pivot writes without a resolver, got %v", got)
	}
}

func TestExtractRelationIDsShapes(t *testing.T) {
	a, b := uuid.New(), uuid.New()
	cases := []struct {
		name string
		in   any
		want int
	}{
		{"nil", nil, 0},
		{"[]any strings", []any{a.String(), b.String()}, 2},
		{"[]string", []string{a.String(), b.String()}, 2},
		{"[]uuid", []uuid.UUID{a, b}, 2},
		{"dedup", []any{a.String(), a.String()}, 1},
		{"drops garbage", []any{"not-a-uuid", a.String()}, 1},
		{"drops nil uuid", []uuid.UUID{uuid.Nil, a}, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := extractRelationIDs(tc.in); len(got) != tc.want {
				t.Fatalf("got %d ids, want %d", len(got), tc.want)
			}
		})
	}
}

func TestInvalidRelationSkipped(t *testing.T) {
	db := setupRelationDB(t)
	bad := Relation{Name: "tags", Kind: "many_to_many", PivotTable: "product_tags; DROP TABLE x", OwnerColumn: "product_id", TargetColumn: "tag_id"}
	svc := setupRelationService(t, db, []Relation{bad})
	user := newUser(uuid.New())

	out, err := svc.Create(context.Background(), "test_products", user, map[string]any{
		"name": "P",
		"tags": []any{uuid.New().String()},
	})
	if err != nil {
		t.Fatalf("create should not fail on a skipped invalid relation: %v", err)
	}
	ownerID, _ := uuid.Parse(out["id"].(string))
	if got := pivotTagIDs(t, db, ownerID); len(got) != 0 {
		t.Fatalf("invalid relation must be skipped, got %v", got)
	}
}
