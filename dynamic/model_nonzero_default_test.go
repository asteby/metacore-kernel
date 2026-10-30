package dynamic

import (
	"reflect"
	"testing"

	"github.com/asteby/metacore-kernel/manifest"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// A bool/numeric column whose declared Default is NOT the Go zero value must be
// built as a POINTER: as a value GORM omits the zero (false / 0) from the INSERT
// and the column DEFAULT silently wins (requires_review=false stored as true).
func TestColumnToField_NonZeroDefaultIsPointer(t *testing.T) {
	cases := []struct {
		name     string
		col      manifest.ColumnDef
		wantElem reflect.Type // nil → must stay a value type
	}{
		{"bool default true", manifest.ColumnDef{Name: "v", Type: "boolean", Default: true}, boolType},
		{"bool default true required", manifest.ColumnDef{Name: "v", Type: "boolean", Default: true, Required: true}, boolType},
		{"bool default false stays value", manifest.ColumnDef{Name: "v", Type: "boolean", Default: false}, nil},
		{"numeric default 5", manifest.ColumnDef{Name: "v", Type: "numeric", Default: 5}, float64Type},
		{"numeric default 0 stays value", manifest.ColumnDef{Name: "v", Type: "numeric", Default: 0}, nil},
		{"bool required no default stays value", manifest.ColumnDef{Name: "v", Type: "boolean", Required: true}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, err := columnToField(tc.col)
			if err != nil {
				t.Fatal(err)
			}
			if tc.wantElem == nil {
				if f.Type.Kind() == reflect.Ptr {
					t.Fatalf("got %v, want a value type", f.Type)
				}
				return
			}
			if f.Type.Kind() != reflect.Ptr || f.Type.Elem() != tc.wantElem {
				t.Fatalf("got %v, want *%v", f.Type, tc.wantElem)
			}
		})
	}
}

// End to end against the real GORM INSERT path: an unset field takes the column
// DEFAULT, an explicit false / 0 is written verbatim.
func TestCreate_ExplicitZeroOverridesNonZeroDefault(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`CREATE TABLE seg (id integer primary key autoincrement, name text,
		requires_review boolean not null default true, weight numeric not null default 5)`).Error; err != nil {
		t.Fatal(err)
	}
	cols := []manifest.ColumnDef{
		{Name: "name", Type: "string"},
		{Name: "requires_review", Type: "boolean", Default: true, Required: true},
		{Name: "weight", Type: "numeric", Default: 5, Required: true},
	}
	var fields []reflect.StructField
	for _, c := range cols {
		f, err := columnToField(c)
		if err != nil {
			t.Fatal(err)
		}
		fields = append(fields, f)
	}
	typ := reflect.StructOf(fields)

	insert := func(name string, review *bool, weight *float64) {
		v := reflect.New(typ)
		v.Elem().FieldByName(exportName("name")).SetString(name)
		if review != nil {
			v.Elem().FieldByName(exportName("requires_review")).Set(reflect.ValueOf(review))
		}
		if weight != nil {
			v.Elem().FieldByName(exportName("weight")).Set(reflect.ValueOf(weight))
		}
		if err := db.Table("seg").Create(v.Interface()).Error; err != nil {
			t.Fatalf("insert %s: %v", name, err)
		}
	}
	no, zero := false, 0.0
	insert("unset", nil, nil)
	insert("explicit", &no, &zero)

	type row struct {
		Name           string
		RequiresReview bool
		Weight         float64
	}
	get := func(name string) row {
		var r row
		if err := db.Table("seg").Where("name = ?", name).Take(&r).Error; err != nil {
			t.Fatal(err)
		}
		return r
	}
	if r := get("unset"); !r.RequiresReview || r.Weight != 5 {
		t.Fatalf("unset must take the column DEFAULT, got %+v", r)
	}
	if r := get("explicit"); r.RequiresReview || r.Weight != 0 {
		t.Fatalf("explicit false/0 must be written, got %+v", r)
	}
}
