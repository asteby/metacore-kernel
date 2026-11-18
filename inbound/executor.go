package inbound

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/asteby/metacore-kernel/dynamic"
	"github.com/asteby/metacore-kernel/manifest"
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
	Branch         string
	Entity         string
	// RequireBranch / RequireEntity: the contract declares that scope, so the
	// event must carry it (the handler enforces it; executors re-check).
	RequireBranch, RequireEntity bool
	// DeleteMode (soft|hard; empty = soft) and MaxRows (rows one event may
	// retire; <= 0 = 1) apply to OpDelete only. Route.Match selects the rows.
	DeleteMode string
	MaxRows    int
	// Guard is the evaluated order guard of the action (nil: none, or the
	// event carries no usable value). Executors that honour it implement
	// GuardSupporter; a stale event is reported as Outcome.Stale.
	Guard *OperationGuard
	// ActionKey / Params (record.action): the model action to run and its
	// payload; OnIneligible is noop (default) | fail. To (record.transition) is
	// the target state. Route.Match selects the rows; MaxRows caps them.
	ActionKey    string
	Params       map[string]any
	OnIneligible string
	To           string
	// DryRun (record.action): resolve the rows, their state and the payload and
	// report them in Outcome.WouldDispatch WITHOUT dispatching anything. The
	// handler does not set it yet (no dry-run mode in inbound); the executor
	// honours it so the contract is ready.
	DryRun bool
}

// WouldDispatch is what a dry-run record.action resolved for one row.
type WouldDispatch struct {
	Action string `json:"action"`
	// RecordID is the first selected row; Rows how many were selected.
	RecordID string `json:"record_id,omitempty"`
	Rows     int    `json:"rows"`
	// State is the lifecycle value of the row ("" when unreadable) and Eligible
	// whether the action RequiresState accepts it (true when it declares none).
	State    string         `json:"state,omitempty"`
	Eligible bool           `json:"eligible"`
	Params   map[string]any `json:"params,omitempty"`
}

// Outcome is what an executor reports back.
type Outcome struct {
	RecordID string
	Created  bool
	// Affected / NotFound are reported by OpDelete: how many rows were
	// retired (RecordID is the first one), or NotFound when nothing matched
	// (already deleted), which is a successful no-op.
	Affected int
	NotFound bool
	// Stale: the guard found a newer row or tombstone; nothing was written.
	Stale bool
	// Noop / Reason: record.action|transition|restore did nothing on purpose
	// (state_not_eligible, already_in_state, already_live); it is a success.
	// Affected counts the rows they changed.
	Noop   bool
	Reason string
	// WouldDispatch is set by a dry-run record.action (Operation.DryRun).
	WouldDispatch *WouldDispatch
}

// Executor applies model routes (create/update/upsert on a dynamic model).
type Executor interface {
	Execute(ctx context.Context, op Operation) (Outcome, error)
}

// OpSupporter is an optional Executor port: an Executor that implements it
// declares which operations it handles. A contract that declares record.delete
// is refused at registration unless the Executor reports SupportsOp(OpDelete),
// and likewise record.action / record.transition / record.restore for
// OpAction / OpTransition / OpRestore, so a custom executor whose default
// branch creates or upserts can never receive an operation it would misread.
// DynamicExecutor supports every Op.
type OpSupporter interface {
	SupportsOp(op Op) bool
}

