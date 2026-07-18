package dynamic

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/asteby/metacore-kernel/auth/adapters"
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
	Hooks       *HookRegistry       // optional
	Scoper      TenantScoper        // optional — default OrganizationScoper

	// OptionsConfigResolver returns the OptionsConfig for a model. Apps
	// typically check (a) a compiled HasMetadata interface on the model and
	// (b) an addon-defined dynamic metadata registry for non-compiled models.
	// If nil, Service.Options returns ErrNoOptionsConfig.
	OptionsConfigResolver OptionsConfigResolver

	// SearchConfigResolver does the same for Service.Search. Apps usually
	// check an alias registry first, then HasMetadata, then the addon registry.
	SearchConfigResolver SearchConfigResolver

	// EnableSelfOptions turns on the self-referential options fallback: when
	// Service.Options is asked for the options of a model's own identity field
	// ("id" / its primary key) and no explicit OptionsConfig declares that
	// field, the service synthesizes a dynamic config that lists the model
	// itself (value = the id column, label auto-derived from the first
	// name-like column). This makes a searchable async picker
	// (`type: "dynamic_select"`, `ref: "<Model>"`) work for ANY model with
	// zero manifest configuration — the declarative, cross-module FK lookup.
	//
	// It never shadows an explicitly declared field config (static enums like
	// an order's status, or a hand-tuned dynamic source, always win), and it
	// only fires for the identity field, so a genuinely-unconfigured non-id
	// field still surfaces ErrOptionsFieldNotFound. Default false keeps the
	// previous strict behaviour for hosts that have not opted in.
	EnableSelfOptions bool

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

	// TableNameResolver returns the database table for a model name. It exists
	// for hosts whose dynamic (addon) models are reflect-built structs that
	// CANNOT carry a Go TableName() method — so neither the modelbase.ModelDefiner
	// assertion nor GORM's pluralized schema parse can recover the real table.
	// The host (which already knows the table from its addon metadata registry)
	// plugs it here. Resolution order in the CRUD paths is:
	//   1. TableNameResolver(model) if set and ok,
	//   2. instance.(modelbase.ModelDefiner).TableName() if implemented,
	//   3. GORM schema parse (pluralized struct name) as last resort.
	// nil leaves the previous behaviour (compiled models) unchanged.
	TableNameResolver TableNameResolver

	// Bus is the in-process event bus where the service publishes canonical
	// "<addonKey>.<model>.<created|updated|deleted>" events post-commit.
	// nil disables emission — existing apps that do not wire a Bus keep the
	// previous behaviour.
	//
	// The contract matches *events.Bus.Publish — pass *events.Bus directly,
	// or any test double satisfying the Publisher interface. The decoupled
	// interface is necessary because events → security → bundle → dynamic
	// would close an import cycle.
	Bus Publisher

	// AddonKeyForModel resolves the addon owner of a model name. The result
	// is used both as the producer addonKey passed to Bus.Publish and as
	// the leading segment of the canonical event name. nil (or returning
	// "") falls back to "kernel" — appropriate for core models and for hosts
	// that have not yet wired an addon registry.
	AddonKeyForModel func(ctx context.Context, model string) string

	// ModelKeyForModel canonicalizes the model segment of a canonical event
	// name to the manifest ModelKey (e.g. table "transfers" -> "Transfer").
	// Create/Update/Delete may be invoked with EITHER the ModelKey or the
	// table/route name (the action-create path passes the route's table name);
	// without this the event fires as `<addon>.transfers.created` while
	// subscribers register on `<addon>.Transfer.created` (the documented
	// contract `<addon>.<ModelKey>.<action>`), so the handler never matches.
	// nil (or returning "") leaves the model segment as passed (retrocompat).
	ModelKeyForModel func(ctx context.Context, model string) string

	// ActionResolver returns the manifest.ActionDef declared for a
	// (model, key) pair. nil disables the action endpoint — calls return
	// ErrNoActionResolver. Hosts wire it from their addon registry: the
	// kernel does not own a global Actions index because actions live
	// inside the addon manifest.
	ActionResolver ActionResolver

	// ActionDispatchers maps Trigger.Type → ActionDispatcher. The built-in
	// "noop" dispatcher is registered automatically when the host does not
	// override it; "wasm" / "webhook" must be wired by the host (the
	// kernel deliberately does not import runtime/wasm or net/http here to
	// keep the dynamic package minimal).
	//
	// The same dispatchers double as the on_transition hook backends: a hook's
	// `do` ("wasm:<export>" | "webhook:<key>" | "compiled:<fn>") is split on its
	// prefix and routed to ActionDispatchers[prefix]. A model with stage hooks
	// therefore needs the matching dispatcher wired (a missing one is logged and
	// skipped for a non-required hook).
	ActionDispatchers map[string]ActionDispatcher

	// StageMachineResolver returns the stage-machine config for a model name —
	// the host wires it from its addon registry (the kernel owns no global model
	// index). When it returns a non-empty machine, Service.Update validates every
	// move of the StageField against Transitions and fires the matching
	// OnTransition hooks. nil (or returning ok=false / an empty machine) disables
	// the stage machine entirely, leaving Update unrestricted (back-compat).
	StageMachineResolver StageMachineResolver

	// ConstraintResolver returns the declarative guard predicates + row-locking
	// strategy for a model name — the host wires it from its addon registry, the
	// same way as StageMachineResolver. When it returns a non-empty set,
	// Service.Create/Update evaluate each Constraint before writing (aborting with
	// 422 + the ErrorKey on a false predicate) and, when Locking=="row", take a
	// SELECT … FOR UPDATE lock inside a transaction so an increment-then-check
	// guard is race-free. nil / ok=false / an empty set disables guards for the
	// model (back-compat).
	ConstraintResolver ConstraintResolver

	// ValidationSchemaResolver returns the declarative column definitions the
	// pre-write field-validation pass enforces for a model name — the host wires
	// it from its addon registry / manifest, the same way as the resolvers
	// above. When it returns a non-empty column slice, Service.Create/Update run
	// a structured validation pass BEFORE the DB write (required / invalid_option
	// / not_found / duplicate / invalid_type) and abort with 422 + a per-field
	// code map (a *ValidationError) on any failure. nil / ok=false / an empty
	// slice disables field validation for the model (back-compat: bad values are
	// dropped by coerce and any surviving violation surfaces from Postgres).
	ValidationSchemaResolver ValidationSchemaResolver

	// SequenceResolver returns the folio-sequence config for a model name —
	// host-wired from the addon registry, like the resolvers above. When set,
	// Service.Create auto-stamps every sequence-bound column left empty with the
	// next atomic folio value, and Service.NextSequence (the wasm sequence_next
	// backend) resolves a counter by (model, key). nil = no folios.
	SequenceResolver SequenceResolver

	// AuthUserExtractor optionally lets the Handler pull the authenticated
	// principal from the request context using the narrow AuthUserProvider
	// contract instead of the legacy UserResolver (which forces apps to
	// materialise a full modelbase.AuthUser).
	//
	// When configured, Handler.user falls back to the extractor whenever the
	// legacy UserResolver is nil or returns nil — apps mid-migration can wire
	// both and the handler picks the first that yields a principal. When nil
	// (the default), behaviour is unchanged.
	//
	// See the adapters package for built-in extractors
	// (ModelbaseExtractor / FiberLocalsExtractor / JWTClaimsExtractor).
	AuthUserExtractor adapters.AuthUserExtractor

	// FileDeleter disposes of file/image assets a dynamic record referenced
	// once that reference is removed — when the record is DELETED, or when a
	// file/image column's value is REPLACED on update. The kernel detects which
	// stored values are orphaned (by inspecting the model's file/image columns
	// against the pre/post row snapshots) but is storage-agnostic: it never
	// touches a filesystem. It hands the orphaned values to this host callback,
	// which resolves them to its backing store and removes them with its own
	// safety guards. nil disables file cleanup entirely (the prior behaviour).
	// See the FileDeleter type doc for the best-effort, non-fatal contract.
	FileDeleter FileDeleter
}

