package dynamic

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"

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
// Mechanism. The transaction starts by claiming the row. Postgres and
// MySQL/MariaDB take its write lock with SELECT id ... WHERE id = ? AND
// <tenant scope> FOR UPDATE (no new tuple version, so no UPDATE triggers or CDC
// noise); a concurrent writer waits for the commit. SQLite has no row locks:
// it runs a no-op UPDATE of the guarded column (SET col = col), which takes the
// database write lock. A row of another tenant is never matched, so no lock is
// taken on it and the answer is ErrRecordNotFound. Only then is the row
// loaded, so the value
// handed to WriteGuard.Stale is the committed one that the write will replace,
// never an older read. A stale row aborts the transaction (ErrStaleWrite, no
// event, no hook after). The decision stays in Go (not in a SQL "WHERE col <=
// ?") because the column may be an instant stored as text, an integer or NULL,
// and the caller's comparison is typed and fails closed on a value it cannot
// read; the lock is what makes that decision safe.
//
// Connections. Everything that runs while the transaction is open uses the
// transaction's own connection (see txHandle / queryDB): validation lookups,
// extension reads and schema introspection included. A guarded write never
// needs a second connection, so a pool of one cannot block and N concurrent
// writes on a pool of N cannot starve each other.
//
// Events. A guarded UpdateGuarded publishes "updated" like Update.
// DeleteSealed publishes ONLY "deleted": the seal is an implementation detail
// of the tombstone and, by the time anything is published, the row is already
// deleted (an "updated" for it could not be re-read by its subscribers).

// ErrStaleWrite is returned by UpdateGuarded and DeleteSealed when
// WriteGuard.Stale judged the locked row newer than the write: nothing was
// written and no event was published.
var ErrStaleWrite = errors.New("dynamic: guarded write is stale")

