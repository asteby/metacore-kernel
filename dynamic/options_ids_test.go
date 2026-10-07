package dynamic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"

	"github.com/asteby/metacore-kernel/modelbase"
)

// Resolve mode (?ids=): a picker editing a saved value asks for exactly the
// options it needs a label for, without opening its popover.

func categoriesOptionsService(t *testing.T) (*Service, uuid.UUID, map[string]string) {
	t.Helper()
	db := setupTestDB(t)
	setupCategoriesTable(t, db)
	orgA, orgB := uuid.New(), uuid.New()
	ids := map[string]string{
		"a1": uuid.NewString(), "a2": uuid.NewString(), "a3": uuid.NewString(),
		"b1": uuid.NewString(), "gone": uuid.NewString(),
	}
	if err := db.Exec(`INSERT INTO test_categories (id, organization_id, name, color, deleted_at) VALUES
		(?, ?, 'Alpha', 'red', NULL),
		(?, ?, 'Beta', 'blue', NULL),
		(?, ?, 'Gamma', 'green', NULL),
		(?, ?, 'Foreign', 'black', NULL),
		(?, ?, 'Deleted', 'grey', '2026-01-01 00:00:00')`,
		ids["a1"], orgA, ids["a2"], orgA, ids["a3"], orgA, ids["b1"], orgB, ids["gone"], orgA).Error; err != nil {
		t.Fatal(err)
	}
	svc := newOptionsService(t, db, optionsConfigFor(OptionsConfig{
		Fields: map[string]FieldOptionsConfig{
			"category_id": {Type: "dynamic", Source: "test_categories", Value: "id", Label: "name"},
			"status": {Type: "static", Options: []StaticOption{
				{Value: "active", Label: "Active"},
				{Value: "inactive", Label: "Inactive"},
				{Value: "archived", Label: "Archived"},
			}},
		},
	}), nil)
	return svc, orgA, ids
}

func optionLabels(opts []Option) []string {
	out := make([]string, 0, len(opts))
	for _, o := range opts {
		out = append(out, fmt.Sprint(o.Label))
	}
	sort.Strings(out)
	return out
}

func TestOptionsIDs_ReturnsExactlyThoseScoped(t *testing.T) {
	svc, orgA, ids := categoriesOptionsService(t)
	user := newUser(orgA)
	res, err := svc.Options(context.Background(), user, OptionsQuery{
		Model: "test_products", Field: "category_id",
		// q, limit and offset must not narrow resolve mode.
		Q: "zzz", Limit: 1, Offset: 5,
		IDs: []string{
			ids["a1"], strings.ToUpper(ids["a2"]), ids["a1"], // dup + upper-case
			ids["b1"],   // other org
			ids["gone"], // soft-deleted
			"not-a-uuid", uuid.NewString(),
		},
	})
	if err != nil {
		t.Fatalf("options ids: %v", err)
	}
	got := optionLabels(res.Options)
	if strings.Join(got, ",") != "Alpha,Beta" {
		t.Fatalf("labels = %v, want [Alpha Beta] (no Gamma, no foreign org, no soft-deleted)", got)
	}
	for _, o := range res.Options {
		if o.Color == nil || o.Color == "" {
			t.Errorf("projection lost color: %+v", o)
		}
	}
}

func TestOptionsIDs_OnlyMalformedIDsIsEmptyNotError(t *testing.T) {
	svc, orgA, _ := categoriesOptionsService(t)
	res, err := svc.Options(context.Background(), newUser(orgA), OptionsQuery{
		Model: "test_products", Field: "category_id", IDs: []string{"nope"},
	})
	if err != nil {
		t.Fatalf("options ids: %v", err)
	}
	if len(res.Options) != 0 {
		t.Fatalf("want 0 options, got %d", len(res.Options))
	}
}

func TestOptionsIDs_Static(t *testing.T) {
	svc, _, _ := categoriesOptionsService(t)
	res, err := svc.Options(context.Background(), nil, OptionsQuery{
		Model: "test_products", Field: "status", IDs: []string{"archived", "active", "missing"},
	})
	if err != nil {
		t.Fatalf("options ids: %v", err)
	}
	if got := optionLabels(res.Options); strings.Join(got, ",") != "Active,Archived" {
		t.Fatalf("labels = %v", got)
	}
}

func TestOptionsIDs_TooMany(t *testing.T) {
	svc, orgA, _ := categoriesOptionsService(t)
	many := make([]string, MaxOptionsIDs+1)
	for i := range many {
		many[i] = uuid.NewString()
	}
	_, err := svc.Options(context.Background(), newUser(orgA), OptionsQuery{
		Model: "test_products", Field: "category_id", IDs: many,
	})
	if err != ErrInvalidInput {
		t.Fatalf("err = %v, want ErrInvalidInput", err)
	}
}