// Publisher is the minimal subset of events.Bus that dynamic.Service depends
// on. *events.Bus satisfies it natively; tests can plug a stub.
type Publisher interface {
	Publish(ctx context.Context, addonKey, event string, orgID uuid.UUID, payload any) error
}

// ModelResolver lets apps supply their own model-name → instance lookup. The
// returned instance should implement modelbase.ModelDefiner; reflect-built
// addon models that cannot (no Go methods) must additionally wire a
// TableNameResolver so the CRUD paths can pin the table explicitly.
type ModelResolver func(ctx context.Context, model string) (any, bool)

// TableNameResolver lets apps map a model name to its database table when the
// instance cannot carry a TableName() method (reflect-built addon models).
// Returning ("", false) defers to the ModelDefiner / GORM-parse fallbacks.
type TableNameResolver func(ctx context.Context, model string) (string, bool)

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
	db                *gorm.DB
	meta              *metadata.Service
	perms             *permission.Service
	hooks             *HookRegistry
	scope             TenantScoper
	optsResolver      OptionsConfigResolver
	searchResolver    SearchConfigResolver
	matchClause       SearchMatchClause
	listSearchClause  SearchMatchClause // explicit-only; threaded into List's builder search
	modelResolver     ModelResolver
	tableNameResolver TableNameResolver
	bus               Publisher
	addonKeyForModel  func(ctx context.Context, model string) string
	modelKeyForModel  func(ctx context.Context, model string) string
	actionResolver    ActionResolver
	actionDispatchers map[string]ActionDispatcher
	stageMachines     StageMachineResolver
	constraints       ConstraintResolver
	validationSchema  ValidationSchemaResolver
	sequences         SequenceResolver
	authExtractor     adapters.AuthUserExtractor
	selfOptions       bool
	fileDeleter       FileDeleter
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
	// Capture whether the host explicitly provided a search clause BEFORE
	// defaulting — only an explicit clause is threaded into List's builder, so
	// List's default search behaviour (builder ILIKE) is unchanged unless the
	// host opts in (e.g. ops wires unaccent ILIKE for accent-insensitive search).
	explicitSearchClause := cfg.SearchMatchClause
	if cfg.SearchMatchClause == nil {
		cfg.SearchMatchClause = defaultSearchMatchClause
	}
	dispatchers := make(map[string]ActionDispatcher, len(cfg.ActionDispatchers)+1)
	for k, d := range cfg.ActionDispatchers {
		if d != nil {
			dispatchers[k] = d
		}
	}
	if _, ok := dispatchers["noop"]; !ok {
		dispatchers["noop"] = NoopDispatcher{}
	}
	return &Service{
		db:                cfg.DB,
		meta:              cfg.Metadata,
		perms:             cfg.Perms(),
		hooks:             cfg.Hooks,
		scope:             cfg.Scoper,
		optsResolver:      cfg.OptionsConfigResolver,
		searchResolver:    cfg.SearchConfigResolver,
		matchClause:       cfg.SearchMatchClause,
		listSearchClause:  explicitSearchClause,
		modelResolver:     cfg.ModelResolver,
		tableNameResolver: cfg.TableNameResolver,
		bus:               cfg.Bus,
		addonKeyForModel:  cfg.AddonKeyForModel,
		modelKeyForModel:  cfg.ModelKeyForModel,
		actionResolver:    cfg.ActionResolver,
		actionDispatchers: dispatchers,
		stageMachines:     cfg.StageMachineResolver,
		constraints:       cfg.ConstraintResolver,
		validationSchema:  cfg.ValidationSchemaResolver,
		sequences:         cfg.SequenceResolver,
		authExtractor:     cfg.AuthUserExtractor,
		selfOptions:       cfg.EnableSelfOptions,
		fileDeleter:       cfg.FileDeleter,
	}
}

