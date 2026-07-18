package dynamic

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/asteby/metacore-kernel/manifest"
	"github.com/asteby/metacore-kernel/metadata"
	"github.com/asteby/metacore-kernel/modelbase"
)

// ---------------------------------------------------------------------------
// Fixtures: an item model with a ref, a unique column and a static enum, plus
// a category model it refs.
// ---------------------------------------------------------------------------

type ValCategory struct {
	modelbase.BaseUUIDModel
	Name string `json:"name" gorm:"size:255"`
}

func (ValCategory) TableName() string { return "val_categories" }
func (ValCategory) DefineTable() modelbase.TableMetadata {
	return modelbase.TableMetadata{Title: "Categories"}
}
func (ValCategory) DefineModal() modelbase.ModalMetadata {
	return modelbase.ModalMetadata{Title: "Category"}
}

type ValItem struct {
	modelbase.BaseUUIDModel
	Name       string     `json:"name" gorm:"size:255"`
	Sku        string     `json:"sku" gorm:"size:255"`
	Status     string     `json:"status" gorm:"size:64"`
	Qty        int        `json:"qty"`
	CategoryID *uuid.UUID `json:"category_id"`
}

func (ValItem) TableName() string { return "val_items" }
func (ValItem) DefineTable() modelbase.TableMetadata {
	return modelbase.TableMetadata{Title: "Items"}
}
func (ValItem) DefineModal() modelbase.ModalMetadata {
	return modelbase.ModalMetadata{Title: "Item"}
}

// valItemColumns is the declarative validation schema for val_items.
func valItemColumns() []manifest.ColumnDef {
	return []manifest.ColumnDef{
		{Name: "name", Type: "string", Required: true},
		{Name: "sku", Type: "string", Unique: true},
		{Name: "status", Type: "string", Options: []manifest.Option{
			{Value: "active"}, {Value: "inactive"},
		}},
		{Name: "qty", Type: "int"},
		{Name: "category_id", Type: "uuid", Ref: "val_categories"},
	}
}

func setupValidationDB(t *testing.T) *gorm.DB {
	t.Helper()
	db := setupTestDB(t) // reuse the sqlite in-memory harness + test_products
	db.Exec(`CREATE TABLE IF NOT EXISTS val_categories (
		id TEXT PRIMARY KEY, organization_id TEXT, created_by_id TEXT,
		created_at DATETIME, updated_at DATETIME, deleted_at DATETIME, name TEXT)`)
	db.Exec(`CREATE TABLE IF NOT EXISTS val_items (
		id TEXT PRIMARY KEY, organization_id TEXT, created_by_id TEXT,
		created_at DATETIME, updated_at DATETIME, deleted_at DATETIME,
		name TEXT, sku TEXT, status TEXT, qty INTEGER, category_id TEXT)`)
	return db
}

func validationService(t *testing.T, db *gorm.DB) *Service {
	t.Helper()
	modelbase.Register("val_items", func() modelbase.ModelDefiner { return &ValItem{} })
	modelbase.Register("val_categories", func() modelbase.ModelDefiner { return &ValCategory{} })
	meta := metadata.New(metadata.Config{CacheTTL: -1})
	return New(Config{
		DB:       db,
		Metadata: meta,
		ValidationSchemaResolver: func(_ context.Context, model string) ([]manifest.ColumnDef, bool) {
			if model == "val_items" {
				return valItemColumns(), true
			}
			return nil, false
		},
	})
}

// fieldCodes extracts the codes reported for a column from an error.
func fieldCodes(t *testing.T, err error, field string) []string {
	t.Helper()
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("want *ValidationError, got %v", err)
	}
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("error should wrap ErrValidation: %v", err)
	}
	var codes []string
	for _, fe := range ve.Fields[field] {
		codes = append(codes, fe.Code)
	}
	return codes
}

func hasCode(codes []string, code string) bool {
	for _, c := range codes {
		if c == code {
			return true
		}
	}
	return false
}

func TestValidate_MissingRequired(t *testing.T) {
	svc := validationService(t, setupValidationDB(t))
	user := newUser(uuid.New())
	_, err := svc.Create(context.Background(), "val_items", user, map[string]any{"sku": "A1"})
	if !hasCode(fieldCodes(t, err, "name"), codeRequired) {
		t.Fatalf("want required on name, got %v", err)
	}
}

