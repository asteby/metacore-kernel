package dynamic

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/asteby/metacore-kernel/modelbase"
)

// TestCategory is a second model used to exercise dynamic-source options
// (products.category_id → categories.id).
type TestCategory struct {
	modelbase.BaseUUIDModel
	Name  string `json:"name" gorm:"size:120"`
	Color string `json:"color"`
}

func (TestCategory) TableName() string { return "test_categories" }
func (TestCategory) DefineTable() modelbase.TableMetadata {
	return modelbase.TableMetadata{Title: "Test Categories"}
}
func (TestCategory) DefineModal() modelbase.ModalMetadata {
	return modelbase.ModalMetadata{Title: "Test Category"}
}

func setupCategoriesTable(t *testing.T, db *gorm.DB) {
	t.Helper()
	db.Exec(`CREATE TABLE IF NOT EXISTS test_categories (
		id TEXT PRIMARY KEY,
		organization_id TEXT,
		created_by_id TEXT,
		created_at DATETIME,
		updated_at DATETIME,
		deleted_at DATETIME,
		name TEXT,
		color TEXT
	)`)
	modelbase.Register("test_categories", func() modelbase.ModelDefiner { return &TestCategory{} })
}

func TestOptionsStatic(t *testing.T) {
	db := setupTestDB(t)
	svc := newOptionsService(t, db, optionsConfigFor(OptionsConfig{
		Fields: map[string]FieldOptionsConfig{
			"status": {
				Type: "static",
				Options: []StaticOption{
					{Value: "active", Label: "Active", Color: "green"},
					{Value: "inactive", Label: "Inactive"},
				},
			},
		},
	}), nil)

	res, err := svc.Options(context.Background(), nil, OptionsQuery{
		Model: "test_products",
		Field: "status",
	})
	if err != nil {
		t.Fatalf("options: %v", err)
	}
	if res.Type != "static" {
		t.Fatalf("type = %q, want static", res.Type)
	}
	if len(res.Options) != 2 {
		t.Fatalf("len = %d, want 2", len(res.Options))
	}
	if res.Options[0].Value != "active" || res.Options[0].Color != "green" {
		t.Fatalf("first opt = %+v", res.Options[0])
	}
}

func TestOptionsDynamicWithQFilter(t *testing.T) {
	db := setupTestDB(t)
	setupCategoriesTable(t, db)

	// Seed categories with a mix of names.
	now := uuid.New().String()
	_ = now
	db.Exec(`INSERT INTO test_categories (id, name, color) VALUES
		(?, 'Alpha', 'red'),
		(?, 'Alphabet', 'blue'),
		(?, 'Beta', 'green')`,
		uuid.NewString(), uuid.NewString(), uuid.NewString())

	svc := newOptionsService(t, db, optionsConfigFor(OptionsConfig{
		Fields: map[string]FieldOptionsConfig{
			"category_id": {
				Type:   "dynamic",
				Source: "test_categories",
				Value:  "id",
				Label:  "name",
			},
		},
	}), nil)

	res, err := svc.Options(context.Background(), nil, OptionsQuery{
		Model: "test_products",
		Field: "category_id",
		Q:     "alph",
	})
	if err != nil {
		t.Fatalf("options: %v", err)
	}
	if res.Type != "dynamic" {
		t.Fatalf("type = %q, want dynamic", res.Type)
	}
	if len(res.Options) != 2 {
		t.Fatalf("expected 2 matches for 'alph', got %d", len(res.Options))
	}
	for _, o := range res.Options {
		label, _ := o.Label.(string)
		if label != "Alpha" && label != "Alphabet" {
			t.Errorf("unexpected label %q", label)
		}
	}
}

func TestOptionsLimitRespected(t *testing.T) {
	db := setupTestDB(t)
	setupCategoriesTable(t, db)
	for i := 0; i < 10; i++ {
		db.Exec(`INSERT INTO test_categories (id, name) VALUES (?, ?)`,
			uuid.NewString(), "Cat-")
	}
	svc := newOptionsService(t, db, optionsConfigFor(OptionsConfig{
		Fields: map[string]FieldOptionsConfig{
			"category_id": {Type: "dynamic", Source: "test_categories", Label: "name"},
		},
	}), nil)

	res, err := svc.Options(context.Background(), nil, OptionsQuery{
		Model: "test_products",
		Field: "category_id",
		Limit: 3,
	})
	if err != nil {
		t.Fatalf("options: %v", err)
	}
	if len(res.Options) != 3 {
		t.Fatalf("limit not applied: got %d", len(res.Options))
	}
	// Over-limit clamp.
	res, _ = svc.Options(context.Background(), nil, OptionsQuery{
		Model: "test_products",
		Field: "category_id",
		Limit: 10_000,
	})
	if got := len(res.Options); got > MaxOptionsLimit {
		t.Fatalf("Limit clamp failed: got %d", got)
	}
}

func TestOptionsErrors(t *testing.T) {
	db := setupTestDB(t)

	// No resolver → ErrNoOptionsConfig
	svcNoResolver := setupService(t, db)
	if _, err := svcNoResolver.Options(context.Background(), nil, OptionsQuery{
		Model: "test_products", Field: "status",
	}); err != ErrNoOptionsConfig {
		t.Fatalf("want ErrNoOptionsConfig, got %v", err)
	}

	// Missing field → ErrFieldRequired
	svc := newOptionsService(t, db, optionsConfigFor(OptionsConfig{
		Fields: map[string]FieldOptionsConfig{},
	}), nil)
	if _, err := svc.Options(context.Background(), nil, OptionsQuery{Model: "test_products"}); err != ErrFieldRequired {
		t.Fatalf("want ErrFieldRequired, got %v", err)
	}

	// Field not in config → ErrOptionsFieldNotFound
	if _, err := svc.Options(context.Background(), nil, OptionsQuery{
		Model: "test_products", Field: "missing",
	}); err != ErrOptionsFieldNotFound {
		t.Fatalf("want ErrOptionsFieldNotFound, got %v", err)
	}

	// Model unknown → ErrModelNotFound
	if _, err := svc.Options(context.Background(), nil, OptionsQuery{
		Model: "not_registered", Field: "foo",
	}); err != ErrModelNotFound {
		t.Fatalf("want ErrModelNotFound, got %v", err)
	}
}

// --- helpers ---------------------------------------------------------------

func optionsConfigFor(cfg OptionsConfig) OptionsConfigResolver {
	return func(context.Context, string, any) (*OptionsConfig, error) {
		return &cfg, nil
	}
}

func newOptionsService(t *testing.T, db *gorm.DB, optsResolver OptionsConfigResolver, searchResolver SearchConfigResolver) *Service {
	t.Helper()
	svc := setupService(t, db)
	svc.optsResolver = optsResolver
	svc.searchResolver = searchResolver
	return svc
}
