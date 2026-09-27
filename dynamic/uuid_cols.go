package dynamic

import (
	"reflect"
	"strings"

	"github.com/google/uuid"
)

// uuidColumnsOf returns the json names of fields whose storage type is
// uuid.UUID (or *uuid.UUID). List uses this so an `in:` on product_id binds
// as uuid[] instead of text[].
func uuidColumnsOf(instance any) map[string]struct{} {
	out := map[string]struct{}{}
	if instance == nil {
		return out
	}
	t := reflect.TypeOf(instance)
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		return out
	}
	uuidT := reflect.TypeOf(uuid.UUID{})
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if f.PkgPath != "" {
			continue
		}
		ft := f.Type
		if ft.Kind() == reflect.Pointer {
			ft = ft.Elem()
		}
		if ft != uuidT {
			continue
		}
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if name == "" || name == "-" {
			continue
		}
		out[name] = struct{}{}
	}
	return out
}
