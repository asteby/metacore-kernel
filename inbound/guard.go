package inbound

import (
	"context"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/asteby/metacore-kernel/dynamic"
	"github.com/asteby/metacore-kernel/modelbase"
	"github.com/asteby/metacore-kernel/runtime/ingest"
)

// OperationGuard is a Guard evaluated against one event (see Operation.Guard).
type OperationGuard struct {
	Field string
	Op    string // GuardGTE | GuardGT
	// Value is the event's instant (RFC 3339 string) or integer.
	Value any
	// Match lists the columns that identify the row (the action's match); it
	// keys the tombstone lookup when no live row exists.
	Match []string
	// Create marks a record.create (run as an upsert on its idempotency
	// column): the row found by that column is this event's own earlier
	// attempt, so only tombstones are compared.
	Create bool
}

// Stable codes of the guard (see docs/inbound-events.md).
const (
	// CodeGuardValueInvalid: the payload value is present but is neither an
	// RFC 3339 instant nor an integer (422, before any effect).
	CodeGuardValueInvalid = "guard_value_invalid"
	// CodeGuardValueFuture: the event's instant is later than now+GuardMaxSkew
	// (422): it would freeze the row against every legitimate later event.
	CodeGuardValueFuture = "guard_value_future"
	// CodeGuardRowUnreadable: the stored value is not null but cannot be
	// compared with the event's (409, fail closed): nothing is written.
	CodeGuardRowUnreadable = "guard_row_value_unreadable"
)

// defaultGuardMaxSkew is how far into the future an event's instant may be
// (DynamicExecutor.GuardMaxSkew = 0).
const defaultGuardMaxSkew = 24 * time.Hour

// guardCursorLayouts are the layouts a database driver may hand back for a
// timestamp column rendered as text: RFC 3339, the Postgres/SQLite
// "YYYY-MM-DD hh:mm:ss[.f]+00[:00]" forms (short offsets such as "+00" too)
// and zone-less ones (read as UTC). The event's value only accepts the first.
var guardCursorLayouts = []string{
	time.RFC3339Nano,
	"2006-01-02 15:04:05.999999999Z07:00",
	"2006-01-02 15:04:05.999999999Z07",
	"2006-01-02 15:04:05.999999999Z0700",
	"2006-01-02 15:04:05.999999999",
	"2006-01-02T15:04:05.999999999",
}

// guardNormalize reduces v to what the guard compares: an int64 or a time.Time
// rounded to the microsecond (the precision of a Postgres timestamp, so a
// nanosecond event ties with its stored copy). A null (nil, nil pointer, nil
// Valuer) yields (nil, nil). row=true is the lenient reading of a database
// value ([]byte, "5" for an integer column, SQL timestamp text); the event's
// value (row=false) must be a JSON-typed RFC 3339 string or a whole number.
func guardNormalize(v any, row bool) (any, error) {
	if isNilPtr(v) {
		return nil, nil
	}
	if vl, ok := v.(driver.Valuer); ok {
		nv, err := vl.Value()
		if err != nil {
			return nil, err
		}
		v = nv
	}
	if rv := reflect.ValueOf(v); rv.IsValid() && rv.Kind() == reflect.Ptr {
		if rv.IsNil() {
			return nil, nil
		}
		v = rv.Elem().Interface()
	}
	switch t := v.(type) {
	case nil:
		return nil, nil
	case []byte:
		if !row {
			return nil, errors.New("unsupported type []uint8")
		}
		return guardNormalizeString(string(t), row)
	case string:
		return guardNormalizeString(t, row)
	case time.Time:
		return t.Round(time.Microsecond), nil
	case json.Number:
		n, err := t.Int64()
		if err != nil {
			return nil, errors.New("not an integer")
		}
		return n, nil
	case int:
		return int64(t), nil
	case int8:
		return int64(t), nil
	case int16:
		return int64(t), nil
	case int32:
		return int64(t), nil
	case int64:
		return t, nil
	case uint8:
		return int64(t), nil
	case uint16:
		return int64(t), nil
	case uint32:
		return int64(t), nil
	case uint:
		if uint64(t) > math.MaxInt64 {
			return nil, errors.New("integer overflows int64")
		}
		return int64(t), nil
	case uint64:
		if t > math.MaxInt64 {
			return nil, errors.New("integer overflows int64")
		}
		return int64(t), nil
	case float32:
		return guardFloat(float64(t))
	case float64:
		return guardFloat(t)
	}
	return nil, fmt.Errorf("unsupported type %T", v)
}

func guardFloat(f float64) (any, error) {
	if f != math.Trunc(f) || math.Abs(f) > 1<<53 {
		return nil, errors.New("not a safe integer")
	}
	return int64(f), nil
}

