package dynamic

import (
	"testing"

	"github.com/asteby/metacore-kernel/manifest"
	"gorm.io/gorm/schema"
	"reflect"
	"sync"
)

// Columns whose snake_case segments start with a digit (aging_1_30) are lossy
// through exportName (Aging130). The struct tag must pin the manifest column
// name or GORM's NamingStrategy writes INSERT INTO ... ("aging130") and Postgres
// answers SQLSTATE 42703.
func TestBuildStructType_PinsColumnNameForDigitSegments(t *testing.T) {
	def := manifest.ModelDefinition{
		TableName: "customers",
		Columns: []manifest.ColumnDef{
			{Name: "aging_1_30", Type: "decimal"},
			{Name: "aging_31_60", Type: "decimal", Default: "0"},
			{Name: "aging_90_plus", Type: "decimal"},
			{Name: "name", Type: "string", Required: true},
		},
	}
	typ, err := BuildStructType(def)
	if err != nil {
		t.Fatal(err)
	}
	s, err := schema.Parse(reflect.New(typ).Interface(), &sync.Map{}, schema.NamingStrategy{})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"aging_1_30", "aging_31_60", "aging_90_plus", "name"} {
		if s.LookUpField(want) == nil || s.LookUpField(want).DBName != want {
			got := []string{}
			for _, f := range s.Fields {
				got = append(got, f.DBName)
			}
			t.Fatalf("column %q not mapped; fields DBNames=%v", want, got)
		}
	}
}
