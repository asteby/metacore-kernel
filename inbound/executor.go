package inbound

import (
	"context"
	"errors"
	"fmt"
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
	Branch string
	Entity string
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

// DynamicExecutor runs model routes through dynamic.Service as a system
// principal scoped to the route's organization: tenant scoping, validation,
// hooks and canonical events all apply; the permission gate is skipped
// because the signed request is the authorization.
type DynamicExecutor struct {
	Service *dynamic.Service
	// Caller builds the principal; default dynamic.NewSystemCaller.
	Caller func(org uuid.UUID) modelbase.AuthUser
	// EntityScope optionally resolves the entity scope of the event. When it
	// returns ok and the event names a branch the scope forbids, the event is
	// rejected as forbidden (RFC 0001: no cross-entity writes).
	EntityScope func(ctx context.Context, org uuid.UUID, entityID string) (dynamic.EntityScope, bool)
}

// Execute implements Executor.
func (d *DynamicExecutor) Execute(ctx context.Context, op Operation) (Outcome, error) {
	if d.Service == nil {
		return Outcome{}, &ExecError{Kind: KindInternal, Code: "executor_unavailable"}
	}
	user := dynamic.NewSystemCaller(op.Org)
	if d.Caller != nil {
		user = d.Caller(op.Org)
	}
	if op.Entity != "" {
		ctx = dynamic.WithEntityID(ctx, op.Entity)
		if d.EntityScope != nil {
			if sc, ok := d.EntityScope(ctx, op.Org, op.Entity); ok {
				ctx = dynamic.WithEntityScope(ctx, sc)
				if op.Branch != "" && !sc.Allows(op.Branch) {
					return Outcome{}, &ExecError{Kind: KindForbidden, Code: "scope_forbidden"}
				}
			}
		}
	}
	if op.Branch != "" {
		ctx = dynamic.WithBranchID(ctx, op.Branch)
	}
	model := op.Route.Model
	switch op.Route.Op {
	case OpCreate:
		return d.create(ctx, model, user, op.Values)
	case OpUpdate, OpUpsert:
		id, err := d.find(ctx, model, user, op.Route.Match, op.Values)
		if err != nil {
			return Outcome{}, err
		}
		if id == uuid.Nil {
			if op.Route.Op == OpUpdate {
				return Outcome{}, &ExecError{Kind: KindNotFound, Code: "record_not_found"}
			}
			return d.create(ctx, model, user, op.Values)
		}
		row, err := d.Service.Update(ctx, model, user, id, op.Values)
		if err != nil {
			return Outcome{}, mapDynamicErr(err)
		}
		return Outcome{RecordID: idOf(row, id.String())}, nil
	}
	return Outcome{}, &ExecError{Kind: KindInternal, Code: "bad_op"}
}

func (d *DynamicExecutor) create(ctx context.Context, model string, user modelbase.AuthUser, vals map[string]any) (Outcome, error) {
	row, err := d.Service.Create(ctx, model, user, vals)
	if err != nil {
		return Outcome{}, mapDynamicErr(err)
	}
	return Outcome{RecordID: idOf(row, ""), Created: true}, nil
}

// find returns the id of the single row matching every match field, or
// uuid.Nil. Because query drops filters on unknown columns, every returned
// row is re-checked against the wanted values: a misconfigured match field can
// never update an arbitrary row.
func (d *DynamicExecutor) find(ctx context.Context, model string, user modelbase.AuthUser, match []string, vals map[string]any) (uuid.UUID, error) {
	filters := make(map[string]query.Filter, len(match))
	for _, f := range match {
		v, ok := vals[f]
		if !ok {
			return uuid.Nil, &ExecError{Kind: KindInvalid, Code: "match_value_missing"}
		}
		filters[f] = query.Filter{Op: query.OpEq, Value: fmt.Sprint(v)}
	}
	rows, _, err := d.Service.List(ctx, model, user, query.Params{Page: 1, PerPage: 3, Filters: filters, SkipCount: true})
	if err != nil {
		return uuid.Nil, mapDynamicErr(err)
	}
	var hit map[string]any
	n := 0
	for _, r := range rows {
		ok := true
		for _, f := range match {
			if fmt.Sprint(r[f]) != fmt.Sprint(vals[f]) {
				ok = false
				break
			}
		}
		if ok {
			hit = r
			n++
		}
	}
	if n == 0 && len(rows) > 0 {
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
