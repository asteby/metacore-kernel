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

// tenantlessThing declares NO organization_id (BaseUUIDModel would add one).
type tenantlessThing struct {
	ID   uuid.UUID `json:"id" gorm:"primaryKey"`
	Code string    `json:"code"`
}

func (tenantlessThing) TableName() string { return "tenantless_things" }
func (tenantlessThing) DefineTable() modelbase.TableMetadata {
	return modelbase.TableMetadata{Title: "Tenantless", Columns: []modelbase.ColumnDef{{Key: "code", Label: "Code"}}}
}
func (tenantlessThing) DefineModal() modelbase.ModalMetadata {
	return modelbase.ModalMetadata{Title: "Tenantless"}
}

// P1-F1: with Config.RequireTenantScope, every tenant-scoped reader refuses a
// model that declares no organization_id (ErrTenantScopeUnavailable) instead
// of answering across every organization.
func TestRequireTenantScope_ModelWithoutOrgColumnIsRefused(t *testing.T) {
	db := setupTestDB(t)
	db.Exec(`CREATE TABLE tenantless_things (id TEXT PRIMARY KEY, code TEXT)`)
	modelbase.Register("tenantless_things", func() modelbase.ModelDefiner { return &tenantlessThing{} })

	build := func(require bool) *Service {
		svc := New(Config{
			AllowUnconfiguredPermissions: true, DB: db, RequireTenantScope: require,
			Metadata: metadata.New(metadata.Config{CacheTTL: -1}),
			OptionsConfigResolver: optionsConfigFor(OptionsConfig{Fields: map[string]FieldOptionsConfig{
				"code": {Type: "dynamic", Source: "tenantless_things", Value: "id", Label: "code"},
			}}),
			SearchConfigResolver: searchConfigFor(SearchConfig{SearchIn: []string{"code"}, Value: "id", Label: "code"}),
		})
		return svc
	}
	ctx := context.Background()
	user := newUser(uuid.New())
	id := uuid.New()

	calls := map[string]func(*Service) error{
		"list": func(s *Service) error {
			_, _, err := s.List(ctx, "tenantless_things", user, query.Params{Page: 1, PerPage: 10})
			return err
		},
		"get": func(s *Service) error { _, err := s.Get(ctx, "tenantless_things", user, id); return err },
		"aggregate": func(s *Service) error {
			_, err := s.Aggregate(ctx, "tenantless_things", user, query.Params{}, []string{"code"})
			return err
		},
		"facets": func(s *Service) error {
			_, err := s.Facets(ctx, user, FacetsQuery{Model: "tenantless_things", Field: "code"})
			return err
		},
		"options": func(s *Service) error {
			_, err := s.Options(ctx, user, OptionsQuery{Model: "tenantless_things", Field: "code"})
			return err
		},
		"search": func(s *Service) error {
			_, err := s.Search(ctx, user, SearchQuery{Model: "tenantless_things", Q: "x"})
			return err
		},
	}
	strict := build(true)
	for name, call := range calls {
		if err := call(strict); !errors.Is(err, ErrTenantScopeUnavailable) {
			t.Errorf("%s: want ErrTenantScopeUnavailable, got %v", name, err)
		}
	}
	// Facets/options/search keep the legacy answer when the flag is off.
	loose := build(false)
	for _, name := range []string{"facets", "options", "search"} {
		if err := calls[name](loose); errors.Is(err, ErrTenantScopeUnavailable) {
			t.Errorf("%s: default posture must not refuse, got %v", name, err)
		}
	}
}

// An org-scoped model still works under RequireTenantScope.
func TestRequireTenantScope_OrgModelStillServed(t *testing.T) {
	db := setupTestDB(t)
	modelbase.Register("test_products", func() modelbase.ModelDefiner { return &TestProduct{} })
	svc := New(Config{AllowUnconfiguredPermissions: true, DB: db, RequireTenantScope: true,
		Metadata: metadata.New(metadata.Config{CacheTTL: -1})})
	user := newUser(uuid.New())
	created := createProduct(t, svc, user, "A", 1)
	if items, _, err := svc.List(context.Background(), "test_products", user, query.Params{Page: 1, PerPage: 10}); err != nil || len(items) != 1 {
		t.Fatalf("list: %d %v", len(items), err)
	}
	id, _ := uuid.Parse(created["id"].(string))
	if _, err := svc.Get(context.Background(), "test_products", user, id); err != nil {
		t.Fatalf("get: %v", err)
	}
	if _, err := svc.Facets(context.Background(), user, FacetsQuery{Model: "test_products", Field: "name"}); err != nil {
		t.Fatalf("facets: %v", err)
	}
}
