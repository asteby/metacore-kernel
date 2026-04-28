package dynamic

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/asteby/metacore-kernel/metadata"
	"github.com/asteby/metacore-kernel/modelbase"
	"github.com/asteby/metacore-kernel/permission"
	"github.com/asteby/metacore-kernel/query"
)

// Config wires the dynamic CRUD service.
type Config struct {
	DB          *gorm.DB
	Metadata    *metadata.Service
	Permissions *permission.Service // optional — nil skips authz
	Hooks       *HookRegistry      // optional
	Scoper      TenantScoper       // optional — default OrganizationScoper

	// OptionsConfigResolver returns the OptionsConfig for a model. Apps
	// typically check (a) a compiled HasMetadata interface on the model and
	// (b) an addon-defined dynamic metadata registry for non-compiled models.
	// If nil, Service.Options returns ErrNoOptionsConfig.
	OptionsConfigResolver OptionsConfigResolver

	// SearchConfigResolver does the same for Service.Search. Apps usually
	// check an alias registry first, then HasMetadata, then the addon registry.
	SearchConfigResolver SearchConfigResolver

	// SearchMatchClause builds the SQL fragment and argument used for a
	// single OR-clause of a search query. Default: `<col> LIKE ?` with value
	// `%<q>%` — portable across sqlite, mysql and postgres.
	//
	// Postgres apps with the unaccent extension installed typically override
	// with:
	//
	//    func(col, q string) (string, any) {
	//        return fmt.Sprintf("unaccent(%s) ILIKE unaccent(?)", col), "%" + q + "%"
	//    }
	SearchMatchClause SearchMatchClause

	// ModelResolver returns a live instance of a registered model by name.
	// Apps that keep their own model registry (e.g. meta-core/models with
	// an addon-populated table) plug it here. When nil, modelbase.Get is
	// used — which only covers models explicitly registered at package init.
	ModelResolver ModelResolver
}

// ModelResolver lets apps supply their own model-name → instance lookup. The
// returned instance must implement modelbase.ModelDefiner (for TableName +
// DefineTable) so the CRUD paths keep working unchanged.
type ModelResolver func(ctx context.Context, model string) (any, bool)

// SearchMatchClause is the app-supplied builder used by Service.Search to
// turn a (column, query) pair into a SQL predicate + bind value. Returning
// ("", nil) from the callback skips the column.
type SearchMatchClause func(col, q string) (fragment string, value any)

// OptionsConfigResolver is the app-supplied lookup used by Service.Options to
// discover how a model exposes its field options.
type OptionsConfigResolver func(ctx context.Context, model string, instance any) (*OptionsConfig, error)

// SearchConfigResolver is the app-supplied lookup used by Service.Search.
type SearchConfigResolver func(ctx context.Context, model string, instance any) (*SearchConfig, error)

// Service is the transport-agnostic dynamic CRUD engine.
type Service struct {
	db             *gorm.DB
	meta           *metadata.Service
	perms          *permission.Service
	hooks          *HookRegistry
	scope          TenantScoper
	optsResolver   OptionsConfigResolver
	searchResolver SearchConfigResolver
	matchClause    SearchMatchClause
	modelResolver  ModelResolver
}

// New constructs a dynamic Service.
func New(cfg Config) *Service {
	if cfg.DB == nil {
		panic("dynamic: Config.DB is required")
	}
	if cfg.Metadata == nil {
		panic("dynamic: Config.Metadata is required")
	}
	if cfg.Scoper == nil {
		cfg.Scoper = OrganizationScoper{}
	}
	if cfg.SearchMatchClause == nil {
		cfg.SearchMatchClause = defaultSearchMatchClause
	}
	return &Service{
		db:             cfg.DB,
		meta:           cfg.Metadata,
		perms:          cfg.Perms(),
		hooks:          cfg.Hooks,
		scope:          cfg.Scoper,
		optsResolver:   cfg.OptionsConfigResolver,
		searchResolver: cfg.SearchConfigResolver,
		matchClause:    cfg.SearchMatchClause,
		modelResolver:  cfg.ModelResolver,
	}
}