func TestValidate_BadEnum(t *testing.T) {
	svc := validationService(t, setupValidationDB(t))
	user := newUser(uuid.New())
	_, err := svc.Create(context.Background(), "val_items", user, map[string]any{
		"name": "Widget", "status": "bogus",
	})
	if !hasCode(fieldCodes(t, err, "status"), codeInvalidOption) {
		t.Fatalf("want invalid_option on status, got %v", err)
	}
	var ve *ValidationError
	errors.As(err, &ve)
	allowed, _ := ve.Fields["status"][0].Params["allowed"].([]string)
	if len(allowed) != 2 {
		t.Fatalf("want 2 allowed options in params, got %v", ve.Fields["status"][0].Params)
	}
}

func TestValidate_RefNotFound(t *testing.T) {
	svc := validationService(t, setupValidationDB(t))
	user := newUser(uuid.New())
	_, err := svc.Create(context.Background(), "val_items", user, map[string]any{
		"name": "Widget", "category_id": uuid.New().String(),
	})
	if !hasCode(fieldCodes(t, err, "category_id"), codeNotFound) {
		t.Fatalf("want not_found on category_id, got %v", err)
	}
}

func TestValidate_EmptyRefPasses(t *testing.T) {
	svc := validationService(t, setupValidationDB(t))
	user := newUser(uuid.New())
	// Empty ref as "" and as the nil UUID must both pass (become null).
	for _, v := range []string{"", nilUUIDString} {
		if _, err := svc.Create(context.Background(), "val_items", user, map[string]any{
			"name": "Widget", "category_id": v,
		}); err != nil {
			t.Fatalf("empty ref %q should pass, got %v", v, err)
		}
	}
}

func TestValidate_DuplicateUnique(t *testing.T) {
	svc := validationService(t, setupValidationDB(t))
	user := newUser(uuid.New())
	if _, err := svc.Create(context.Background(), "val_items", user, map[string]any{
		"name": "One", "sku": "DUP",
	}); err != nil {
		t.Fatalf("first create: %v", err)
	}
	_, err := svc.Create(context.Background(), "val_items", user, map[string]any{
		"name": "Two", "sku": "DUP",
	})
	if !hasCode(fieldCodes(t, err, "sku"), codeDuplicate) {
		t.Fatalf("want duplicate on sku, got %v", err)
	}
}

func TestValidate_InvalidType(t *testing.T) {
	svc := validationService(t, setupValidationDB(t))
	user := newUser(uuid.New())
	_, err := svc.Create(context.Background(), "val_items", user, map[string]any{
		"name": "Widget", "qty": "not-a-number",
	})
	if !hasCode(fieldCodes(t, err, "qty"), codeInvalidType) {
		t.Fatalf("want invalid_type on qty, got %v", err)
	}
}

func TestValidate_ValidInputPasses(t *testing.T) {
	db := setupValidationDB(t)
	svc := validationService(t, db)
	user := newUser(uuid.New())

	// Seed a real category row in the tenant.
	catID := uuid.New()
	db.Exec(`INSERT INTO val_categories (id, organization_id, name) VALUES (?, ?, ?)`,
		catID.String(), user.GetOrganizationID().String(), "Tools")

	out, err := svc.Create(context.Background(), "val_items", user, map[string]any{
		"name": "Widget", "sku": "OK1", "status": "active",
		"qty": 5, "category_id": catID.String(),
	})
	if err != nil {
		t.Fatalf("valid input should pass, got %v", err)
	}
	if out["name"] != "Widget" {
		t.Fatalf("unexpected create result: %v", out)
	}
}

func TestValidate_UpdateExcludesSelfFromUnique(t *testing.T) {
	svc := validationService(t, setupValidationDB(t))
	user := newUser(uuid.New())
	created, err := svc.Create(context.Background(), "val_items", user, map[string]any{
		"name": "One", "sku": "SELF",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	id := uuid.MustParse(created["id"].(string))
	// Re-saving the same sku on the same row must NOT flag duplicate.
	if _, err := svc.Update(context.Background(), "val_items", user, id, map[string]any{
		"sku": "SELF", "status": "inactive",
	}); err != nil {
		t.Fatalf("self-update should pass, got %v", err)
	}
}
