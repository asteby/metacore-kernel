package metakit

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/schema"

	"github.com/asteby/metacore-kernel/dynamic"
	"github.com/asteby/metacore-kernel/modelbase"
)

// Partial-update guard.
//
// Compiled models are often edited through paths that write a MAP, not a
// struct: the generic PUT /api/data/:model/:id (Updates(input)), an agent tool
// (Table(t).Updates(map), no model, no hooks) and row actions. With a map,
// BeforeSave receives an empty model and cannot validate the row. The guard
// closes that gap with a global GORM callback:
//
//  1. load the rows the statement will touch (same WHERE, same transaction);
//  2. apply the map onto a copy of each row;
//  3. outside a domain action (Trusted), only the form columns (DefineModal)
//     may change — status, revocation, evidence or closing columns change only
//     through their action;
//  4. run the model rule with the row before and after the change.
//
// Map inserts without a model (agent tools) run the same rule with before == nil.

const trustedKey = "metakit:trusted"

// Callback names registered by Install.
const (
	CallbackUpdate = "metakit:guard_update"
	CallbackCreate = "metakit:guard_create"
)

// Trusted marks the session of a domain action that already checked its
// transition. Its Updates(map) may touch columns outside the form; the model
// rule still runs on the resulting row.
func Trusted(db *gorm.DB) *gorm.DB { return db.Set(trustedKey, true) }

// IsTrusted reports whether tx was marked with Trusted.
func IsTrusted(tx *gorm.DB) bool {
	if tx == nil {
		return false
	}
	v, ok := tx.Get(trustedKey)
	b, _ := v.(bool)
	return ok && b
}

// AlwaysWritable are system columns any path may touch (soft delete, update
// stamp, audit actor). Hosts may add their own before Install; do not mutate
// it concurrently with writes.
var AlwaysWritable = map[string]bool{
	"updated_at":    true,
	"deleted_at":    true,
	"updated_by_id": true,
	"deleted_by_id": true,
}

type guardEntry struct {
	newModel func() any
	editable map[string]bool
	check    func(before, after any) error
}

var (
	guardMu sync.RWMutex
	guards  = map[string]guardEntry{}
	schemas sync.Map
)

type modalDefiner interface {
	DefineModal() modelbase.ModalMetadata
}

// GuardOption tunes a Guard registration.
type GuardOption func(editable map[string]bool)

// Editable adds columns that a non-trusted map write may change, on top of the
// fields of DefineModal. Use it for models without a modal or for columns an
// inline table edit owns.
func Editable(cols ...string) GuardOption {
	return func(editable map[string]bool) {
		for _, c := range cols {
			editable[c] = true
		}
	}
}

// Guard registers the rule of a table. The editable set is the model's
// DefineModal fields (if T implements it) plus any Editable option. check
// receives the row before the change (nil on insert) and the resulting row; a
// non-nil error aborts the write as a 422 validation error. check may be nil
// to enforce only the editable columns. Registering the same table again
// replaces the previous rule.
func Guard[T any](table string, check func(before, after *T) error, opts ...GuardOption) {
	editable := map[string]bool{}
	if m, ok := any(new(T)).(modalDefiner); ok {
		for _, f := range m.DefineModal().Fields {
			editable[f.Key] = true
		}
	}
	for _, o := range opts {
		if o != nil {
			o(editable)
		}
	}
	guardMu.Lock()
	defer guardMu.Unlock()
	guards[table] = guardEntry{
		newModel: func() any { return new(T) },
		editable: editable,
		check: func(before, after any) error {
			if check == nil {
				return nil
			}
			var b *T
			if before != nil {
				b = before.(*T)
			}
			return check(b, after.(*T))
		},
	}
}

func guardFor(table string) (guardEntry, bool) {
	guardMu.RLock()
	defer guardMu.RUnlock()
	g, ok := guards[strings.Trim(table, `"`)]
	return g, ok
}

// Install registers the callbacks on the connection. It is idempotent.
func Install(db *gorm.DB) error {
	if db.Callback().Update().Get(CallbackUpdate) == nil {
		if err := db.Callback().Update().Before("gorm:update").Register(CallbackUpdate, guardUpdate); err != nil {
			return err
		}
	}
	if db.Callback().Create().Get(CallbackCreate) == nil {
		if err := db.Callback().Create().Before("gorm:create").Register(CallbackCreate, guardCreate); err != nil {
			return err
		}
	}
	return nil
}