// lookupModel resolves a model name to an instance. Apps that wire a
// ModelResolver get app-specific behaviour (e.g. addon-registered models);
// otherwise we fall back to modelbase's package-init registry.
func (s *Service) lookupModel(ctx context.Context, name string) (any, bool) {
	if s.modelResolver != nil {
		return s.modelResolver(ctx, name)
	}
	inst, ok := modelbase.Get(name)
	if !ok {
		return nil, false
	}
	return inst, true
}

// defaultSearchMatchClause is the portable LIKE matcher used when apps do not
// configure a dialect-specific one.
func defaultSearchMatchClause(col, q string) (string, any) {
	return fmt.Sprintf("%s LIKE ?", col), "%" + q + "%"
}

func (c Config) Perms() *permission.Service { return c.Permissions }

// List returns paginated, filtered, sorted records for a model.
func (s *Service) List(ctx context.Context, model string, user modelbase.AuthUser, params query.Params) ([]map[string]any, query.PageMeta, error) {
	instance, tableMeta, err := s.resolveModel(ctx, model)
	if err != nil {
		return nil, query.PageMeta{}, err
	}
	if err := s.checkPerm(ctx, user, model, "read"); err != nil {
		return nil, query.PageMeta{}, err
	}

	sliceType := reflect.SliceOf(reflect.TypeOf(instance))
	results := reflect.New(sliceType).Interface()

	db := s.db.WithContext(ctx).Table(instance.(modelbase.ModelDefiner).TableName())
	db = s.scope.ScopeQuery(db, user)

	builder := query.New(tableMeta)
	db = builder.Apply(db, params)

	total, err := builder.Count(s.db.WithContext(ctx).Table(instance.(modelbase.ModelDefiner).TableName()).Scopes(func(d *gorm.DB) *gorm.DB {
		return s.scope.ScopeQuery(builder.Apply(d, params), user)
	}), params)
	if err != nil {
		return nil, query.PageMeta{}, fmt.Errorf("dynamic: count: %w", err)
	}

	db = builder.Paginate(db, params)
	if err := db.Find(results).Error; err != nil {
		return nil, query.PageMeta{}, fmt.Errorf("dynamic: list: %w", err)
	}

	items := toMapSlice(results)
	return items, builder.PageMeta(total, params), nil
}

// TableMetadata is a thin accessor over the metadata service so the export /
// import handlers in this package can read column definitions without
// pulling another dependency through the constructor.
func (s *Service) TableMetadata(ctx context.Context, model string) (*modelbase.TableMetadata, error) {
	return s.meta.GetTable(ctx, model)
}

// Get returns a single record by ID.
func (s *Service) Get(ctx context.Context, model string, user modelbase.AuthUser, id uuid.UUID) (map[string]any, error) {
	instance, _, err := s.resolveModel(ctx, model)
	if err != nil {
		return nil, err
	}
	if err := s.checkPerm(ctx, user, model, "read"); err != nil {
		return nil, err
	}

	db := s.db.WithContext(ctx).Table(instance.(modelbase.ModelDefiner).TableName())
	db = s.scope.ScopeQuery(db, user)

	if err := db.First(instance, "id = ?", id).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			return nil, ErrRecordNotFound
		}
		return nil, err
	}
	return toMap(instance), nil
}