// WriteGuard decides whether a guarded write must be skipped.
type WriteGuard struct {
	// Field is the guarded column (its json/column name). It must be a column
	// of the model, and its json tag must not carry omitempty/omitzero (see
	// Service.CheckGuardField).
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
	// soft delete): it commits or rolls back together with the update. Its ctx
	// carries the transaction, so a helper that asks "which handle?" gets tx
	// and never waits for a second connection.
	then func(ctx context.Context, tx *gorm.DB) error
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

// deleting reports that the update is DeleteSealed's seal: the row ends the
// transaction tombstoned, so update() publishes no "updated" for it.
func (o *updateOpts) deleting() bool { return o != nil && o.then != nil }

// txCtxKey keys the transaction handle a guarded write puts in the context it
// hands to the code that runs inside the transaction.
type txCtxKey struct{}

// withTxHandle returns ctx carrying tx as the handle of the open transaction.
func withTxHandle(ctx context.Context, tx *gorm.DB) context.Context {
	return context.WithValue(ctx, txCtxKey{}, tx)
}

// queryDB is the handle for a read that may run while a guarded write holds a
// transaction: that transaction when ctx carries one, s.db otherwise (every
// plain Update/Create/Delete: nothing changes for them). A query that went
// through s.db inside the transaction would need a SECOND connection: with a
// pool of one it blocks forever, and with N concurrent writes on a pool of N
// every writer holds one connection and waits for another (starvation).
func (s *Service) queryDB(ctx context.Context) *gorm.DB {
	if tx, ok := ctx.Value(txCtxKey{}).(*gorm.DB); ok && tx != nil {
		return tx
	}
	return s.db
}

// guardFieldIssue explains why field cannot be a guarded column of instance
// ("" when it can). The decision reads the value from the row as JSON (toMap):
// a non-pointer field tagged omitempty/omitzero disappears from it while it
// holds its zero value, so it would be read as NULL and never block.
func guardFieldIssue(instance any, field string) string {
	t := reflect.TypeOf(instance)
	for t != nil && t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	if t == nil || t.Kind() != reflect.Struct {
		return "the model is not a struct"
	}
	issue := ""
	found := false
	var walk func(reflect.Type)
	walk = func(t reflect.Type) {
		for i := 0; i < t.NumField() && !found; i++ {
			f := t.Field(i)
			if f.Anonymous {
				ft := f.Type
				for ft.Kind() == reflect.Ptr {
					ft = ft.Elem()
				}
				if ft.Kind() == reflect.Struct {
					walk(ft)
				}
				continue
			}
			if columnNameFromField(f) != field {
				continue
			}
			found = true
			if f.Type.Kind() == reflect.Ptr || f.Type.Kind() == reflect.Interface {
				// nil is NULL and a value is always present: omitting nil is harmless.
				continue
			}
			tag := f.Tag.Get("json")
			for _, opt := range strings.Split(tag, ",")[1:] {
				if opt == "omitempty" || opt == "omitzero" {
					issue = fmt.Sprintf("json tag %q drops the column from the row while it holds its zero value", tag)
				}
			}
		}
	}
	walk(t)
	return issue
}

// checkGuardColumn is the model-side validation of a guarded column.
func checkGuardColumn(instance any, field string) error {
	if _, ok := structColumnSet(instance)[field]; !ok {
		return fmt.Errorf("%w: guard field %q is not a column of the model", ErrInvalidInput, field)
	}
	if issue := guardFieldIssue(instance, field); issue != "" {
		return fmt.Errorf("%w: guard_field_not_comparable: guard field %q: %s", ErrInvalidInput, field, issue)
	}
	return nil
}

// CheckGuardField reports (nil = fine) whether field can be the column of a
// WriteGuard of model: it must be a column of the model (json:"-" is not) and
// its json tag must not carry omitempty/omitzero (a non-pointer field with it
// vanishes from the row at its zero value and would be read as NULL, which
// never blocks). Callers that register guards ahead of time (inbound
// contracts) use it to fail at registration instead of on the first event; the
// guarded writes check it again on every call. The error wraps ErrInvalidInput.
func (s *Service) CheckGuardField(ctx context.Context, model, field string) error {
	instance, _, err := s.resolveModel(ctx, model)
	if err != nil {
		return err
	}
	return checkGuardColumn(instance, field)
}

// claimRow takes the write lock of the row on tx (see the file comment). It
// answers ErrRecordNotFound when no live row with that id is visible to the
// caller's tenant scope (the lock is never taken on a row of another tenant).
func (s *Service) claimRow(ctx context.Context, tx *gorm.DB, tableName string, instance any, user modelbase.AuthUser, id uuid.UUID, field string) error {
	if err := checkGuardColumn(instance, field); err != nil {
		return err
	}
	if lockBySelect(tx) {
		row := map[string]any{}
		q := s.scope.ScopeQuery(s.tableDB(ctx, tx.WithContext(ctx), tableName, instance), user)
		err := q.Model(instance).Select("id").Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ?", id).Take(&row).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return ErrRecordNotFound
		}
		if err != nil {
			return fmt.Errorf("dynamic: lock row: %w", err)
		}
		return nil
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

// lockBySelect reports whether the dialect locks a row with SELECT ... FOR
// UPDATE (Postgres, MySQL/MariaDB). SQLite and unknown dialects take the
// no-op UPDATE.
func lockBySelect(tx *gorm.DB) bool {
	if tx == nil || tx.Dialector == nil {
		return false
	}
	switch tx.Dialector.Name() {
	case "postgres", "mysql":
		return true
	}
	return false
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
// both back, so no live row is left carrying the sealed value. g (optional) is
// checked on the locked row like UpdateGuarded: stale answers ErrStaleWrite and
// the row is neither sealed nor deleted. ErrRecordNotFound means the row is
// gone. The caller needs update and delete permission.
//
// Events and hooks. Exactly ONE canonical event is published, "deleted", after
// the commit and only if it succeeded; its `before` is the row as sealed. The
// seal publishes no "updated" and runs no after-update hook: by then the row is
// already tombstoned, so a subscriber could not re-read it. The before-update
// hooks, validation and audit stamps of the seal do run (inside the
// transaction), and so do the after-delete hooks, file cleanup and pivot
// cleanup of Delete. The steps the seal inherits from Update after the commit
// (pivot sync, extension upsert, document lines) are best effort here: a
// failure is logged and never stops the deleted event or the cleanup.
//
// Order. The stale decision that counts is the one under the row lock. To not
// run before-delete hooks for a write that is already stale, a read before the
// hooks answers ErrStaleWrite early when the committed row is already stale;
// if another writer makes it stale between that read and the lock, the
// before-delete hooks have run and the write is still refused with
// ErrStaleWrite (before-delete hooks must tolerate a refused delete, as they
// already must when checkDeletable or the transaction fails).
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
	if g != nil {
		if err := checkGuardColumn(instance, g.Field); err != nil {
			return err
		}
	}
	if err := s.authorize(ctx, user, model, instance, modelbase.AccessDelete); err != nil {
		return err
	}
	if ctx, err = s.requireDeleteReason(ctx, model); err != nil {
		return err
	}
	if err := s.refuseIfReadOnly(ctx, model, "delete"); err != nil {
		return err
	}
	if err := s.refuseIfAppendOnly(ctx, model, "delete"); err != nil {
		return err
	}
	if err := s.refuseIfRowCondition(ctx, model, "delete", user, id); err != nil {
		return err
	}
	tableName, err := s.tableNameFor(ctx, model, instance)
	if err != nil {
		return err
	}
	if g != nil && s.alreadyStale(ctx, model, tableName, user, id, g) {
		return ErrStaleWrite
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
		then: func(ctx context.Context, tx *gorm.DB) error {
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

// alreadyStale is DeleteSealed's cheap read before the hooks: true only when
// the committed row is readable and g.Stale judges it stale. Any problem
// (row missing, unreadable, g.Stale failing) answers false and leaves the
// decision to the one under the row lock. The value reaches g.Stale the same
// way it does there (the row as JSON).
func (s *Service) alreadyStale(ctx context.Context, model, tableName string, user modelbase.AuthUser, id uuid.UUID, g *WriteGuard) bool {
	inst, _, err := s.resolveModel(ctx, model)
	if err != nil {
		return false
	}
	q := s.scope.ScopeQuery(s.tableDB(ctx, s.db.WithContext(ctx), tableName, inst), user)
	if err := q.First(inst, "id = ?", id).Error; err != nil {
		return false
	}
	stale, err := g.Stale(toMap(inst)[g.Field])
	return err == nil && stale
}
