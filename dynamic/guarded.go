package dynamic

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/asteby/metacore-kernel/modelbase"
)

// Guarded writes: UpdateGuarded and DeleteSealed are additive variants of
// Update and Delete for callers that apply a write ONLY IF the row they read is
// still the row they judge (an inbound event carrying a version or an instant).
// They run the same validation, hooks, canonical events and audit stamps as
// Update/Delete; what they add is that the "is it stale?" decision and the write
// happen under ONE row lock held by ONE transaction, so it holds across
// processes and replicas, not just inside one process.
//
// Mechanism. The transaction starts with a no-op UPDATE of the guarded column
// (SET col = col WHERE id = ? AND <tenant scope>): in Postgres it takes the
// row's write lock and a concurrent writer waits for the commit; in SQLite it
// takes the database write lock. Only then is the row loaded, so the value
// handed to WriteGuard.Stale is the committed one that the write will replace,
// never an older read. A stale row aborts the transaction (ErrStaleWrite, no
// event, no hook after). The decision stays in Go (not in a SQL "WHERE col <=
// ?") because the column may be an instant stored as text, an integer or NULL,
// and the caller's comparison is typed and fails closed on a value it cannot
// read; the lock is what makes that decision safe.

// ErrStaleWrite is returned by UpdateGuarded and DeleteSealed when
// WriteGuard.Stale judged the locked row newer than the write: nothing was
// written and no event was published.
var ErrStaleWrite = errors.New("dynamic: guarded write is stale")

// WriteGuard decides whether a guarded write must be skipped.
type WriteGuard struct {
	// Field is the guarded column (its json/column name). It must be a column
	// of the model.
	Field string
	// Stale receives the value of Field in the row as it is committed AND
	// locked (nil for NULL) and reports whether the write must be skipped. Its
	// error aborts the write and is returned unchanged.
	Stale func(current any) (bool, error)
}

func (g *WriteGuard) check() error {
	if g == nil || g.Field == "" || g.Stale == nil {
		return fmt.Errorf("%w: write guard needs Field and Stale", ErrInvalidInput)
	}
	return nil
}

// updateOpts tunes update() (service.go). A nil *updateOpts is a plain Update.
type updateOpts struct {
	guard *WriteGuard
	// then runs on the same transaction right after the save (DeleteSealed's
	// soft delete): it commits or rolls back together with the update.
	then func(tx *gorm.DB) error
	// after, when set, receives the row as saved.
	after *map[string]any
}

func (o *updateOpts) writeGuard() *WriteGuard {
	if o == nil {
		return nil
	}
	return o.guard
}

func (o *updateOpts) needsTx() bool { return o != nil && (o.guard != nil || o.then != nil) }

// claimRow takes the write lock of the row on tx (see the file comment). It
// answers ErrRecordNotFound when no live row with that id is visible to the
// caller's tenant scope.
func (s *Service) claimRow(ctx context.Context, tx *gorm.DB, tableName string, instance any, user modelbase.AuthUser, id uuid.UUID, field string) error {
	if _, ok := structColumnSet(instance)[field]; !ok {
		return fmt.Errorf("%w: guard field %q is not a column of the model", ErrInvalidInput, field)
	}
	q := s.scope.ScopeQuery(s.tableDB(ctx, tx.WithContext(ctx), tableName, instance), user)
	res := q.Model(instance).Where("id = ?", id).
		UpdateColumn(field, gorm.Expr("?", clause.Column{Name: field}))
	if res.Error != nil {
		return fmt.Errorf("dynamic: lock row: %w", res.Error)
	}
	if res.RowsAffected > 0 {
		return nil
	}
	// Some drivers count only changed rows (a no-op UPDATE reports 0): tell a
	// missing row from an unchanged one.
	var n int64
	cq := s.scope.ScopeQuery(s.tableDB(ctx, tx.WithContext(ctx), tableName, instance), user)
	if err := cq.Model(instance).Where("id = ?", id).Count(&n).Error; err != nil {
		return fmt.Errorf("dynamic: lock row: %w", err)
	}
	if n == 0 {
		return ErrRecordNotFound
	}
	return nil
}

// UpdateGuarded is Update applied only if g.Stale does not judge the row stale.
// The row is locked first and g.Stale sees the locked, committed value; a stale
// row yields ErrStaleWrite with nothing written or published. ErrRecordNotFound
// means the row is gone (deleted since the caller read it). Hooks, validation,
// the canonical "updated" event and audit stamps are exactly Update's. g is
// required.
func (s *Service) UpdateGuarded(ctx context.Context, model string, user modelbase.AuthUser, id uuid.UUID, input map[string]any, g *WriteGuard) (map[string]any, error) {
	if err := g.check(); err != nil {
		return nil, err
	}
	return s.update(ctx, model, user, id, input, &updateOpts{guard: g})
}

// DeleteSealed soft-deletes a row after stamping it with seal (typically the
// guarded column with the event's value, so the tombstone carries the date of
// the deletion), all in ONE transaction: a failure of either statement rolls
// both back, so no live row is left carrying the sealed value. Exactly one
// canonical "updated" (the seal) and one "deleted" event are published, after
// the commit and only if it succeeded. The caller needs update and delete
// permission. g (optional) is checked on the locked row like UpdateGuarded:
// stale answers ErrStaleWrite and the row is neither sealed nor deleted.
// ErrRecordNotFound means the row is gone.
func (s *Service) DeleteSealed(ctx context.Context, model string, user modelbase.AuthUser, id uuid.UUID, seal map[string]any, g *WriteGuard) error {
	if len(seal) == 0 {
		return fmt.Errorf("%w: empty seal", ErrInvalidInput)
	}
	if g != nil {
		if err := g.check(); err != nil {
			return err
		}
	}
	instance, tableMeta, err := s.resolveModel(ctx, model)
	if err != nil {
		return err
	}
	if err := s.authorize(ctx, user, model, instance, modelbase.AccessDelete); err != nil {
		return err
	}
	if ctx, err = s.requireDeleteReason(ctx, model); err != nil {
		return err
	}
	if err := s.refuseIfAppendOnly(ctx, model, "delete"); err != nil {
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
	if err := s.checkDeletable(ctx, model, tableName, instance, user, id); err != nil {
		return err
	}

	hasBy := s.stampsDeletedBy(ctx, tableName, instance)
	var sealed map[string]any
	_, err = s.update(ctx, model, user, id, cloneMap(seal), &updateOpts{
		guard: g,
		after: &sealed,
		then: func(tx *gorm.DB) error {
			if err := s.softDeleteIn(ctx, tx, tableName, instance, user, id, hasBy); err != nil {
				return fmt.Errorf("dynamic: delete: %w", err)
			}
			return nil
		},
	})
	if err != nil {
		return err
	}
	return s.afterDelete(ctx, model, tableMeta, user, id, sealed, hc)
}
