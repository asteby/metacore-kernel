package dynamic

import (
	"context"
	"reflect"
	"strings"
	"sync"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/schema"
)

// nullable_text.go — an unset optional text column is NULL, never "".
//
// Reflect-built addon models carry text/varchar columns as plain Go `string`
// (model.go keeps them value-typed on purpose: "" is a legitimate value and the
// read shape of every addon depends on it). The price was that GORM wrote the
// zero value "" for every text column the caller did NOT send:
//
//   - Create: a column absent from the input was INSERTed as '' instead of
//     NULL / its DB default.
//   - Update: Save rewrites every column, so a NULL loaded as "" came back as
//     '' on the next edit of the row.
//
// '' is not "unset" for a UNIQUE index: a partial unique index written the
// idiomatic way (`WHERE idempotency_key IS NOT NULL`) lets exactly ONE row with
// '' per org exist and 500s every later write (QA 0922 VEN-N02: the second
// manual drawer cash-out of an org failed with duplicate key
// pos_cash_movements_idempotency_uq). The same trap applies to any nullable
// external_id / reference / code column with a uniqueness rule.
//
// Fix, once for every model: on Create and Update the write OMITS each string
// field whose key the caller did not send, when the physical column is
// NULLABLE. Postgres then applies the column DEFAULT (or NULL) on insert, and
// leaves the stored value untouched on update. A value the caller DID send —
// including an explicit "" — is written as-is, and a NOT NULL text column keeps
// the historical '' behaviour (omitting it would turn a working insert into a
// 23502). Nullability comes from the database itself (information_schema via
// the GORM migrator), cached per table, so a manifest that under-declares
// not_null can never make an insert fail that used to succeed.

// nullableColsTTL bounds how long a table's nullable-column set is trusted. An
// addon upgrade that adds a column or tightens NOT NULL is picked up within it;
// until then a new column simply keeps the legacy (value-written) behaviour.
const nullableColsTTL = 5 * time.Minute

type nullableColsEntry struct {
	cols map[string]bool // column name -> nullable
	at   time.Time
}

type nullableColsKey struct {
	cfg   *gorm.Config // one per opened database
	table string
}

var nullableColsCache sync.Map // nullableColsKey -> nullableColsEntry

// nullableColumns returns the table's physical column -> nullable map, or nil
// when it cannot be determined (the caller then omits nothing).
func nullableColumns(db *gorm.DB, table string) map[string]bool {
	key := nullableColsKey{cfg: db.Config, table: table}
	if e, ok := nullableColsCache.Load(key); ok {
		entry := e.(nullableColsEntry)
		if time.Since(entry.at) < nullableColsTTL {
			return entry.cols
		}
	}
	types, err := db.Migrator().ColumnTypes(table)
	if err != nil || len(types) == 0 {
		return nil
	}
	cols := make(map[string]bool, len(types))
	for _, ct := range types {
		nullable, ok := ct.Nullable()
		cols[ct.Name()] = ok && nullable
	}
	nullableColsCache.Store(key, nullableColsEntry{cols: cols, at: time.Now()})
	return cols
}

// unsentNullableStringFields lists the struct field names of `instance` that are
// string-typed, were NOT supplied in `input`, and map to a nullable column
// without a declared GORM default — the fields a write must omit so the
// database keeps NULL instead of receiving "". Primary keys and fields GORM
// already skips (default-tagged, read-only) are left alone.
func unsentNullableStringFields(ctx context.Context, db *gorm.DB, table string, instance any, input map[string]any) []string {
	stmt := &gorm.Statement{DB: db}
	if err := stmt.Parse(instance); err != nil || stmt.Schema == nil {
		return nil
	}
	var nullable map[string]bool
	var omit []string
	for _, f := range stmt.Schema.Fields {
		if f.DBName == "" || f.PrimaryKey || f.HasDefaultValue || f.NotNull || !f.Creatable {
			continue
		}
		if f.FieldType.Kind() != reflect.String {
			continue
		}
		if inputHasField(input, f) {
			continue
		}
		if nullable == nil {
			if nullable = nullableColumns(db.Session(&gorm.Session{NewDB: true, Context: ctx}), table); nullable == nil {
				return nil
			}
		}
		if nullable[f.DBName] {
			omit = append(omit, f.DBName)
		}
	}
	return omit
}

// inputHasField reports whether the caller supplied the field, under either its
// JSON key (the wire name) or its column name.
func inputHasField(input map[string]any, f *schema.Field) bool {
	if _, ok := input[f.DBName]; ok {
		return true
	}
	if name, _, _ := strings.Cut(f.Tag.Get("json"), ","); name != "" && name != "-" {
		if _, ok := input[name]; ok {
			return true
		}
	}
	return false
}

// omitUnsentNullableText applies unsentNullableStringFields to a write handle.
func omitUnsentNullableText(ctx context.Context, db *gorm.DB, table string, instance any, input map[string]any) *gorm.DB {
	if omit := unsentNullableStringFields(ctx, db, table, instance, input); len(omit) > 0 {
		return db.Omit(omit...)
	}
	return db
}