// SupportsOp implements OpSupporter: the DynamicExecutor handles every Op.
func (d *DynamicExecutor) SupportsOp(Op) bool { return true }

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
	Delete(ctx context.Context, model string, user modelbase.AuthUser, id uuid.UUID) error
	// UpdateGuarded and DeleteSealed are the guarded writes of dynamic/guarded.go:
	// the staleness check and the write happen under one row lock, and the seal
	// plus the soft delete in one transaction.
	UpdateGuarded(ctx context.Context, model string, user modelbase.AuthUser, id uuid.UUID, input map[string]any, g *dynamic.WriteGuard) (map[string]any, error)
	DeleteSealed(ctx context.Context, model string, user modelbase.AuthUser, id uuid.UUID, seal map[string]any, g *dynamic.WriteGuard) error
	TableMetadata(ctx context.Context, model string) (*modelbase.TableMetadata, error)
	// ExecAction, Restore, ActionDef and ActiveStageMachine back record.action,
	// record.restore and record.transition (see record_ops.go).
	ExecAction(ctx context.Context, model string, user modelbase.AuthUser, id uuid.UUID, key string, payload map[string]any) (dynamic.ActionResult, error)
	Restore(ctx context.Context, model string, user modelbase.AuthUser, id uuid.UUID) (map[string]any, error)
	ActionDef(ctx context.Context, model, key string) (*manifest.ActionDef, bool)
	ActiveStageMachine(ctx context.Context, model string) (*dynamic.StageMachine, bool)
}

// HardDeleter physically removes a row. dynamic.Service only soft-deletes, so
// a host that wants record.delete mode "hard" supplies this port (and lists
// the models in DynamicExecutor.HardDeleteModels). It must apply the same
// tenant scoping as the user it receives.
type HardDeleter interface {
	HardDelete(ctx context.Context, model string, user modelbase.AuthUser, id uuid.UUID) error
}

// defaultMaxDeleteRows is the DynamicExecutor cap on rows per record.delete.
const defaultMaxDeleteRows = 100

// maxDeleteRowsExecutorLimit is the highest MaxDeleteRows the executor accepts:
// matchRows lists limit+1 rows and query.MaxPerPage (200) clips the page, so a
// limit of 200 could not detect an excess.
const maxDeleteRowsExecutorLimit = 199

// DynamicExecutor runs model routes through dynamic.Service as a system
// principal scoped to the route's organization: tenant scoping, validation,
// hooks and canonical events all apply; the permission gate is skipped
// because the signed request is the authorization.
//
// Upsert is find-then-write. Inside one process the pair is serialized per
// (org, model, match values) as an optimization only; across replicas the model
// MUST carry a unique index over the match fields (see docs/inbound-events.md):
// the loser's create then fails, the executor re-reads and updates instead.
// With a guard the write is conditional on the database row (UpdateGuarded /
// DeleteSealed lock the row and judge the guard on the committed value), so a
// stale event never overwrites a newer one, whichever replica wrote it.
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
	// Authorize is the optional write gate of the host: the allow-list of
	// models an enabled addon declared writable and the permission of whoever
	// holds the inbound entry ("<model>.<action>"). It runs before any
	// create|update|delete reaches the model (upsert asks "update", and
	// "create" when it has to insert); record.action asks "action:<key>",
	// record.transition "transition:<to>" and record.restore "restore"; false
	// answers 403 forbidden. nil keeps
	// the historical rule: the signed route is the authorization.
	Authorize func(ctx context.Context, org uuid.UUID, model, action string) bool
	// HardDeleter and HardDeleteModels enable record.delete mode "hard": it is
	// refused unless the host supplies the deleter AND lists the model here.
	HardDeleter      HardDeleter
	HardDeleteModels map[string]bool
	// MaxDeleteRows caps rows per record.delete (default 100).
	MaxDeleteRows int
	// Tombstones lets an action guard see soft-deleted rows (see
	// TombstoneFinder); when nil the store is used if it implements it.
	Tombstones TombstoneFinder
	// GuardMaxSkew bounds how far into the future an event's guard instant may
	// be (default 24h; negative disables the bound). A later one is refused
	// with guard_value_future: signed with year 9999 it would freeze the row
	// and every legitimate later event would be stale_event. Integer guards
	// (versions) are not bounded here; use "maximum" in the JSON schema.
	GuardMaxSkew time.Duration

	store dynStore         // test seam; nil means Service
	now   func() time.Time // test seam; nil means time.Now
	// locks is an OPTIMIZATION, never a guarantee: it serializes the requests of
	// this process that share a match key, so they do not queue on the database
	// row lock. It does not cross replicas and an action whose match differs
	// (a delete by sku+warehouse next to an upsert by sku) takes another bucket.
	// What makes a guarded write correct is dynamic.Service.UpdateGuarded /
	// DeleteSealed: the stale check runs on the row locked in the database, so
	// it holds whatever the match and however many replicas there are.
	locks [64]sync.Mutex
}

