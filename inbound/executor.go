package inbound

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"sync"

	"github.com/google/uuid"

	"github.com/asteby/metacore-kernel/dynamic"
	"github.com/asteby/metacore-kernel/modelbase"
	"github.com/asteby/metacore-kernel/query"
)

// Operation is the validated, mapped event handed to an executor. Org comes
// from the authenticated route (never from the payload).
type Operation struct {
	Org      uuid.UUID
	Contract string
	EventKey string
	Route    Route
	Values   map[string]any
	// Branch / Entity are the optional scope extracted from the payload.
	Origin string // "inbound:<contract>"
	// Hop is the hop counter of the incoming event (0 = external).
	Hop int
	// IdempotencyKey is "<receipt>:<action>" for chain actions; stable across
	// retries of the same event.
	IdempotencyKey string
	// CreateDefaults are constants applied ONLY when a row is created (never
	// on update, so a default like status=new cannot revert later changes).
	CreateDefaults map[string]any
	Branch string
	Entity string
	// RequireBranch / RequireEntity: the contract declares that scope, so the
	// event must carry it (the handler enforces it; executors re-check).
	RequireBranch, RequireEntity bool
}

// Outcome is what an executor reports back.
type Outcome struct {
	RecordID string
	Created  bool
}

// Executor applies model routes (create/update/upsert on a dynamic model).
type Executor interface {
	Execute(ctx context.Context, op Operation) (Outcome, error)
}

// CommandFunc is a host-registered command. Contracts reference it by name;
// they cannot define one.
type CommandFunc func(ctx context.Context, op Operation) (Outcome, error)

// CommandRegistry holds the host's registered commands.
type CommandRegistry struct {
	mu sync.RWMutex
	m  map[string]CommandFunc
}

// NewCommandRegistry returns an empty registry.
func NewCommandRegistry() *CommandRegistry { return &CommandRegistry{m: map[string]CommandFunc{}} }

// Register adds (or replaces) a command.
func (r *CommandRegistry) Register(name string, fn CommandFunc) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.m[name] = fn
}

