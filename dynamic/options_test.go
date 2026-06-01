package dynamic

import (
	"context"
	"reflect"
	"testing"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/asteby/metacore-kernel/modelbase"
)

// orgScopedReflectLike simulates a reflect-built addon model: the org column
// maps to a Go field whose name is NOT "OrganizationID" (the reflect builder
// derives "OrganizationId" from the snake_case column), so the legacy
// FieldByName("OrganizationID") tenant-scope gate would miss it and leak other
// orgs' rows. hasOrgColumn (column-based) detects it correctly.
type orgScopedReflectLike struct {
	ID    uuid.UUID `json:"id" gorm:"type:uuid;primaryKey"`
	OrgID uuid.UUID `json:"organization_id" gorm:"type:uuid"`
	Name  string    `json:"name"`
}

func (orgScopedReflectLike) TableName() string { return "test_org_scoped" }
func (orgScopedReflectLike) DefineTable() modelbase.TableMetadata {
	return modelbase.TableMetadata{Title: "Org Scoped"}
}
func (orgScopedReflectLike) DefineModal() modelbase.ModalMetadata {
	return modelbase.ModalMetadata{Title: "Org Scoped"}
}

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

// noOptionsConfig mimics a host whose resolver finds nothing for the model —
// the exact condition the self-referential fallback rescues.
func noOptionsConfig() OptionsConfigResolver {
	return func(context.Context, string, any) (*OptionsConfig, error) {
		return nil, ErrNoOptionsConfig
	}
}

// TestSelfReferentialOptions: with EnableSelfOptions on and NO declared config
// for the identity field, Options lists the model's own rows — value=id,
// label auto-derived from the first name-like column ("name"). This is the
// zero-config dynamic_select picker.
func TestSelfReferentialOptions(t *testing.T) {
	db := setupTestDB(t)
	db.Exec(`INSERT INTO test_products (id, name, price) VALUES
		(?, 'Brake Pad', 10),
		(?, 'Brake Disc', 20),
		(?, 'Oil Filter', 5)`,
		uuid.NewString(), uuid.NewString(), uuid.NewString())

	svc := newOptionsService(t, db, noOptionsConfig(), nil)
	svc.selfOptions = true

	res, err := svc.Options(context.Background(), nil, OptionsQuery{
		Model: "test_products",
		Field: "id",
		Q:     "brake",
	})
	if err != nil {
		t.Fatalf("self options: %v", err)
	}
	if res.Type != "dynamic" {
		t.Fatalf("type = %q, want dynamic", res.Type)
	}
	if len(res.Options) != 2 {
		t.Fatalf("expected 2 'brake' matches, got %d", len(res.Options))
	}
	for _, o := range res.Options {
		label, _ := o.Label.(string)
		if label != "Brake Pad" && label != "Brake Disc" {
			t.Errorf("unexpected label %q", label)
		}
		if o.ID == nil || o.ID == "" {
			t.Errorf("self option missing id value: %+v", o)
		}
	}
}

// TestSelfOptionsDisabledByDefault: without the opt-in, an unconfigured field
// still fails loudly (no surprise behaviour change for existing hosts).
func TestSelfOptionsDisabledByDefault(t *testing.T) {
	db := setupTestDB(t)
	svc := newOptionsService(t, db, noOptionsConfig(), nil) // selfOptions stays false
	if _, err := svc.Options(context.Background(), nil, OptionsQuery{
		Model: "test_products", Field: "id",
	}); err != ErrNoOptionsConfig {
		t.Fatalf("want ErrNoOptionsConfig, got %v", err)
	}
}

// TestSelfOptionsDoesNotShadowDeclared: a declared static enum (order status)
// keeps winning even with the fallback enabled.
func TestSelfOptionsDoesNotShadowDeclared(t *testing.T) {
	db := setupTestDB(t)
	svc := newOptionsService(t, db, optionsConfigFor(OptionsConfig{
		Fields: map[string]FieldOptionsConfig{
			"status": {Type: "static", Options: []StaticOption{{Value: "draft", Label: "Draft"}}},
		},
	}), nil)
	svc.selfOptions = true

	res, err := svc.Options(context.Background(), nil, OptionsQuery{Model: "test_products", Field: "status"})
	if err != nil {
		t.Fatalf("static with fallback on: %v", err)
	}
	if res.Type != "static" || len(res.Options) != 1 || res.Options[0].Value != "draft" {
		t.Fatalf("declared static config was shadowed: %+v", res)
	}
}