// idsAccessModel is listed only by holders of a capability the test user lacks.
type idsAccessModel struct {
	modelbase.BaseUUIDModel
	Name string `json:"name"`
}

func (idsAccessModel) TableName() string                    { return "test_ids_access" }
func (idsAccessModel) DefineTable() modelbase.TableMetadata { return modelbase.TableMetadata{} }
func (idsAccessModel) DefineModal() modelbase.ModalMetadata { return modelbase.ModalMetadata{} }
func (idsAccessModel) DefineAccess() modelbase.AccessPolicy {
	return modelbase.AccessPolicy{Default: modelbase.AccessCapabilities("ids_access.view")}
}

func TestOptionsIDs_RespectsAccessPolicy(t *testing.T) {
	db := setupTestDB(t)
	if err := db.Exec(`CREATE TABLE test_ids_access (id TEXT PRIMARY KEY, organization_id TEXT, created_by_id TEXT, created_at DATETIME, updated_at DATETIME, deleted_at DATETIME, name TEXT)`).Error; err != nil {
		t.Fatal(err)
	}
	modelbase.Register("test_ids_access", func() modelbase.ModelDefiner { return &idsAccessModel{} })
	org := uuid.New()
	id := uuid.NewString()
	db.Exec(`INSERT INTO test_ids_access (id, organization_id, name) VALUES (?, ?, 'Secret')`, id, org)
	svc := newOptionsService(t, db, noOptionsConfig(), nil)
	svc.selfOptions = true

	_, err := svc.Options(context.Background(), newUser(org), OptionsQuery{
		Model: "test_ids_access", Field: "id", IDs: []string{id},
	})
	var denied *AccessDeniedError
	if !errors.As(err, &denied) {
		t.Fatalf("err = %v, want AccessDeniedError", err)
	}
}

// A cross-addon ref arrives addon-qualified ("quotes.Quote"); the host's model
// resolver maps it. Resolve mode must work through that path too.
func TestOptionsIDs_QualifiedRef(t *testing.T) {
	db := setupTestDB(t)
	setupCategoriesTable(t, db)
	org := uuid.New()
	q1, q2 := uuid.NewString(), uuid.NewString()
	db.Exec(`INSERT INTO test_categories (id, organization_id, name) VALUES (?, ?, 'COT-001'), (?, ?, 'COT-002')`, q1, org, q2, org)
	svc := newOptionsService(t, db, noOptionsConfig(), nil)
	svc.selfOptions = true
	svc.modelResolver = func(_ context.Context, name string) (any, bool) {
		if name == "quotes.Quote" {
			return &TestCategory{}, true
		}
		return modelbase.Get(name)
	}
	res, err := svc.Options(context.Background(), newUser(org), OptionsQuery{
		Model: "quotes.Quote", Field: "id", IDs: []string{q2},
	})
	if err != nil {
		t.Fatalf("qualified ids: %v", err)
	}
	if len(res.Options) != 1 || fmt.Sprint(res.Options[0].Label) != "COT-002" {
		t.Fatalf("options = %+v, want only COT-002", res.Options)
	}
}

func TestOptionsIDs_HandlerParsesCommaAndRepeated(t *testing.T) {
	svc, _, ids := categoriesOptionsService(t)
	app := fiber.New()
	NewHandler(svc, nil).MountOptions(app)

	get := func(url string) (int, []any) {
		resp, err := app.Test(httptest.NewRequest("GET", url, nil), fiber.TestConfig{Timeout: 0})
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		var env map[string]any
		_ = json.Unmarshal(raw, &env)
		data, _ := env["data"].([]any)
		return resp.StatusCode, data
	}

	// No user → unscoped (org filter off), so every org row is resolvable;
	// the soft-deleted one never is.
	status, data := get(fmt.Sprintf("/options/test_products?field=category_id&ids=%s,%s&ids=%s&ids=%s",
		ids["a1"], ids["a2"], ids["a3"], ids["gone"]))
	if status != 200 || len(data) != 3 {
		t.Fatalf("status=%d len=%d, want 200/3", status, len(data))
	}

	many := make([]string, MaxOptionsIDs+1)
	for i := range many {
		many[i] = uuid.NewString()
	}
	if status, _ := get("/options/test_products?field=category_id&ids=" + strings.Join(many, ",")); status != 400 {
		t.Fatalf("too many ids status = %d, want 400", status)
	}
}