// AuthExtractor returns the configured AuthUserExtractor (or nil).
// dynamic.Handler uses it as the second-chance source for the authenticated
// principal when the legacy UserResolver is unwired or returns nil.
func (s *Service) AuthExtractor() adapters.AuthUserExtractor { return s.authExtractor }

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

// listBuilderOpts returns the query.Builder options for List. Currently it only
// threads a host-supplied search clause (e.g. unaccent ILIKE) when one was
// explicitly configured; otherwise List keeps the builder's default ILIKE. Kept
// as a helper so future List-only builder options have one wiring point.
func (s *Service) listBuilderOpts() []query.Option {
	var opts []query.Option
	if s.listSearchClause != nil {
		opts = append(opts, query.WithSearchClause(query.SearchClause(s.listSearchClause)))
	}
	return opts
}

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

	tableName, err := s.tableNameFor(ctx, model, instance)
	if err != nil {
		return nil, query.PageMeta{}, err
	}
	db := s.db.WithContext(ctx).Table(tableName)
	db = s.scope.ScopeQuery(db, user)

	// Wire relations into the builder so ?with= preloads and
	// f_<relation>.<field> filters work without callers having to plumb
	// HasRelations themselves. Models that do not implement HasRelations
	// get an empty relation set — the new query features are no-ops.
	builder := query.New(tableMeta, s.listBuilderOpts()...).WithTableName(tableName)
	if rels, ok := instance.(modelbase.HasRelations); ok {
		builder = builder.WithRelations(rels.DefineRelations())
	}
	db = builder.Apply(db, params)

	total, err := builder.Count(s.db.WithContext(ctx).Table(tableName).Scopes(func(d *gorm.DB) *gorm.DB {
		// Count must NOT carry the ORDER BY (applySort) — a COUNT(*) ordered by a
		// non-grouped column 42803s. Apply only the WHERE-shaping clauses.
		// scopeSoftDelete: Find's dest schema hides soft-deleted rows; a bare
		// COUNT has no schema, and a total above the listable set makes clients
		// page forever after the last row.
		return s.scope.ScopeQuery(scopeSoftDelete(builder.ApplyForCount(d, params), instance), user)
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

// Aggregate runs a single SELECT SUM(col) ... over the FILTERED set for each
// column in `columns`, applying the SAME relation filters, column filters,
// search and org/branch scope as List — but NO sort, pagination or preloads.
// It returns one map keyed by column name to the summed value, so the host can
// render a table footer whose totals match the filtered list (not the visible
// page). Columns that are unknown or unsafe are dropped by the builder's
// whitelist (applyAggregations) exactly like a garbage sort param, so the
// caller never has to pre-validate the column set.
func (s *Service) Aggregate(ctx context.Context, model string, user modelbase.AuthUser, params query.Params, columns []string) (map[string]any, error) {
	instance, tableMeta, err := s.resolveModel(ctx, model)
	if err != nil {
		return nil, err
	}
	if err := s.checkPerm(ctx, user, model, "read"); err != nil {
		return nil, err
	}

	tableName, err := s.tableNameFor(ctx, model, instance)
	if err != nil {
		return nil, err
	}

	// Each requested column becomes a SUM(col) AS col aggregation. The builder
	// re-validates Field/Alias against its allowed set + isSafeIdent, so a raw
	// column name can never be interpolated — unknown columns are dropped.
	aggs := make([]query.Aggregation, 0, len(columns))
	for _, col := range columns {
		aggs = append(aggs, query.Aggregation{Func: query.AggSum, Field: col, Alias: col})
	}
	// Overlay the aggregations onto the parsed list params so the footer carries
	// the SAME relation filters / column filters / search the list does.
	params.Aggregations = aggs
	// A footer total is one aggregate over the whole filtered set — never grouped
	// or sorted. Drop any inbound group_by/sort so the SELECT stays a single row.
	params.GroupBy = nil
	params.SortBy = ""

	db := s.db.WithContext(ctx).Table(tableName)
	db = s.scope.ScopeQuery(db, user)
	// The footer scans into a map (no schema) — mirror Find's soft-delete
	// filter or totals include rows the list never shows.
	db = scopeSoftDelete(db, instance)

	builder := query.New(tableMeta, s.listBuilderOpts()...).WithTableName(tableName)
	if rels, ok := instance.(modelbase.HasRelations); ok {
		builder = builder.WithRelations(rels.DefineRelations())
	}
	db = builder.ApplyForAggregate(db, params)

	row := map[string]any{}
	if err := db.Take(&row).Error; err != nil {
		return nil, fmt.Errorf("dynamic: aggregate: %w", err)
	}
	return row, nil
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

	tableName, err := s.tableNameFor(ctx, model, instance)
	if err != nil {
		return nil, err
	}
	db := s.db.WithContext(ctx).Table(tableName)
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

	tableName, err := s.tableNameFor(ctx, model, instance)
	if err != nil {
		return nil, err
	}

	s.scope.InjectOnCreate(input, user)
	input["created_by_id"] = user.GetID()

	// Normalize string-encoded typed fields (uuid/number/bool) so a form that
	// sends everything as strings doesn't fail the unmarshal. See coerce.go.
	// Structured pre-write validation: required / invalid_option / not_found /
	// duplicate / invalid_type, collected into a per-field code map. Runs after
	// scope injection but BEFORE coerce (so invalid_type can catch the raw
	// string values coerce would silently drop) and before hooks/guards/
	// mapToStruct — a bad input is rejected 422 with a field map instead of a
	// raw Postgres 500. A model with no wired validation schema is unaffected.
	if err := s.validateWrite(ctx, model, tableName, user, input, nil); err != nil {
		return nil, err
	}

	coerceInputToStruct(input, instance)

	hc := HookContext{Model: model, User: user, DB: s.db}
	if err := s.hooks.runBeforeCreate(ctx, hc, input); err != nil {
		return nil, err
	}

	// Declarative guards: evaluate the model's column Constraints against the
	// (formula-computed) input BEFORE the write. A false predicate aborts with a
	// 422 + ErrorKey and nothing is inserted. No locking is needed on create —
	// there is no prior row to serialize against.
	if mc := s.resolveConstraints(ctx, model); mc != nil {
		if err := evalConstraints(mc, input); err != nil {
			return nil, err
		}
	}

	// Folio sequences: stamp the next atomic value onto every sequence-bound
	// column the caller left empty. Runs AFTER the guards so a rejected create
	// never burns a folio; a failed INSERT below still can (the counter lives
	// in its own autocommit statement) — the format is documented gap-tolerant.
	// An explicitly-supplied value wins (ETL imports keep historical numbers).
	if err := s.assignSequences(ctx, model, user, input); err != nil {
		return nil, err
	}

	if err := mapToStruct(input, instance); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidInput, err)
	}

	// .Table pins the INTO so reflect-built addon models (no TableName()
	// method) insert into the right table instead of GORM's pluralized guess.
	if err := s.db.WithContext(ctx).Table(tableName).Create(instance).Error; err != nil {
		return nil, fmt.Errorf("dynamic: create: %w", err)
	}

	after := toMap(instance)
	idStr, _ := after["id"].(string)
	s.publishCanonical(ctx, model, "created", user, idStr, nil, after)

	_ = s.hooks.runAfterCreate(ctx, hc, instance)
	return after, nil
}

// Update modifies a record by ID.
func (s *Service) Update(ctx context.Context, model string, user modelbase.AuthUser, id uuid.UUID, input map[string]any) (map[string]any, error) {
	instance, tableMeta, err := s.resolveModel(ctx, model)
	if err != nil {
		return nil, err
	}
	if err := s.checkPerm(ctx, user, model, "update"); err != nil {
		return nil, err
	}

	tableName, err := s.tableNameFor(ctx, model, instance)
	if err != nil {
		return nil, err
	}

	// Declarative guards: when the model declares column Constraints AND
	// Locking=="row", the load → evaluate → save must share ONE transaction so
	// the SELECT … FOR UPDATE taken on load is still held at write time (an
	// increment-then-check guard is then race-free). When there is no row
	// locking, execDB stays s.db and the legacy per-statement flow is unchanged.
	mc := s.resolveConstraints(ctx, model)
	lockRows := mc.locksRows()

	sm := s.resolveStageMachine(ctx, model)
	var before, after map[string]any

	// core runs the load/evaluate/save against execDB. inTx reports whether it
	// runs inside a caller-owned transaction (the row-locking path) — the save +
	// transition hooks then run directly on execDB rather than opening a nested
	// transaction, and the FOR UPDATE lock is applied on load.
	core := func(execDB *gorm.DB, inTx bool) error {
		load := s.scope.ScopeQuery(execDB.WithContext(ctx).Table(tableName), user)
		if inTx && lockRows {
			load = load.Clauses(clause.Locking{Strength: "UPDATE"})
		}
		if err := load.First(instance, "id = ?", id).Error; err != nil {
			if err == gorm.ErrRecordNotFound {
				return ErrRecordNotFound
			}
			return err
		}

		// Snapshot the loaded record before BeforeUpdate hooks or input merging
		// can mutate `instance` — the canonical event needs the pre-mutation row
		// as `before`.
		before = toMap(instance)

		// Stage machine: when the model declares one and this Update changes the
		// stage_field, validate the move against the declared transitions BEFORE we
		// persist anything (a disallowed move is rejected 422, never written).
		var fromStage, toStage string
		var stageChanged bool
		if sm != nil {
			fromStage, toStage, stageChanged, err = s.checkTransition(sm, before, input)
			if err != nil {
				return err
			}
			// Declarative `set`: when the move fires hooks that stamp fields on the
			// row itself, apply them into `input` BEFORE we persist so they ride the
			// same Save and surface in the canonical event (subscribers see them). We
			// resolve "+tag"/"-tag" against the merged current-plus-incoming view, then
			// fold only the set keys back onto input.
			if stageChanged {
				merged := make(map[string]any, len(before)+len(input))
				for k, v := range before {
					merged[k] = v
				}
				for k, v := range input {
					merged[k] = v
				}
				if ApplyTransitionSets(sm, fromStage, toStage, merged) {
					for _, h := range sm.matchingHooks(fromStage, toStage) {
						for k := range h.Set {
							input[k] = merged[k]
						}
					}
				}
			}
		}

		// Structured pre-write validation (PATCH-style: only keys present in
		// input are checked, and this record's own id is excluded from the
		// unique check). Runs before coerce (raw values for invalid_type) and
		// before hooks/guards/mapToStruct; inside the lock when locking so the
		// duplicate check is serialized with the write.
		selfID := id
		if err := s.validateWrite(ctx, model, tableName, user, input, &selfID); err != nil {
			return err
		}

		// Normalize string-encoded typed fields before merging into the record.
		coerceInputToStruct(input, instance)

		hc := HookContext{Model: model, User: user, DB: execDB}
		if err := s.hooks.runBeforeUpdate(ctx, hc, id.String(), input); err != nil {
			return err
		}

		// Declarative guards: evaluate the model's column Constraints against the
		// merged (locked, formula-computed) row BEFORE the write. A false predicate
		// aborts with a 422 + ErrorKey, rolling back the transaction when locking.
		if mc != nil {
			merged := make(map[string]any, len(before)+len(input))
			for k, v := range before {
				merged[k] = v
			}
			for k, v := range input {
				merged[k] = v
			}
			if err := evalConstraints(mc, merged); err != nil {
				return err
			}
		}

		if err := mapToStruct(input, instance); err != nil {
			return fmt.Errorf("%w: %v", ErrInvalidInput, err)
		}

		// When the stage changed and the machine declares on_transition hooks, the
		// save + hook dispatch run together in a transaction so a REQUIRED hook
		// failure rolls back the whole transition. Otherwise the save is a plain
		// write (unchanged from the legacy path) and any (non-required) hooks fire
		// best-effort post-commit. In the row-locking path we are ALREADY inside a
		// transaction, so save + hooks run directly on execDB.
		runHooksInTx := stageChanged && len(sm.matchingHooks(fromStage, toStage)) > 0
		if inTx {
			if err := execDB.Table(tableName).Save(instance).Error; err != nil {
				return fmt.Errorf("dynamic: update: %w", err)
			}
			after = toMap(instance)
			if runHooksInTx {
				return s.runTransitionHooks(ctx, model, user, execDB, sm, fromStage, toStage, before, after)
			}
			return nil
		}
		if runHooksInTx {
			return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
				if err := tx.Table(tableName).Save(instance).Error; err != nil {
					return fmt.Errorf("dynamic: update: %w", err)
				}
				after = toMap(instance)
				return s.runTransitionHooks(ctx, model, user, tx, sm, fromStage, toStage, before, after)
			})
		}
		if err := s.db.WithContext(ctx).Table(tableName).Save(instance).Error; err != nil {
			return fmt.Errorf("dynamic: update: %w", err)
		}
		after = toMap(instance)
		return nil
	}

	if lockRows {
		if txErr := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			return core(tx, true)
		}); txErr != nil {
			return nil, txErr
		}
	} else {
		if err := core(s.db, false); err != nil {
			return nil, err
		}
	}

	// Orphaned-file cleanup: any file/image column whose value changed leaves
	// its OLD asset unreferenced. Storage-agnostic — we hand the orphaned values
	// to the host's FileDeleter (best-effort, post-commit). nil deleter = no-op.
	if s.fileDeleter != nil {
		if orphaned := collectOrphanedFromUpdate(before, after, tableMeta); len(orphaned) > 0 {
			s.fileDeleter(ctx, model, orphaned)
		}
	}

	s.publishCanonical(ctx, model, "updated", user, id.String(), before, after)

	// After-hooks fire post-commit against the standalone handle (the tx, if any,
	// is already committed here).
	_ = s.hooks.runAfterUpdate(ctx, HookContext{Model: model, User: user, DB: s.db}, instance)
	return after, nil
}

