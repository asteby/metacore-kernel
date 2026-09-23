package dynamic

import (
	"context"
	"errors"
	"testing"

	"github.com/asteby/metacore-kernel/manifest"
	"github.com/google/uuid"
)

// QA 7Leguas VEN-N08: a sale line for a soft-deleted product went through.
// A CREATE must reject a ref to a soft-deleted row; an UPDATE keeps an old
// link working.
func TestValidate_CreateRejectsSoftDeletedRef(t *testing.T) {
	db := setupValidationDB(t)
	svc := validationService(t, db)
	org := uuid.New()
	user := newUser(org)
	cat := uuid.NewString()
	if err := db.Exec(`INSERT INTO val_categories (id, organization_id, name, deleted_at) VALUES (?, ?, 'old', CURRENT_TIMESTAMP)`, cat, org.String()).Error; err != nil {
		t.Fatal(err)
	}
	_, err := svc.Create(context.Background(), "val_items", user, map[string]any{"name": "W", "category_id": cat})
	if !hasCode(fieldCodes(t, err, "category_id"), codeNotFound) {
		t.Fatalf("create pointing at a deleted category must be not_found, got %v", err)
	}
	// The same id is still a valid link on update (PATCH of an old record).
	ok, err := svc.refExists(context.Background(), user, "val_categories", cat, false)
	if err != nil || !ok {
		t.Fatalf("update-time ref to a deleted row must still resolve: ok=%v err=%v", ok, err)
	}
}

func TestCheckNoDeletedRefs(t *testing.T) {
	db := setupValidationDB(t)
	org := uuid.New()
	live, gone := uuid.NewString(), uuid.NewString()
	db.Exec(`INSERT INTO val_categories (id, organization_id, name) VALUES (?, ?, 'live')`, live, org.String())
	db.Exec(`INSERT INTO val_categories (id, organization_id, name, deleted_at) VALUES (?, ?, 'gone', CURRENT_TIMESTAMP)`, gone, org.String())
	cols := []manifest.ColumnDef{{Name: "category_id", Ref: "val_categories"}, {Name: "name"}}
	ctx := context.Background()

	if err := CheckNoDeletedRefs(ctx, db, org, cols, map[string]any{"category_id": live}); err != nil {
		t.Fatalf("live ref must pass: %v", err)
	}
	if err := CheckNoDeletedRefs(ctx, db, org, cols, map[string]any{"category_id": uuid.NewString()}); err != nil {
		t.Fatalf("an unknown id is not this check's business: %v", err)
	}
	err := CheckNoDeletedRefs(ctx, db, org, cols, map[string]any{"category_id": gone})
	if !errors.Is(err, ErrDeletedRef) {
		t.Fatalf("deleted ref must fail with ErrDeletedRef, got %v", err)
	}
	// Another tenant's deleted row with the same id is invisible.
	if err := CheckNoDeletedRefs(ctx, db, uuid.New(), cols, map[string]any{"category_id": gone}); err != nil {
		t.Fatalf("other org must not see this org's rows: %v", err)
	}
}
