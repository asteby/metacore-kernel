package modelbase

import (
	"reflect"
	"strings"
)

// DeriveAuditMeta builds the AuditMeta of a model instance (compiled GORM model
// or a reflect-built dynamic struct) from the audit columns it actually has: a
// field counts when its JSON name (or, for fields tagged json:"-" such as
// BaseUUIDModel.DeletedAt, its Go name) is one of the standard audit columns.
// Returns nil when the model has none, so the served metadata stays unchanged
// for models without audit columns. Embedded structs are walked.
func DeriveAuditMeta(model any) *AuditMeta {
	t := reflect.TypeOf(model)
	for t != nil && t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	if t == nil || t.Kind() != reflect.Struct {
		return nil
	}
	cols := map[string]bool{}
	var walk func(reflect.Type)
	walk = func(t reflect.Type) {
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			if f.Anonymous {
				ft := f.Type
				for ft.Kind() == reflect.Ptr {
					ft = ft.Elem()
				}
				if ft.Kind() == reflect.Struct {
					walk(ft)
					continue
				}
			}
			name := strings.Split(f.Tag.Get("json"), ",")[0]
			if name == "" || name == "-" {
				switch f.Name {
				case "CreatedAt":
					name = "created_at"
				case "UpdatedAt":
					name = "updated_at"
				case "DeletedAt":
					name = "deleted_at"
				case "CreatedByID":
					name = "created_by_id"
				case "UpdatedByID":
					name = "updated_by_id"
				case "DeletedByID":
					name = "deleted_by_id"
				}
			}
			cols[name] = true
		}
	}
	walk(t)
	m := &AuditMeta{}
	pick := func(col string) string {
		if cols[col] {
			return col
		}
		return ""
	}
	m.CreatedAt, m.CreatedBy = pick("created_at"), pick("created_by_id")
	m.UpdatedAt, m.UpdatedBy = pick("updated_at"), pick("updated_by_id")
	m.DeletedAt, m.DeletedBy = pick("deleted_at"), pick("deleted_by_id")
	if *m == (AuditMeta{}) {
		return nil
	}
	return m
}