// Create inserts a record from a map[string]any input.
func (s *Service) Create(ctx context.Context, model string, user modelbase.AuthUser, input map[string]any) (map[string]any, error) {
	instance, _, err := s.resolveModel(ctx, model)
	if err != nil {
		return nil, err
	}
	if err := s.checkPerm(ctx, user, model, "create"); err != nil {
		return nil, err
	}

	s.scope.InjectOnCreate(input, user)
	input["created_by_id"] = user.GetID()

	hc := HookContext{Model: model, User: user, DB: s.db}
	if err := s.hooks.runBeforeCreate(ctx, hc, input); err != nil {
		return nil, err
	}

	if err := mapToStruct(input, instance); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidInput, err)
	}

	if err := s.db.WithContext(ctx).Create(instance).Error; err != nil {
		return nil, fmt.Errorf("dynamic: create: %w", err)
	}

	_ = s.hooks.runAfterCreate(ctx, hc, instance)
	return toMap(instance), nil
}

// Update modifies a record by ID.
func (s *Service) Update(ctx context.Context, model string, user modelbase.AuthUser, id uuid.UUID, input map[string]any) (map[string]any, error) {
	instance, _, err := s.resolveModel(ctx, model)
	if err != nil {
		return nil, err
	}
	if err := s.checkPerm(ctx, user, model, "update"); err != nil {
		return nil, err
	}

	db := s.db.WithContext(ctx).Table(instance.(modelbase.ModelDefiner).TableName())
	db = s.scope.ScopeQuery(db, user)

	if err := db.First(instance, "id = ?", id).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			return nil, ErrRecordNotFound
		}
		return nil, err
	}

	hc := HookContext{Model: model, User: user, DB: s.db}
	if err := s.hooks.runBeforeUpdate(ctx, hc, id.String(), input); err != nil {
		return nil, err
	}

	if err := mapToStruct(input, instance); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidInput, err)
	}

	if err := s.db.WithContext(ctx).Save(instance).Error; err != nil {
		return nil, fmt.Errorf("dynamic: update: %w", err)
	}

	_ = s.hooks.runAfterUpdate(ctx, hc, instance)
	return toMap(instance), nil
}

// Delete soft-deletes a record by ID.
func (s *Service) Delete(ctx context.Context, model string, user modelbase.AuthUser, id uuid.UUID) error {
	instance, _, err := s.resolveModel(ctx, model)
	if err != nil {
		return err
	}
	if err := s.checkPerm(ctx, user, model, "delete"); err != nil {
		return err
	}

	hc := HookContext{Model: model, User: user, DB: s.db}
	if err := s.hooks.runBeforeDelete(ctx, hc, id.String()); err != nil {
		return err
	}

	db := s.db.WithContext(ctx).Table(instance.(modelbase.ModelDefiner).TableName())
	db = s.scope.ScopeQuery(db, user)

	if err := db.Delete(instance, "id = ?", id).Error; err != nil {
		return fmt.Errorf("dynamic: delete: %w", err)
	}

	_ = s.hooks.runAfterDelete(ctx, hc, id.String())
	return nil
}

// --- internal -----------------------------------------------------------

func (s *Service) resolveModel(ctx context.Context, model string) (any, *modelbase.TableMetadata, error) {
	definer, ok := modelbase.Get(model)
	if !ok {
		return nil, nil, ErrModelNotFound
	}
	meta, err := s.meta.GetTable(ctx, model)
	if err != nil {
		return nil, nil, err
	}
	return definer, meta, nil
}

func (s *Service) checkPerm(ctx context.Context, user modelbase.AuthUser, model, action string) error {
	if s.perms == nil {
		return nil
	}
	return s.perms.Check(ctx, user, permission.Cap(model, action))
}

func mapToStruct(input map[string]any, target any) error {
	b, err := json.Marshal(input)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, target)
}

func toMap(v any) map[string]any {
	b, _ := json.Marshal(v)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	return m
}

func toMapSlice(slicePtr any) []map[string]any {
	v := reflect.ValueOf(slicePtr).Elem()
	result := make([]map[string]any, v.Len())
	for i := 0; i < v.Len(); i++ {
		result[i] = toMap(v.Index(i).Addr().Interface())
	}
	return result
}