func (r *CommandRegistry) get(name string) (CommandFunc, bool) {
	if r == nil {
		return nil, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	fn, ok := r.m[name]
	return fn, ok
}

// Kind classifies an execution failure for the uniform error response.
type Kind int

const (
	KindInternal  Kind = iota // 500, generic message
	KindInvalid               // 422 unprocessable
	KindConflict              // 409
	KindNotFound              // 404 record not found
	KindForbidden             // 403
)

// ExecError is how executors and commands signal a client-meaningful failure.
// Err is kept server-side only and never sent to the caller.
type ExecError struct {
	Kind Kind
	Code string // machine-readable, safe to expose
	Err  error
}

func (e *ExecError) Error() string {
	if e.Err != nil {
		return e.Code + ": " + e.Err.Error()
	}
	return e.Code
}
func (e *ExecError) Unwrap() error { return e.Err }

// dynStore is the slice of dynamic.Service the executor uses (lets tests
// substitute a fake; *dynamic.Service satisfies it).
type dynStore interface {
	List(ctx context.Context, model string, user modelbase.AuthUser, params query.Params) ([]map[string]any, query.PageMeta, error)
	Create(ctx context.Context, model string, user modelbase.AuthUser, input map[string]any) (map[string]any, error)
	Update(ctx context.Context, model string, user modelbase.AuthUser, id uuid.UUID, input map[string]any) (map[string]any, error)
	TableMetadata(ctx context.Context, model string) (*modelbase.TableMetadata, error)
}

// DynamicExecutor runs model routes through dynamic.Service as a system
// principal scoped to the route's organization: tenant scoping, validation,
// hooks and canonical events all apply; the permission gate is skipped
// because the signed request is the authorization.
//
// Upsert is find-then-write. Inside one process the pair is serialized per
// (org, model, match values); across replicas the model MUST carry a unique
// index over the match fields (see docs/inbound-events.md): the loser's create
// then fails, the executor re-reads and updates instead.
type DynamicExecutor struct {
	Service *dynamic.Service
	// Caller builds the principal; default dynamic.NewSystemCaller.
	Caller func(org uuid.UUID) modelbase.AuthUser
	// EntityScope resolves the entity scope of the event. It is REQUIRED when
	// a contract names an entity: an event naming an entity with no resolver,
	// or one the resolver cannot resolve (ok=false), is refused (fail closed).
	// Rows and branches outside the resolved scope are invisible to the match
	// and are never written.
	EntityScope func(ctx context.Context, org uuid.UUID, entityID string) (dynamic.EntityScope, bool)

	store dynStore // test seam; nil means Service
	locks [64]sync.Mutex
}

func (d *DynamicExecutor) st() (dynStore, error) {
	if d.store != nil {
		return d.store, nil
	}
	if d.Service == nil {
		return nil, &ExecError{Kind: KindInternal, Code: "executor_unavailable"}
	}
	return d.Service, nil
}

// CheckContract implements ContractChecker: every match field of a model route
// must be a declared column of the model, otherwise the query layer would
// silently drop the filter. The failure is a startup error, not a runtime one.
func (d *DynamicExecutor) CheckContract(ctx context.Context, c Contract) error {
	if c.Route.Model != "" {
		if err := d.checkModel(ctx, c.Key, c.Route.Model, c.Route.Match); err != nil {
			return err
		}
	}
	for i, a := range c.Actions {
		if a.Model == "" {
			continue
		}
		fields := append([]string(nil), a.Match...)
		if a.IdempotencyField != "" {
			fields = append(fields, a.IdempotencyField)
		}
		if err := d.checkModel(ctx, fmt.Sprintf("%s actions[%d]", c.Key, i), a.Model, fields); err != nil {
			return err
		}
	}
	return nil
}

// checkModel requires every field to be a declared column of the model.
func (d *DynamicExecutor) checkModel(ctx context.Context, who, model string, fields []string) error {
	st, err := d.st()
	if err != nil {
		return err
	}
	meta, err := st.TableMetadata(ctx, model)
	if err != nil || meta == nil {
		return fmt.Errorf("%w: %s: model %q has no metadata", ErrInvalidContract, who, model)
	}
	cols := map[string]bool{}
	for _, col := range meta.Columns {
		cols[col.Key] = true
	}
	for _, m := range fields {
		if !cols[m] {
			return fmt.Errorf("%w: %s: field %q is not a filterable column of model %q", ErrInvalidContract, who, m, model)
		}
	}
	return nil
}

func scopeErr(code string) error { return &ExecError{Kind: KindForbidden, Code: code} }

// Execute implements Executor.
func (d *DynamicExecutor) Execute(ctx context.Context, op Operation) (Outcome, error) {
	st, err := d.st()
	if err != nil {
		return Outcome{}, err
	}
	user := dynamic.NewSystemCaller(op.Org)
	if d.Caller != nil {
		user = d.Caller(op.Org)
	}
	if (op.RequireEntity && op.Entity == "") || (op.RequireBranch && op.Branch == "") {
		return Outcome{}, scopeErr("scope_required")
	}
	var sc *dynamic.EntityScope
	if op.Entity != "" {
		if d.EntityScope == nil {
			return Outcome{}, &ExecError{Kind: KindInternal, Code: "entity_scope_unconfigured"}
		}
		s, ok := d.EntityScope(ctx, op.Org, op.Entity)
		if !ok {
			return Outcome{}, scopeErr("scope_unresolved")
		}
		sc = &s
		ctx = dynamic.WithEntityID(ctx, op.Entity)
		ctx = dynamic.WithEntityScope(ctx, s)
		if op.Branch != "" && !s.Allows(op.Branch) {
			return Outcome{}, scopeErr("scope_forbidden")
		}
		if v, ok := op.Values["branch_id"]; ok && v != nil && !s.Allows(fmt.Sprint(v)) {
			return Outcome{}, scopeErr("scope_forbidden")
		}
	}
	if op.Branch != "" {
		ctx = dynamic.WithBranchID(ctx, op.Branch)
	}
	model := op.Route.Model
	switch op.Route.Op {
	case OpCreate:
		return create(ctx, st, model, user, withDefaults(op.CreateDefaults, op.Values))
	case OpUpdate, OpUpsert:
		unlock := d.lock(op)
		defer unlock()
		id, err := find(ctx, st, model, user, op.Route.Match, op.Values, sc)
		if err != nil {
			return Outcome{}, err
		}
		if id == uuid.Nil {
			if op.Route.Op == OpUpdate {
				return Outcome{}, &ExecError{Kind: KindNotFound, Code: "record_not_found"}
			}
			out, cerr := create(ctx, st, model, user, withDefaults(op.CreateDefaults, op.Values))
			if cerr == nil {
				return out, nil
			}
			// Lost a cross-replica race (unique index) -> update the winner.
			id, ferr := find(ctx, st, model, user, op.Route.Match, op.Values, sc)
			if ferr != nil || id == uuid.Nil {
				return Outcome{}, cerr
			}
			return update(ctx, st, model, user, id, op.Values)
		}
		return update(ctx, st, model, user, id, op.Values)
	}
	return Outcome{}, &ExecError{Kind: KindInternal, Code: "bad_op"}
}

func (d *DynamicExecutor) lock(op Operation) func() {
	h := fnv.New32a()
	h.Write([]byte(op.Org.String() + "|" + op.Route.Model))
	for _, m := range op.Route.Match {
		fmt.Fprintf(h, "|%s=%v", m, op.Values[m])
	}
	mu := &d.locks[h.Sum32()%uint32(len(d.locks))]
	mu.Lock()
	return mu.Unlock
}

func create(ctx context.Context, st dynStore, model string, user modelbase.AuthUser, vals map[string]any) (Outcome, error) {
	row, err := st.Create(ctx, model, user, vals)
	if err != nil {
		return Outcome{}, mapDynamicErr(err)
	}
	return Outcome{RecordID: idOf(row, ""), Created: true}, nil
}

func update(ctx context.Context, st dynStore, model string, user modelbase.AuthUser, id uuid.UUID, vals map[string]any) (Outcome, error) {
	row, err := st.Update(ctx, model, user, id, vals)
	if err != nil {
		return Outcome{}, mapDynamicErr(err)
	}
	return Outcome{RecordID: idOf(row, id.String())}, nil
}

// find returns the id of the single row matching every match field, or
// uuid.Nil. Because query drops filters on unknown columns, every returned
// row is re-checked against the wanted values (typed comparison); rows outside
// the entity scope are invisible, so an upsert never touches another entity.
func find(ctx context.Context, st dynStore, model string, user modelbase.AuthUser, match []string, vals map[string]any, sc *dynamic.EntityScope) (uuid.UUID, error) {
	filters := make(map[string]query.Filter, len(match))
	for _, f := range match {
		v, ok := vals[f]
		if !ok {
			return uuid.Nil, &ExecError{Kind: KindInvalid, Code: "match_value_missing"}
		}
		filters[f] = query.Filter{Op: query.OpEq, Value: fmt.Sprint(v)}
	}
	rows, _, err := st.List(ctx, model, user, query.Params{Page: 1, PerPage: 3, Filters: filters, SkipCount: true})
	if err != nil {
		return uuid.Nil, mapDynamicErr(err)
	}
	var hit map[string]any
	n, matched := 0, 0
	for _, r := range rows {
		ok := true
		for _, f := range match {
			if !valuesEqual(r[f], vals[f]) {
				ok = false
				break
			}
		}
		if !ok {
			continue
		}
		matched++
		if sc != nil {
			if b, has := r["branch_id"]; has && b != nil {
				if !sc.Allows(fmt.Sprint(b)) {
					continue
				}
			} else if !sc.Allows("") {
				continue
			}
		}
		hit = r
		n++
	}
	if matched == 0 && len(rows) > 0 {
		// Rows came back but none matches: the filter was ignored.
		return uuid.Nil, &ExecError{Kind: KindInternal, Code: "match_field_unusable",
			Err: errors.New("match field is not a filterable column of " + model)}
	}
	if n > 1 {
		return uuid.Nil, &ExecError{Kind: KindConflict, Code: "ambiguous_match"}
	}
	if n == 0 {
		return uuid.Nil, nil
	}
	id, err := uuid.Parse(fmt.Sprint(hit["id"]))
	if err != nil {
		return uuid.Nil, &ExecError{Kind: KindInternal, Code: "bad_record_id", Err: err}
	}
	return id, nil
}

// valuesEqual compares by type: numbers numerically, strings and bools
// exactly; anything else by its printed form.
func valuesEqual(a, b any) bool {
	if fa, ok := asFloat(a); ok {
		fb, ok := asFloat(b)
		return ok && fa == fb
	}
	if sa, ok := a.(string); ok {
		sb, ok := b.(string)
		return ok && sa == sb
	}
	if ba, ok := a.(bool); ok {
		bb, ok := b.(bool)
		return ok && ba == bb
	}
	return a != nil && b != nil && fmt.Sprint(a) == fmt.Sprint(b)
}

func asFloat(v any) (float64, bool) {
	switch t := v.(type) {
	case int:
		return float64(t), true
	case int32:
		return float64(t), true
	case int64:
		return float64(t), true
	case uint:
		return float64(t), true
	case uint32:
		return float64(t), true
	case uint64:
		return float64(t), true
	case float32:
		return float64(t), true
	case float64:
		return t, true
	}
	return 0, false
}

// ContractChecker is an optional Executor extension: validate a contract
// against the target model at registration time (startup error).
type ContractChecker interface {
	CheckContract(ctx context.Context, c Contract) error
}

func idOf(row map[string]any, def string) string {
	if v, ok := row["id"]; ok && v != nil {
		return fmt.Sprint(v)
	}
	return def
}

func mapDynamicErr(err error) error {
	var ee *ExecError
	if errors.As(err, &ee) {
		return err
	}
	switch {
	case errors.Is(err, dynamic.ErrValidation), errors.Is(err, dynamic.ErrInvalidInput),
		errors.Is(err, dynamic.ErrConstraintViolation), errors.Is(err, dynamic.ErrInvalidTransition),
		errors.Is(err, dynamic.ErrFieldRequired):
		return &ExecError{Kind: KindInvalid, Code: "rejected_by_model", Err: err}
	case errors.Is(err, dynamic.ErrInvalidState), errors.Is(err, dynamic.ErrSingletonExists):
		return &ExecError{Kind: KindConflict, Code: "state_conflict", Err: err}
	case errors.Is(err, dynamic.ErrRecordNotFound):
		return &ExecError{Kind: KindNotFound, Code: "record_not_found", Err: err}
	case errors.Is(err, dynamic.ErrForbidden):
		return &ExecError{Kind: KindForbidden, Code: "forbidden", Err: err}
	}
	return &ExecError{Kind: KindInternal, Code: "internal", Err: err}
}

// withDefaults returns vals over defaults (mapped data wins) as a new map.
func withDefaults(defaults, vals map[string]any) map[string]any {
	if len(defaults) == 0 {
		return vals
	}
	out := make(map[string]any, len(defaults)+len(vals))
	for k, v := range defaults {
		out[k] = v
	}
	for k, v := range vals {
		out[k] = v
	}
	return out
}