// MapUpdate reports whether the hook runs for a partial Updates(map): the
// receiver is an empty model and there is no full row to validate in the
// hook. That validation is not skipped — the Install callback loads the row,
// applies the map and runs the rule registered with Guard.
func MapUpdate(tx *gorm.DB) bool {
	if tx == nil || tx.Statement == nil {
		return false
	}
	_, ok := tx.Statement.Dest.(map[string]interface{})
	return ok
}

// Validation turns a domain validation error into the kernel's typed
// *dynamic.ValidationError, so the dynamic CRUD answers 422 with the message
// instead of a generic 500. A *dynamic.ValidationError passes through
// unchanged. Returns nil for nil.
func Validation(err error) error {
	if err == nil {
		return nil
	}
	var ve *dynamic.ValidationError
	if errors.As(err, &ve) {
		return err
	}
	return dynamic.NewValidationError().AddMessage("_", err.Error()).Err()
}

func modelSchema(tx *gorm.DB, model any) (*schema.Schema, error) {
	return schema.Parse(model, &schemas, tx.NamingStrategy)
}

func guardUpdate(tx *gorm.DB) {
	if tx.Error != nil || tx.Statement == nil {
		return
	}
	set, ok := tx.Statement.Dest.(map[string]interface{})
	if !ok {
		return // struct: the model's BeforeSave validates it
	}
	g, ok := guardFor(tx.Statement.Table)
	if !ok {
		return
	}
	sch, err := modelSchema(tx, g.newModel())
	if err != nil {
		_ = tx.AddError(err)
		return
	}
	rows, err := loadTargets(tx, g, sch)
	if err != nil {
		_ = tx.AddError(err)
		return
	}
	trusted := IsTrusted(tx)
	for _, before := range rows {
		after := g.newModel()
		reflect.ValueOf(after).Elem().Set(reflect.ValueOf(before).Elem())
		if err := apply(tx.Statement.Context, sch, after, set); err != nil {
			_ = tx.AddError(err)
			return
		}
		if !trusted {
			if err := onlyEditable(sch, g.editable, before, after); err != nil {
				_ = tx.AddError(err)
				return
			}
		}
		if err := g.check(before, after); err != nil {
			_ = tx.AddError(Validation(err))
			return
		}
	}
}

func guardCreate(tx *gorm.DB) {
	if tx.Error != nil || tx.Statement == nil || tx.Statement.Schema != nil {
		return // with a model, the struct's BeforeSave validates the insert
	}
	g, ok := guardFor(tx.Statement.Table)
	if !ok {
		return
	}
	var maps []map[string]interface{}
	switch v := tx.Statement.Dest.(type) {
	case map[string]interface{}:
		maps = []map[string]interface{}{v}
	case []map[string]interface{}:
		maps = v
	default:
		return
	}
	sch, err := modelSchema(tx, g.newModel())
	if err != nil {
		_ = tx.AddError(err)
		return
	}
	for _, m := range maps {
		after := g.newModel()
		if err := apply(tx.Statement.Context, sch, after, m); err != nil {
			_ = tx.AddError(err)
			return
		}
		if err := g.check(nil, after); err != nil {
			_ = tx.AddError(Validation(err))
			return
		}
	}
}