// Delete soft-deletes a record by ID.
func (s *Service) Delete(ctx context.Context, model string, user modelbase.AuthUser, id uuid.UUID) error {
	instance, tableMeta, err := s.resolveModel(ctx, model)
	if err != nil {
		return err
	}
	if err := s.checkPerm(ctx, user, model, "delete"); err != nil {
		return err
	}

	tableName, err := s.tableNameFor(ctx, model, instance)
	if err != nil {
		return err
	}

	hc := HookContext{Model: model, User: user, DB: s.db}
	if err := s.hooks.runBeforeDelete(ctx, hc, id.String()); err != nil {
		return err
	}

	db := s.db.WithContext(ctx).Table(tableName)
	db = s.scope.ScopeQuery(db, user)

	// Snapshot the row before the delete so the canonical event can carry
	// `before` AND so we can route the deleted row's file/image assets to the
	// FileDeleter. A miss here is tolerated: the delete itself drives the error
	// semantics and subscribers see `before == nil` to mean the row was
	// already gone (or out of tenant scope) at publish time. We load whenever a
	// bus OR a file deleter is wired — either consumer needs the pre-delete row.
	var before map[string]any
	if s.bus != nil || s.fileDeleter != nil {
		loadDB := s.db.WithContext(ctx).Table(tableName)
		loadDB = s.scope.ScopeQuery(loadDB, user)
		if err := loadDB.First(instance, "id = ?", id).Error; err == nil {
			before = toMap(instance)
		}
	}

	if err := db.Delete(instance, "id = ?", id).Error; err != nil {
		return fmt.Errorf("dynamic: delete: %w", err)
	}

	// Orphaned-file cleanup: the row is gone, so every file/image asset it
	// referenced is now unreachable. Storage-agnostic — hand the values to the
	// host's FileDeleter (best-effort, post-commit). nil deleter = no-op.
	if s.fileDeleter != nil {
		if orphaned := collectOrphanedFromDelete(before, tableMeta); len(orphaned) > 0 {
			s.fileDeleter(ctx, model, orphaned)
		}
	}

	s.publishCanonical(ctx, model, "deleted", user, id.String(), before, nil)

	_ = s.hooks.runAfterDelete(ctx, hc, id.String())
	return nil
}

// --- internal -----------------------------------------------------------

func (s *Service) resolveModel(ctx context.Context, model string) (any, *modelbase.TableMetadata, error) {
	// Route through lookupModel so an app-supplied Config.ModelResolver wins
	// over the package-init modelbase registry. Hosts that keep their own
	// model index (e.g. meta-core/models) plug it via Config.ModelResolver;
	// when nil, lookupModel falls back to modelbase.Get for backward compat.
	definer, ok := s.lookupModel(ctx, model)
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
