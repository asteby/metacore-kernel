package inbound

import (
	"context"
	"database/sql/driver"
	"fmt"
	"reflect"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

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

// blocks reports whether the event is stale against cur, the guarded column of
// the stored row: "event op cur" does not hold. A null or incomparable cur
// (other kind, unparseable) never blocks: the guard is opt-in by data.
func (g *OperationGuard) blocks(cur any) bool {
	if v, ok := cur.(driver.Valuer); ok {
		cur, _ = v.Value()
	}
	if rv := reflect.ValueOf(cur); rv.Kind() == reflect.Ptr {
		if rv.IsNil() {
			return false
		}
		cur = rv.Elem().Interface()
	}
	if s, ok := cur.(string); ok {
		for _, l := range []string{time.RFC3339Nano, "2006-01-02 15:04:05.999999999Z07:00", "2006-01-02 15:04:05.999999999"} {
			if t, err := time.Parse(l, s); err == nil {
				cur = t
				break
			}
		}
	}
	if cur == nil {
		return false
	}
	c, err := ingest.CompareCursor(g.Value, cur)
	if err != nil {
		return false
	}
	if g.Op == GuardGT {
		return c <= 0
	}
	return c < 0
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

// TombstoneFinder is the optional port that lets a guard see soft-deleted
// rows (dynamic.Service lists live rows only), so a late "updated" cannot
// resurrect a deleted row. It returns up to limit soft-deleted rows of org
// whose columns equal where, most recently deleted first. The host supplies it
// (DynamicExecutor.Tombstones); GormTombstones is a ready implementation.
type TombstoneFinder interface {
	Tombstones(ctx context.Context, model string, org uuid.UUID, where map[string]any, limit int) ([]map[string]any, error)
}

// GormTombstones implements TombstoneFinder over the model's table.
type GormTombstones struct {
	DB *gorm.DB
	// Table maps a model name to its table; nil means the same name.
	Table func(model string) string
}

// Tombstones implements TombstoneFinder.
func (g GormTombstones) Tombstones(ctx context.Context, model string, org uuid.UUID, where map[string]any, limit int) ([]map[string]any, error) {
	table := model
	if g.Table != nil {
		table = g.Table(model)
	}
	var rows []map[string]any
	err := g.DB.WithContext(ctx).Table(table).Where(where).
		Where("organization_id = ?", org.String()).Where("deleted_at IS NOT NULL").
		Order("deleted_at DESC").Limit(limit).Find(&rows).Error
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

// tombstoneStale reports whether the most recent tombstone of the row an
// upsert/create would produce is newer than the event (same organization and
// entity/branch scope, same match). It is what stops a late event from
// resurrecting a deleted row.
func (d *DynamicExecutor) tombstoneStale(ctx context.Context, st dynStore, op Operation, sc *dynamic.EntityScope) (bool, error) {
	g := op.Guard
	if len(g.Match) == 0 {
		return false, nil
	}
	tf := d.tombstoner(st)
	if tf == nil {
		return false, &ExecError{Kind: KindInternal, Code: "tombstones_unavailable"}
	}
	where := make(map[string]any, len(g.Match))
	for _, f := range g.Match {
		v, err := matchValue(op.Values, f)
		if err != nil {
			return false, err
		}
		where[f] = v
	}
	rows, err := tf.Tombstones(ctx, op.Route.Model, op.Org, where, 20)
	if err != nil {
		return false, mapDynamicErr(err)
	}
	for _, r := range rows {
		same := true
		for _, f := range g.Match {
			if !valuesEqual(r[f], op.Values[f]) {
				same = false
				break
			}
		}
		if same && scopeAllows(r, sc) {
			return g.blocks(r[g.Field]), nil // the most recent tombstone decides
		}
	}
	return false, nil
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

// sealAndDelete stamps the guarded column with the event's value and then
// soft-deletes, so the tombstone carries the deletion's date. dynamic.Service
// exposes no transaction across Update and Delete: the pair runs under the
// per-key lock and the stamp is written first, so a crash in between leaves a
// live row with the newer value (a retry with gte converges).
func sealAndDelete(ctx context.Context, st dynStore, user modelbase.AuthUser, op Operation, mode string, id uuid.UUID) error {
	if g := op.Guard; g != nil && mode == DeleteSoft {
		if _, err := st.Update(ctx, op.Route.Model, user, id, map[string]any{g.Field: g.Value}); err != nil {
			return err
		}
	}
	return st.Delete(ctx, op.Route.Model, user, id)
}