func (d *DynamicExecutor) clock() time.Time {
	if d.now != nil {
		return d.now()
	}
	return time.Now()
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
		if err := d.checkWritable(ctx, c.Key, c.Route.Model, c.Route.Mapping); err != nil {
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
		who := fmt.Sprintf("%s actions[%d]", c.Key, i)
		if err := d.checkModel(ctx, who, a.Model, fields); err != nil {
			return err
		}
		if a.Do != ActionRecordDelete && !isStateOp(a.Do) {
			if err := d.checkWritable(ctx, who, a.Model, a.Mapping); err != nil {
				return err
			}
		}
		if a.Do == ActionRecordDelete {
			if err := d.checkDeleteModel(ctx, who, a); err != nil {
				return err
			}
		}
		if isStateOp(a.Do) {
			if err := d.checkStateOpModel(ctx, who, a); err != nil {
				return err
			}
		}
		if a.Guard != nil {
			if err := d.checkGuard(ctx, who, a); err != nil {
				return err
			}
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

// checkWritable requires every mapping target to be a column an entry may write
// (WritableColumns): a system, readonly, generated or protected column fails
// registration instead of being rejected at runtime.
func (d *DynamicExecutor) checkWritable(ctx context.Context, who, model string, mapping map[string]string) error {
	if len(mapping) == 0 {
		return nil
	}
	st, err := d.st()
	if err != nil {
		return err
	}
	meta, err := st.TableMetadata(ctx, model)
	if err != nil || meta == nil {
		return fmt.Errorf("%w: %s: model %q has no metadata", ErrInvalidContract, who, model)
	}
	ok := map[string]bool{}
	for _, c := range WritableColumns(meta) {
		ok[c.Name] = true
	}
	for to := range mapping {
		if !ok[to] {
			return fmt.Errorf("%w: %s: mapping target %q is not a writable column of model %q", ErrInvalidContract, who, to, model)
		}
	}
	return nil
}

// checkDeleteModel fails registration when a record.delete cannot work: soft
// needs a model with deleted_at; hard needs the deleter and the explicit
// opt-in of the model.
func (d *DynamicExecutor) checkDeleteModel(ctx context.Context, who string, a Action) error {
	if d.maxDeleteRows() > maxDeleteRowsExecutorLimit {
		return fmt.Errorf("%w: %s: MaxDeleteRows %d exceeds %d (query page cap)", ErrInvalidContract, who, d.maxDeleteRows(), maxDeleteRowsExecutorLimit)
	}
	if err := d.checkAppendOnly(ctx, who, a); err != nil {
		return err
	}
	if a.MaxRows > d.maxDeleteRows() {
		return fmt.Errorf("%w: %s: max_rows %d exceeds the executor cap %d", ErrInvalidContract, who, a.MaxRows, d.maxDeleteRows())
	}
	if a.Mode == DeleteHard {
		if d.HardDeleter == nil || !d.HardDeleteModels[a.Model] {
			return fmt.Errorf("%w: %s: hard delete is not enabled for model %q", ErrInvalidContract, who, a.Model)
		}
		return nil
	}
	st, err := d.st()
	if err != nil {
		return err
	}
	meta, err := st.TableMetadata(ctx, a.Model)
	if err != nil || !supportsSoftDelete(meta) {
		return fmt.Errorf("%w: %s: model %q does not support soft delete (deleted_at)", ErrInvalidContract, who, a.Model)
	}
	return nil
}

// checkAppendOnly refuses record.delete on an append-only ledger (metadata
// AppendOnly), soft and hard alike. A mandatory delete reason is not in the
// model metadata: that policy still fails at runtime as a 422 rejected_by_model.
func (d *DynamicExecutor) checkAppendOnly(ctx context.Context, who string, a Action) error {
	st, err := d.st()
	if err != nil {
		return err
	}
	if meta, err := st.TableMetadata(ctx, a.Model); err == nil && meta != nil && meta.AppendOnly {
		return fmt.Errorf("%w: %s: model %q is append-only and cannot be deleted", ErrInvalidContract, who, a.Model)
	}
	return nil
}

func supportsSoftDelete(meta *modelbase.TableMetadata) bool {
	return meta != nil && meta.Audit != nil && meta.Audit.DeletedAt != ""
}

func (d *DynamicExecutor) maxDeleteRows() int {
	if d.MaxDeleteRows > 0 {
		return d.MaxDeleteRows
	}
	return defaultMaxDeleteRows
}

func (d *DynamicExecutor) allow(ctx context.Context, op Operation, action string) error {
	if d.Authorize != nil && !d.Authorize(ctx, op.Org, op.Route.Model, action) {
		return &ExecError{Kind: KindForbidden, Code: "forbidden"}
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
	if op.Guard != nil {
		if err := op.Guard.checkFuture(d.clock(), d.GuardMaxSkew); err != nil {
			return Outcome{}, err
		}
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
	// The match values take the type of their column before anything reads
	// them (find, typed comparison, tombstones, write).
	if err := normalizeMatch(ctx, st, &op); err != nil {
		return Outcome{}, err
	}
	model := op.Route.Model
	switch op.Route.Op {
	case OpCreate:
		if err := d.allow(ctx, op, "create"); err != nil {
			return Outcome{}, err
		}
		return create(ctx, st, model, user, withDefaults(op.CreateDefaults, op.Values))
	case OpDelete:
		return d.deleteRows(ctx, st, user, op, sc)
	case OpAction, OpTransition, OpRestore:
		return d.stateOp(ctx, st, user, op, sc)
	case OpUpdate, OpUpsert:
		if err := d.allow(ctx, op, "update"); err != nil {
			return Outcome{}, err
		}
		// The in-process lock only reduces contention between the requests of
		// this replica; the guarantee of a guarded write is the row lock of
		// UpdateGuarded (see writeRow), which holds across replicas.
		unlock := d.lock(op)
		defer unlock()
		// A row another replica deletes between the find and the guarded write
		// is judged again from the top (a tombstone may now decide), a bounded
		// number of times.
		for attempt := 0; attempt < maxWriteAttempts; attempt++ {
			out, gone, err := d.upsertOnce(ctx, st, user, op, sc)
			if !gone {
				return out, err
			}
		}
		return Outcome{}, &ExecError{Kind: KindConflict, Code: "concurrent_modification"}
	}
	return Outcome{}, &ExecError{Kind: KindInternal, Code: "bad_op"}
}

// maxWriteAttempts bounds how many times an upsert/update re-reads the row
// after losing it to a concurrent delete.
const maxWriteAttempts = 3

// upsertOnce is one find -> guard -> write pass of record.update|upsert. gone
// reports that the row vanished under a guarded write and the pass must be
// repeated.
func (d *DynamicExecutor) upsertOnce(ctx context.Context, st dynStore, user modelbase.AuthUser, op Operation, sc *dynamic.EntityScope) (out Outcome, gone bool, err error) {
	model := op.Route.Model
	row, err := findRow(ctx, st, model, user, op.Route.Match, op.Values, sc)
	if err != nil {
		return Outcome{}, false, err
	}
	id, err := rowID(row)
	if err != nil {
		return Outcome{}, false, err
	}
	if id != uuid.Nil {
		stale, err := d.guardStale(ctx, st, op, sc, row)
		if err != nil {
			return Outcome{}, false, err
		}
		if stale {
			return Outcome{Stale: true}, false, nil
		}
		return d.writeRow(ctx, st, user, op, id)
	}
	if op.Route.Op == OpUpdate {
		return Outcome{}, false, &ExecError{Kind: KindNotFound, Code: "record_not_found"}
	}
	if err := d.allow(ctx, op, "create"); err != nil {
		return Outcome{}, false, err
	}
	stale, err := d.guardStale(ctx, st, op, sc, nil)
	if err != nil {
		return Outcome{}, false, err
	}
	if stale {
		return Outcome{Stale: true}, false, nil
	}
	out, cerr := create(ctx, st, model, user, withDefaults(op.CreateDefaults, op.Values))
	if cerr == nil {
		return out, false, nil
	}
	// Lost a cross-replica race (unique index) -> update the winner,
	// but only after the guard and the tombstones have judged it too.
	winner, ferr := findRow(ctx, st, model, user, op.Route.Match, op.Values, sc)
	wid, ierr := rowID(winner)
	if ferr != nil || ierr != nil || wid == uuid.Nil {
		return Outcome{}, false, cerr
	}
	stale, err = d.guardStale(ctx, st, op, sc, winner)
	if err == nil && !stale {
		stale, err = d.guardStale(ctx, st, op, sc, nil)
	}
	if err != nil {
		return Outcome{}, false, err
	}
	if stale {
		return Outcome{Stale: true}, false, nil
	}
	return d.writeRow(ctx, st, user, op, wid)
}

// writeRow updates the row id with the event's values. Without a guard it is a
// plain Update. With one it is UpdateGuarded: the row is locked and the guard
// is judged again on the locked, committed value, so an event that went stale
// after the read (another replica wrote a newer one) is dropped instead of
// overwriting it. gone reports the row no longer exists.
func (d *DynamicExecutor) writeRow(ctx context.Context, st dynStore, user modelbase.AuthUser, op Operation, id uuid.UUID) (out Outcome, gone bool, err error) {
	model := op.Route.Model
	if op.Guard == nil {
		out, err = update(ctx, st, model, user, id, op.Values)
		return out, false, err
	}
	vals := make(map[string]any, len(op.Values))
	for k, v := range op.Values {
		vals[k] = v
	}
	row, err := st.UpdateGuarded(ctx, model, user, id, vals, op.Guard.writeGuard())
	switch {
	case err == nil:
		return Outcome{RecordID: idOf(row, id.String())}, false, nil
	case errors.Is(err, dynamic.ErrStaleWrite):
		return Outcome{Stale: true}, false, nil
	case errors.Is(err, dynamic.ErrRecordNotFound):
		return Outcome{}, true, nil
	}
	return Outcome{}, false, mapDynamicErr(err)
}

// deleteRows runs OpDelete. Rows are selected only by Route.Match (typed
// re-check, organization and entity scope applied by the store), never by id.
// Nothing is deleted when more rows match than MaxRows. Zero matches is a
// successful no-op (NotFound), so a replayed or already-applied delete
// converges.
func (d *DynamicExecutor) deleteRows(ctx context.Context, st dynStore, user modelbase.AuthUser, op Operation, sc *dynamic.EntityScope) (Outcome, error) {
	model := op.Route.Model
	mode := op.DeleteMode
	if mode == "" {
		mode = DeleteSoft
	}
	if mode != DeleteSoft && mode != DeleteHard {
		return Outcome{}, &ExecError{Kind: KindInvalid, Code: "bad_delete_mode"}
	}
	limit := op.MaxRows
	if limit <= 0 {
		limit = 1
	}
	if limit > d.maxDeleteRows() {
		return Outcome{}, &ExecError{Kind: KindInvalid, Code: "max_rows_exceeds_limit"}
	}
	if len(op.Route.Match) == 0 {
		return Outcome{}, &ExecError{Kind: KindInvalid, Code: "match_required"}
	}
	if err := d.allow(ctx, op, "delete"); err != nil {
		return Outcome{}, err
	}
	if op.Guard != nil && mode == DeleteSoft {
		// The seal is an UPDATE through dynamic.Service (hooks, "updated"
		// events, audit stamps), so the host must allow "update" as well as
		// "delete": a grant of delete alone must not write the row. Checked
		// before any effect; a host that grants only delete gets 403 here.
		if err := d.allow(ctx, op, "update"); err != nil {
			return Outcome{}, err
		}
	}
	if mode == DeleteHard {
		if d.HardDeleter == nil || !d.HardDeleteModels[model] {
			return Outcome{}, &ExecError{Kind: KindForbidden, Code: "hard_delete_disabled"}
		}
	} else {
		meta, err := st.TableMetadata(ctx, model)
		if err != nil {
			return Outcome{}, mapDynamicErr(err)
		}
		if !supportsSoftDelete(meta) {
			return Outcome{}, &ExecError{Kind: KindInvalid, Code: "soft_delete_unsupported"}
		}
	}
	unlock := d.lock(op)
	defer unlock()
	hits, err := matchRows(ctx, st, model, user, op.Route.Match, op.Values, sc, limit+1)
	if err != nil {
		return Outcome{}, err
	}
	if len(hits) == 0 {
		return Outcome{NotFound: true}, nil
	}
	if len(hits) > limit {
		return Outcome{}, &ExecError{Kind: KindConflict, Code: "too_many_matches"}
	}
	ids := make([]uuid.UUID, 0, len(hits))
	stale := 0
	for _, h := range hits {
		if op.Guard != nil {
			blocked, gerr := op.Guard.blocks(h[op.Guard.Field])
			if gerr != nil {
				return Outcome{}, gerr
			}
			if blocked {
				stale++ // a newer row than this delete event: it stays
				continue
			}
		}
		id, perr := uuid.Parse(fmt.Sprint(canonValue(h["id"])))
		if perr != nil {
			return Outcome{}, &ExecError{Kind: KindInternal, Code: "bad_record_id", Err: perr}
		}
		ids = append(ids, id)
	}
	if len(ids) == 0 && stale > 0 {
		return Outcome{Stale: true}, nil
	}
	n, lost := 0, 0
	for _, id := range ids {
		var derr error
		if mode == DeleteHard {
			derr = d.HardDeleter.HardDelete(ctx, model, user, id)
		} else {
			derr = sealAndDelete(ctx, st, user, op, mode, id)
		}
		if errors.Is(derr, dynamic.ErrStaleWrite) {
			lost++ // a newer event sealed the row after we listed it: it stays
			continue
		}
		if derr != nil && !errors.Is(derr, dynamic.ErrRecordNotFound) {
			return Outcome{}, mapDynamicErr(derr)
		}
		if derr == nil {
			n++
		}
	}
	if n == 0 && lost > 0 {
		return Outcome{Stale: true}, nil
	}
	if n == 0 {
		return Outcome{NotFound: true}, nil
	}
	return Outcome{RecordID: ids[0].String(), Affected: n}, nil
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

// findRow returns the single row matching every match field (nil when none
// does). Because query drops filters on unknown columns, every returned row is
// re-checked against the wanted values (typed comparison); rows outside the
// entity scope are invisible, so an upsert never touches another entity.
func findRow(ctx context.Context, st dynStore, model string, user modelbase.AuthUser, match []string, vals map[string]any, sc *dynamic.EntityScope) (map[string]any, error) {
	hits, err := matchRows(ctx, st, model, user, match, vals, sc, 3)
	if err != nil {
		return nil, err
	}
	if len(hits) > 1 {
		return nil, &ExecError{Kind: KindConflict, Code: "ambiguous_match"}
	}
	if len(hits) == 0 {
		return nil, nil
	}
	return hits[0], nil
}

// rowID is the id of a row from findRow; uuid.Nil for no row.
func rowID(row map[string]any) (uuid.UUID, error) {
	if row == nil {
		return uuid.Nil, nil
	}
	id, err := uuid.Parse(fmt.Sprint(canonValue(row["id"])))
	if err != nil {
		return uuid.Nil, &ExecError{Kind: KindInternal, Code: "bad_record_id", Err: err}
	}
	return id, nil
}

// matchValue returns the usable value of a match field. An explicit null is
// as missing as an absent key; an empty value must never reach List: query
// drops an OpEq filter on "" (no WHERE), which would return unfiltered rows.
// Zero and false are legitimate values.
func matchValue(vals map[string]any, f string) (any, error) {
	v, ok := vals[f]
	if !ok || v == nil {
		return nil, &ExecError{Kind: KindInvalid, Code: "match_value_missing"}
	}
	if sv, isStr := v.(string); isStr && strings.TrimSpace(sv) == "" {
		return nil, &ExecError{Kind: KindInvalid, Code: "match_value_empty"}
	}
	return v, nil
}

// scopeAllows reports whether the entity scope (nil: none) sees the row.
func scopeAllows(r map[string]any, sc *dynamic.EntityScope) bool {
	if sc == nil {
		return true
	}
	if b, has := r["branch_id"]; has && b != nil {
		return sc.Allows(fmt.Sprint(canonValue(b)))
	}
	return sc.Allows("")
}

// matchRows lists up to perPage rows whose match columns equal vals and that
// the entity scope allows.
func matchRows(ctx context.Context, st dynStore, model string, user modelbase.AuthUser, match []string, vals map[string]any, sc *dynamic.EntityScope, perPage int) ([]map[string]any, error) {
	filters := make(map[string]query.Filter, len(match))
	for _, f := range match {
		v, err := matchValue(vals, f)
		if err != nil {
			return nil, err
		}
		filters[f] = query.Filter{Op: query.OpEq, Value: filterString(v)}
	}
	rows, _, err := st.List(ctx, model, user, query.Params{Page: 1, PerPage: perPage, Filters: filters, SkipCount: true})
	if err != nil {
		return nil, mapDynamicErr(err)
	}
	var hits []map[string]any
	matched := 0
	for _, r := range rows {
		ok := true
		for _, f := range match {
			if !matchEqual(r[f], vals[f]) {
				ok = false
				break
			}
		}
		if !ok {
			continue
		}
		matched++
		if !scopeAllows(r, sc) {
			continue
		}
		hits = append(hits, r)
	}
	if matched == 0 && len(rows) > 0 {
		// Rows came back but none matches: the filter was ignored.
		return nil, &ExecError{Kind: KindInternal, Code: "match_field_unusable",
			Err: errors.New("match field is not a filterable column of " + model)}
	}
	return hits, nil
}

// canonValue normalizes what a database driver may return for the same value
// so two readings compare equal: []byte becomes a string, a uuid ([16]byte as
// pgx hands it, uuid.UUID, or its text in any case) its canonical lower-case
// text. Anything else is returned as is.
func canonValue(v any) any {
	switch t := v.(type) {
	case [16]byte:
		return uuid.UUID(t).String()
	case uuid.UUID:
		return t.String()
	case *uuid.UUID:
		if t == nil {
			return nil
		}
		return t.String()
	case []byte:
		return canonString(string(t))
	case string:
		return canonString(t)
	case *string:
		if t == nil {
			return nil
		}
		return canonString(*t)
	}
	return v
}

func canonString(s string) string {
	if len(s) == 36 {
		if u, err := uuid.Parse(s); err == nil {
			return u.String()
		}
	}
	return s
}

// asInstant reads v as an instant (time.Time or an RFC 3339 / SQL timestamp
// string).
func asInstant(v any) (time.Time, bool) {
	switch t := v.(type) {
	case time.Time:
		return t, true
	case string:
		if n, err := guardNormalizeString(t, true); err == nil {
			if tm, ok := n.(time.Time); ok {
				return tm, true
			}
		}
	}
	return time.Time{}, false
}

// valuesEqual compares by type: numbers numerically, strings and bools
// exactly (after canonValue: bytes, uuid case and form); instants as instants
// whatever their zone; anything else by its printed form.
func valuesEqual(a, b any) bool {
	a, b = canonValue(a), canonValue(b)
	if ta, ok := a.(time.Time); ok {
		tb, ok := asInstant(b)
		return ok && ta.Round(time.Microsecond).Equal(tb.Round(time.Microsecond))
	}
	if tb, ok := b.(time.Time); ok {
		ta, ok := asInstant(a)
		return ok && ta.Round(time.Microsecond).Equal(tb.Round(time.Microsecond))
	}
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
	case errors.Is(err, dynamic.ErrValidation), errors.Is(err, dynamic.ErrInvalidInput), errors.Is(err, dynamic.ErrAppendOnly),
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