func guardNormalizeString(s string, row bool) (any, error) {
	s = strings.TrimSpace(s)
	layouts := guardCursorLayouts[:1]
	if row {
		layouts = guardCursorLayouts
	}
	for _, l := range layouts {
		if t, err := time.Parse(l, s); err == nil {
			return t.Round(time.Microsecond), nil
		}
	}
	if row {
		if n, err := strconv.ParseInt(s, 10, 64); err == nil {
			return n, nil
		}
	}
	return nil, errors.New("not an RFC 3339 instant nor an integer")
}

func isNilPtr(v any) bool {
	rv := reflect.ValueOf(v)
	return rv.IsValid() && rv.Kind() == reflect.Ptr && rv.IsNil()
}

// blocks reports whether the event is stale against cur, the guarded column of
// the stored row: "event op cur" does not hold. A null cur never blocks (the
// guard is opt-in by data). A cur that is not null but cannot be read, or is
// of another kind than the event's (an integer against an instant), FAILS
// CLOSED with guard_row_value_unreadable: nothing is written. Instants compare
// at microsecond precision.
func (g *OperationGuard) blocks(cur any) (bool, error) {
	cv, err := guardNormalize(cur, true)
	if err != nil {
		return false, &ExecError{Kind: KindConflict, Code: CodeGuardRowUnreadable, Err: err}
	}
	if cv == nil {
		return false, nil
	}
	ev, err := guardNormalize(g.Value, false)
	if err != nil || ev == nil {
		return false, &ExecError{Kind: KindInvalid, Code: CodeGuardValueInvalid, Err: err}
	}
	c, err := ingest.CompareCursor(ev, cv)
	if err != nil {
		return false, &ExecError{Kind: KindConflict, Code: CodeGuardRowUnreadable, Err: err}
	}
	if g.Op == GuardGT {
		return c <= 0, nil
	}
	return c < 0, nil
}

// checkFuture rejects an event whose instant is later than now+maxSkew
// (maxSkew 0 = 24h; negative disables the bound). Integers are not bounded:
// there is no clock to compare a version with (bound them in the JSON schema
// with "maximum").
func (g *OperationGuard) checkFuture(now time.Time, maxSkew time.Duration) error {
	if maxSkew < 0 {
		return nil
	}
	if maxSkew == 0 {
		maxSkew = defaultGuardMaxSkew
	}
	ev, err := guardNormalize(g.Value, false)
	if err != nil || ev == nil {
		return &ExecError{Kind: KindInvalid, Code: CodeGuardValueInvalid, Err: err}
	}
	if t, ok := ev.(time.Time); ok && t.After(now.Add(maxSkew)) {
		return &ExecError{Kind: KindInvalid, Code: CodeGuardValueFuture}
	}
	return nil
}

// GuardSupporter is an optional Executor port: an Executor that implements it
// and answers true honours Operation.Guard. A contract with a guard is refused
// at registration otherwise, so a custom executor can never apply a stale
// event just because it ignores the field. DynamicExecutor supports it.
type GuardSupporter interface {
	SupportsGuard() bool
}

// SupportsGuard implements GuardSupporter.
func (d *DynamicExecutor) SupportsGuard() bool { return true }

func executorGuards(ex Executor) bool {
	s, ok := ex.(GuardSupporter)
	return ok && s.SupportsGuard()
}

// branchColumn is the row key of the branch scope (see scopeAllows).
const branchColumn = "branch_id"

// TombstoneQuery is one tombstone lookup. The finder must apply EVERY
// condition in the database, before Limit, and order by the deletion date,
// most recent first: a post-filter after the limit lets foreign tombstones push
// the row's own out of the page.
type TombstoneQuery struct {
	Model string
	Org   uuid.UUID
	// Where holds the match columns the tombstone must equal.
	Where map[string]any
	// DeletedAtColumn is the soft-delete column (modelbase Audit.DeletedAt);
	// empty means "deleted_at".
	DeletedAtColumn string
	// BranchColumn is the branch column of the model, "" when it has none.
	// Branch (when set) restricts to tombstones of exactly that branch; Scope
	// (the entity scope, nil = none) to the branches it allows.
	BranchColumn string
	Branch       string
	Scope        *dynamic.EntityScope
	Limit        int
}

// TombstoneFinder is the optional port that lets a guard see soft-deleted
// rows (dynamic.Service lists live rows only), so a late "updated" cannot
// resurrect a deleted row. It returns up to q.Limit soft-deleted rows of the
// organization matching q, most recently deleted first. The host supplies it
// (DynamicExecutor.Tombstones); GormTombstones is a ready implementation.
type TombstoneFinder interface {
	Tombstones(ctx context.Context, q TombstoneQuery) ([]map[string]any, error)
}

var identRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,62}(\.[A-Za-z_][A-Za-z0-9_]{0,62})?$`)

// GormTombstones implements TombstoneFinder over the model's table.
type GormTombstones struct {
	DB *gorm.DB
	// Table maps a model name to its table; nil means the same name.
	Table func(model string) string
	// OrgColumn is the organization column (default "organization_id"): the
	// model metadata does not name it.
	OrgColumn string
}

// Tombstones implements TombstoneFinder. Table and column names are validated
// as identifiers and quoted; values are bound parameters.
func (g GormTombstones) Tombstones(ctx context.Context, q TombstoneQuery) ([]map[string]any, error) {
	table := q.Model
	if g.Table != nil {
		table = g.Table(q.Model)
	}
	if !identRe.MatchString(table) {
		return nil, fmt.Errorf("tombstones: invalid table name")
	}
	orgCol, delCol := g.OrgColumn, q.DeletedAtColumn
	if orgCol == "" {
		orgCol = "organization_id"
	}
	if delCol == "" {
		delCol = "deleted_at"
	}
	cols := []string{orgCol, delCol}
	if q.BranchColumn != "" {
		cols = append(cols, q.BranchColumn)
	}
	for k := range q.Where {
		cols = append(cols, k)
	}
	for _, c := range cols {
		if !fieldRe.MatchString(c) {
			return nil, fmt.Errorf("tombstones: invalid column name")
		}
	}
	tx := g.DB.WithContext(ctx).Table(table).Where(q.Where).
		Where("? = ?", clause.Column{Name: orgCol}, q.Org.String()).
		Where("? IS NOT NULL", clause.Column{Name: delCol})
	if q.BranchColumn != "" {
		bc := clause.Column{Name: q.BranchColumn}
		if q.Branch != "" {
			tx = tx.Where("? = ?", bc, q.Branch)
		}
		if s := q.Scope; s != nil && (s.Restricted || s.Unresolved) {
			ids := s.BranchIDs
			if s.Unresolved {
				ids = nil
			}
			switch {
			case len(ids) > 0 && s.IncludeUnassigned:
				tx = tx.Where("(? IN ? OR ? IS NULL)", bc, ids, bc)
			case len(ids) > 0:
				tx = tx.Where("? IN ?", bc, ids)
			case s.IncludeUnassigned:
				tx = tx.Where("? IS NULL", bc)
			default:
				return nil, nil // the scope sees no tombstone
			}
		}
	}
	limit := q.Limit
	if limit <= 0 {
		limit = 20
	}
	var rows []map[string]any
	err := tx.Order(clause.OrderByColumn{Column: clause.Column{Name: delCol}, Desc: true}).
		Limit(limit).Find(&rows).Error
	return rows, err
}

// tombstoner returns the host's TombstoneFinder, or the store when it is one.
func (d *DynamicExecutor) tombstoner(st dynStore) TombstoneFinder {
	if d.Tombstones != nil {
		return d.Tombstones
	}
	tf, _ := st.(TombstoneFinder)
	return tf
}

// eventBranch is the branch the event's row lives in: the operation's branch,
// else the mapped branch_id.
func eventBranch(op Operation) string {
	if op.Branch != "" {
		return op.Branch
	}
	if v, ok := op.Values[branchColumn]; ok && v != nil {
		if s, isStr := canonValue(v).(string); isStr {
			return s
		}
	}
	return ""
}

// tombstoneRows returns the soft-deleted rows of the organization whose match
// columns equal the operation values, most recently deleted first, up to limit.
// It applies the branch / entity scope of the operation: a tombstone of another
// branch is never returned. The finder filters in the database; the re-check
// here is defence in depth for a host finder that does not.
func (d *DynamicExecutor) tombstoneRows(ctx context.Context, st dynStore, op Operation, match []string, sc *dynamic.EntityScope, limit int) ([]map[string]any, error) {
	tf := d.tombstoner(st)
	if tf == nil {
		return nil, &ExecError{Kind: KindInternal, Code: "tombstones_unavailable"}
	}
	where := make(map[string]any, len(match))
	for _, f := range match {
		v, err := matchValue(op.Values, f)
		if err != nil {
			return nil, err
		}
		where[f] = v
	}
	meta, err := st.TableMetadata(ctx, op.Route.Model)
	if err != nil {
		return nil, mapDynamicErr(err)
	}
	if meta == nil {
		return nil, &ExecError{Kind: KindInternal, Code: "tombstones_unavailable"}
	}
	q := TombstoneQuery{Model: op.Route.Model, Org: op.Org, Where: where, Scope: sc, Limit: limit}
	if supportsSoftDelete(meta) {
		q.DeletedAtColumn = meta.Audit.DeletedAt
	}
	for _, c := range meta.Columns {
		if c.Key == branchColumn {
			q.BranchColumn, q.Branch = branchColumn, eventBranch(op)
			break
		}
	}
	rows, err := tf.Tombstones(ctx, q)
	if err != nil {
		return nil, mapDynamicErr(err)
	}
	var out []map[string]any
	for _, r := range rows {
		same := true
		for _, f := range match {
			if !valuesEqual(r[f], op.Values[f]) {
				same = false
				break
			}
		}
		if !same || !scopeAllows(r, sc) {
			continue
		}
		if q.Branch != "" && !valuesEqual(r[branchColumn], q.Branch) {
			continue
		}
		out = append(out, r)
	}
	return out, nil
}

// tombstoneStale reports whether the most recent tombstone of the row an
// upsert/create would produce is newer than the event (same organization,
// same match, and the branch / entity scope of the operation: a tombstone of
// another branch never blocks). It is what stops a late event from
// resurrecting a deleted row.
func (d *DynamicExecutor) tombstoneStale(ctx context.Context, st dynStore, op Operation, sc *dynamic.EntityScope) (bool, error) {
	g := op.Guard
	if len(g.Match) == 0 {
		return false, nil
	}
	rows, err := d.tombstoneRows(ctx, st, op, g.Match, sc, 20)
	if err != nil {
		return false, err
	}
	if len(rows) > 0 {
		return g.blocks(rows[0][g.Field]) // the most recent tombstone decides
	}
	return false, nil
}

// guardStale evaluates the guard of an upsert/update against the stored row
// (nil: none, then the tombstones decide). A record.create compares only
// tombstones: the row found by its idempotency column is its own attempt.
func (d *DynamicExecutor) guardStale(ctx context.Context, st dynStore, op Operation, sc *dynamic.EntityScope, row map[string]any) (bool, error) {
	if op.Guard == nil {
		return false, nil
	}
	if row != nil && !op.Guard.Create {
		return op.Guard.blocks(row[op.Guard.Field])
	}
	if row != nil {
		return false, nil
	}
	return d.tombstoneStale(ctx, st, op, sc)
}

// checkGuard (registration) requires the guarded column to be writable and,
// except in record.delete, mapped; a create/upsert on a soft-delete model also
// needs a way to read tombstones.
func (d *DynamicExecutor) checkGuard(ctx context.Context, who string, a Action) error {
	if err := d.checkWritable(ctx, who, a.Model, map[string]string{a.Guard.Field: ""}); err != nil {
		return fmt.Errorf("guard.field: %w", err)
	}
	if _, ok := a.Mapping[a.Guard.Field]; !ok && a.Do != ActionRecordDelete {
		return fmt.Errorf("%w: %s: guard_field_not_mapped: guard.field %q is not in mapping", ErrInvalidContract, who, a.Guard.Field)
	}
	if (a.Do == ActionRecordUpsert || a.Do == ActionRecordCreate) && len(a.Match) > 0 {
		st, err := d.st()
		if err != nil {
			return err
		}
		meta, err := st.TableMetadata(ctx, a.Model)
		if err == nil && supportsSoftDelete(meta) && d.tombstoner(st) == nil {
			return fmt.Errorf("%w: %s: guard_tombstones_unavailable: model %q soft-deletes and the executor has no TombstoneFinder", ErrInvalidContract, who, a.Model)
		}
	}
	return nil
}

// writeGuard is the guard as dynamic.Service judges it on the locked row:
// stale when the event does not satisfy "event op row[field]". A record.create
// (Create) compares only tombstones, never the row it finds: that row is its
// own earlier attempt.
func (g *OperationGuard) writeGuard() *dynamic.WriteGuard {
	return &dynamic.WriteGuard{Field: g.Field, Stale: func(cur any) (bool, error) {
		if g.Create {
			return false, nil
		}
		return g.blocks(cur)
	}}
}

// sealAndDelete soft-deletes the row, stamping the guarded column with the
// event's value first so the tombstone carries the deletion's date. With a
// guard both statements run in ONE transaction (dynamic.Service.DeleteSealed):
// a crash or a failed delete rolls the seal back too, so no live row is left
// holding the event's value (which a "gt" retry would then report stale
// forever). The guard is judged again on the locked row: a newer event that
// won the race makes it return dynamic.ErrStaleWrite and nothing is written.
func sealAndDelete(ctx context.Context, st dynStore, user modelbase.AuthUser, op Operation, mode string, id uuid.UUID) error {
	if g := op.Guard; g != nil && mode == DeleteSoft {
		return st.DeleteSealed(ctx, op.Route.Model, user, id, map[string]any{g.Field: g.Value}, g.writeGuard())
	}
	return st.Delete(ctx, op.Route.Model, user, id)
}
