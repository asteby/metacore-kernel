package dynamic

import (
	"context"
	"reflect"
	"testing"

	"github.com/google/uuid"

	"github.com/asteby/metacore-kernel/metadata"
	"github.com/asteby/metacore-kernel/modelbase"
)

type dynMovesMeta struct {
	modelbase.BaseUUIDModel
	Note string `json:"note"`
}

func (dynMovesMeta) TableName() string { return "dyn_moves" }
func (dynMovesMeta) DefineTable() modelbase.TableMetadata {
	return modelbase.TableMetadata{Title: "Dyn Moves", Columns: []modelbase.ColumnDef{{Key: "note", Label: "Note"}}}
}
func (dynMovesMeta) DefineModal() modelbase.ModalMetadata { return modelbase.ModalMetadata{Title: "Dyn Move"} }

// TestUnsentNullableText_StaysNull is the QA 0922 VEN-N02 regression: a
// nullable text column the caller does not send (idempotency_key on a manual
// drawer movement) must persist as NULL — not "" — on Create AND survive an
// Update untouched. With "" the partial unique index
// `(organization_id, idempotency_key) WHERE idempotency_key IS NOT NULL` admits
// ONE such row per org and 500s the second.
func TestUnsentNullableText_StaysNull(t *testing.T) {
	db := setupTestDB(t)
	db.Exec(`CREATE TABLE dyn_moves (
		id TEXT PRIMARY KEY,
		organization_id TEXT,
		created_by_id TEXT,
		created_at DATETIME,
		idempotency_key TEXT,
		kind TEXT NOT NULL,
		note TEXT
	)`)
	db.Exec(`CREATE UNIQUE INDEX dyn_moves_idem_uq ON dyn_moves (organization_id, idempotency_key) WHERE idempotency_key IS NOT NULL`)

	modelbase.Register("dyn_moves", func() modelbase.ModelDefiner { return &dynMovesMeta{} })
	dynType := reflect.StructOf([]reflect.StructField{
		{Name: "ID", Type: reflect.TypeOf(""), Tag: `json:"id" gorm:"column:id;primaryKey"`},
		{Name: "OrganizationID", Type: reflect.TypeOf(""), Tag: `json:"organization_id" gorm:"column:organization_id"`},
		{Name: "CreatedByID", Type: reflect.TypeOf(""), Tag: `json:"created_by_id" gorm:"column:created_by_id"`},
		{Name: "IdempotencyKey", Type: reflect.TypeOf(""), Tag: `json:"idempotency_key" gorm:"column:idempotency_key;type:text"`},
		{Name: "Kind", Type: reflect.TypeOf(""), Tag: `json:"kind" gorm:"column:kind;type:text"`},
		{Name: "Note", Type: reflect.TypeOf(""), Tag: `json:"note" gorm:"column:note;type:text"`},
	})
	svc := New(Config{
		DB:       db,
		Metadata: metadata.New(metadata.Config{CacheTTL: -1}),
		ModelResolver: func(_ context.Context, name string) (any, bool) {
			if name != "dyn_moves" {
				return nil, false
			}
			return reflect.New(dynType).Interface(), true
		},
		TableNameResolver: func(_ context.Context, name string) (string, bool) {
			return "dyn_moves", name == "dyn_moves"
		},
	})
	user := newUser(uuid.New())
	ctx := context.Background()

	ids := []uuid.UUID{uuid.New(), uuid.New()}
	for i, id := range ids {
		// Two hand-entered movements in the same org, no idempotency_key sent.
		if _, err := svc.Create(ctx, "dyn_moves", user, map[string]any{
			"id":   id.String(),
			"note": "manual",
		}); err != nil {
			t.Fatalf("create #%d without idempotency_key: %v", i+1, err)
		}
	}

	var nulls int64
	db.Raw(`SELECT count(*) FROM dyn_moves WHERE idempotency_key IS NULL`).Scan(&nulls)
	if nulls != 2 {
		t.Fatalf("rows with NULL idempotency_key = %d, want 2 (unset text must not persist as '')", nulls)
	}
	// A NOT NULL text column the caller did not send keeps the historical ''.
	var emptyKind int64
	db.Raw(`SELECT count(*) FROM dyn_moves WHERE kind = ''`).Scan(&emptyKind)
	if emptyKind != 2 {
		t.Fatalf("NOT NULL kind rows with '' = %d, want 2", emptyKind)
	}

	// Update of another column must not rewrite the NULL as ''.
	if _, err := svc.Update(ctx, "dyn_moves", user, ids[0], map[string]any{"note": "edited"}); err != nil {
		t.Fatalf("update: %v", err)
	}
	if _, err := svc.Update(ctx, "dyn_moves", user, ids[1], map[string]any{"note": "edited"}); err != nil {
		t.Fatalf("update of the second row (would collide on ''): %v", err)
	}
	db.Raw(`SELECT count(*) FROM dyn_moves WHERE idempotency_key IS NULL AND note = 'edited'`).Scan(&nulls)
	if nulls != 2 {
		t.Fatalf("after update NULL idempotency_key rows = %d, want 2", nulls)
	}

	// A value the caller sends — including an explicit "" — is written as-is.
	id3 := uuid.New()
	if _, err := svc.Create(ctx, "dyn_moves", user, map[string]any{
		"id":              id3.String(),
		"idempotency_key": "k-1",
		"note":            "",
	}); err != nil {
		t.Fatalf("create with key: %v", err)
	}
	var key, note *string
	db.Raw(`SELECT idempotency_key FROM dyn_moves WHERE id = ?`, id3.String()).Scan(&key)
	db.Raw(`SELECT note FROM dyn_moves WHERE id = ?`, id3.String()).Scan(&note)
	if key == nil || *key != "k-1" {
		t.Fatalf("idempotency_key = %v, want k-1", key)
	}
	if note == nil || *note != "" {
		t.Fatalf("explicit empty note = %v, want ''", note)
	}
}