// TestSelfOptionsOnlyIdentityField: the fallback fires only for "id" — an
// unconfigured non-identity field must still surface ErrOptionsFieldNotFound so
// a manifest bug (FK target not declared) is not silently self-listed.
func TestSelfOptionsOnlyIdentityField(t *testing.T) {
	db := setupTestDB(t)
	svc := newOptionsService(t, db, optionsConfigFor(OptionsConfig{
		Fields: map[string]FieldOptionsConfig{},
	}), nil)
	svc.selfOptions = true

	if _, err := svc.Options(context.Background(), nil, OptionsQuery{
		Model: "test_products", Field: "category_id",
	}); err != ErrOptionsFieldNotFound {
		t.Fatalf("want ErrOptionsFieldNotFound for non-id field, got %v", err)
	}
}

// TestDeriveLabelColumn covers the preference walk and the id fallback.
func TestDeriveLabelColumn(t *testing.T) {
	if got := deriveLabelColumn(&TestProduct{}); got != "name" {
		t.Fatalf("TestProduct label = %q, want name", got)
	}
	// A struct with no name-like column falls back to id.
	type onlyAmount struct {
		modelbase.BaseUUIDModel
		Amount float64 `json:"amount"`
	}
	if got := deriveLabelColumn(&onlyAmount{}); got != "id" {
		t.Fatalf("onlyAmount label = %q, want id fallback", got)
	}
	// `code` is honoured when present and no higher-priority column exists.
	type withCode struct {
		modelbase.BaseUUIDModel
		Code string `json:"code"`
	}
	if got := deriveLabelColumn(&withCode{}); got != "code" {
		t.Fatalf("withCode label = %q, want code", got)
	}
}

// TestSelfOptionsTenantScoped: the self-referential picker must NOT leak rows
// across organizations. Regression for reflect-built models whose org field is
// "OrganizationId" (not "OrganizationID"), which the legacy field-name gate
// missed — returning every tenant's rows.
func TestSelfOptionsTenantScoped(t *testing.T) {
	db := setupTestDB(t)
	db.Exec(`CREATE TABLE IF NOT EXISTS test_org_scoped (
		id TEXT PRIMARY KEY, organization_id TEXT, name TEXT)`)
	modelbase.Register("test_org_scoped", func() modelbase.ModelDefiner { return &orgScopedReflectLike{} })

	orgA := uuid.New()
	orgB := uuid.New()
	db.Exec(`INSERT INTO test_org_scoped (id, organization_id, name) VALUES (?,?,?),(?,?,?),(?,?,?)`,
		uuid.NewString(), orgA.String(), "A-Journal",
		uuid.NewString(), orgB.String(), "B-Journal",
		uuid.NewString(), orgA.String(), "A-Journal-2")

	svc := newOptionsService(t, db, noOptionsConfig(), nil)
	svc.selfOptions = true

	user := &fakeUser{id: uuid.New(), orgID: orgA}
	res, err := svc.Options(context.Background(), user, OptionsQuery{Model: "test_org_scoped", Field: "id"})
	if err != nil {
		t.Fatalf("scoped self options: %v", err)
	}
	if len(res.Options) != 2 {
		t.Fatalf("tenant scope leaked: got %d options, want 2 (org A only)", len(res.Options))
	}
	for _, o := range res.Options {
		if label, _ := o.Label.(string); label == "B-Journal" {
			t.Errorf("cross-tenant leak: org B row returned in org A's picker")
		}
	}
}

// TestHasOrgColumn locks the column-based detection that the scoping relies on.
func TestHasOrgColumn(t *testing.T) {
	if !hasOrgColumn(&TestProduct{}) {
		t.Error("compiled model (BaseUUIDModel) should be detected as org-scoped")
	}
	if !hasOrgColumn(&orgScopedReflectLike{}) {
		t.Error("reflect-like model should be detected as org-scoped via its column")
	}
	// Premise: the field is genuinely NOT named OrganizationID, so the legacy
	// field-name gate would have missed it.
	if _, ok := reflect.TypeOf(orgScopedReflectLike{}).FieldByName("OrganizationID"); ok {
		t.Error("test premise broken: field is literally named OrganizationID")
	}
	type noOrg struct {
		ID   uuid.UUID `json:"id"`
		Name string    `json:"name"`
	}
	if hasOrgColumn(&noOrg{}) {
		t.Error("a model without organization_id must not be treated as org-scoped")
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