// loadTargets reads, on the same connection, the rows the statement's WHERE
// reaches (plus the model's primary key, when the statement carries one).
func loadTargets(tx *gorm.DB, g guardEntry, sch *schema.Schema) ([]any, error) {
	q := tx.Session(&gorm.Session{NewDB: true}).Model(g.newModel())
	where, hasWhere := tx.Statement.Clauses["WHERE"]
	if hasWhere {
		if w, ok := where.Expression.(clause.Where); ok && len(w.Exprs) > 0 {
			q = q.Clauses(clause.Where{Exprs: w.Exprs})
		} else {
			hasWhere = false
		}
	}
	if pk := sch.PrioritizedPrimaryField; pk != nil && tx.Statement.Model != nil {
		rv := reflect.ValueOf(tx.Statement.Model)
		if rv.Kind() == reflect.Ptr && !rv.IsNil() && rv.Elem().Kind() == reflect.Struct && rv.Elem().Type() == sch.ModelType {
			if v, zero := pk.ValueOf(tx.Statement.Context, rv.Elem()); !zero {
				q = q.Where(clause.Eq{Column: clause.Column{Name: pk.DBName}, Value: v})
				hasWhere = true
			}
		}
	}
	if !hasWhere {
		return nil, nil // GORM refuses an UPDATE without WHERE
	}
	slice := reflect.New(reflect.SliceOf(reflect.TypeOf(g.newModel())))
	if err := q.Find(slice.Interface()).Error; err != nil {
		return nil, err
	}
	out := make([]any, slice.Elem().Len())
	for i := range out {
		out[i] = slice.Elem().Index(i).Interface()
	}
	return out, nil
}

// apply copies the map onto the struct with GORM's own setters. SQL
// expressions (gorm.Expr) have no Go value and are skipped.
func apply(ctx context.Context, sch *schema.Schema, dst any, set map[string]interface{}) error {
	rv := reflect.ValueOf(dst)
	for k, v := range set {
		f := sch.LookUpField(k)
		if f == nil || f.DBName == "" {
			continue
		}
		switch v.(type) {
		case clause.Expr, clause.Expression:
			continue
		}
		if err := f.Set(ctx, rv, v); err != nil {
			return dynamic.NewValidationError().Add(f.DBName, "invalid_type", nil).Err()
		}
	}
	return nil
}

// onlyEditable refuses changes to columns outside the editable set. The
// failure uses the kernel code protected_field, which the SDK localizes.
func onlyEditable(sch *schema.Schema, editable map[string]bool, before, after any) error {
	bv, av := reflect.ValueOf(before), reflect.ValueOf(after)
	ve := dynamic.NewValidationError()
	for _, f := range sch.Fields {
		if f.DBName == "" || editable[f.DBName] || AlwaysWritable[f.DBName] {
			continue
		}
		b := f.ReflectValueOf(context.Background(), bv).Interface()
		a := f.ReflectValueOf(context.Background(), av).Interface()
		if !sameValue(b, a) {
			ve.Add(f.DBName, "protected_field", nil)
		}
	}
	return ve.Err()
}

// sameValue compares two column values; instants compare at second precision
// because a form posts them back as RFC 3339 without fraction.
func sameValue(a, b any) bool {
	ta, aok := asTime(a)
	tb, bok := asTime(b)
	if aok || bok {
		if ta == nil || tb == nil {
			return ta == nil && tb == nil
		}
		return ta.Truncate(time.Second).Equal(tb.Truncate(time.Second))
	}
	if reflect.DeepEqual(a, b) {
		return true
	}
	ja, errA := json.Marshal(a)
	jb, errB := json.Marshal(b)
	return errA == nil && errB == nil && string(ja) == string(jb)
}

// asTime recognizes time.Time, *time.Time and types embedding time.Time
// (dates.Time, dates.Date). It returns (nil, true) for a nil time pointer or a
// zero time.
func asTime(v any) (*time.Time, bool) {
	if v == nil {
		return nil, false
	}
	rv := reflect.ValueOf(v)
	for rv.Kind() == reflect.Ptr {
		if rv.IsNil() {
			if isTimeType(rv.Type().Elem()) {
				return nil, true
			}
			return nil, false
		}
		rv = rv.Elem()
	}
	if t, ok := rv.Interface().(time.Time); ok {
		if t.IsZero() {
			return nil, true
		}
		return &t, true
	}
	if rv.Kind() == reflect.Struct {
		if f := rv.FieldByName("Time"); f.IsValid() && f.CanInterface() {
			if t, ok := f.Interface().(time.Time); ok {
				if t.IsZero() {
					return nil, true
				}
				return &t, true
			}
		}
	}
	return nil, false
}

func isTimeType(t reflect.Type) bool {
	if t == reflect.TypeOf(time.Time{}) {
		return true
	}
	if t.Kind() == reflect.Struct {
		if f, ok := t.FieldByName("Time"); ok && f.Type == reflect.TypeOf(time.Time{}) {
			return true
		}
	}
	return false
}
