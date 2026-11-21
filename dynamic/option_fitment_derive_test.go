package dynamic

import (
	"testing"

	"github.com/asteby/metacore-kernel/manifest"
	v3 "github.com/asteby/metacore-kernel/manifest/v3"
)

// option_fitment rides from the manifest column onto the served table column and
// form field, and a column without it serves nothing (additive).
func TestDerive_OptionFitmentProjected(t *testing.T) {
	def := manifest.ModelDefinition{
		TableName: "quote_items",
		Columns: []manifest.ColumnDef{
			{Name: "product_id", Type: "uuid", Ref: "products", OptionFitment: &v3.OptionFitment{
				SubjectField: "vehicle_id", SubjectModel: "Vehicle", TargetModel: "TireSpec", Mode: "filter"}},
			{Name: "note", Type: "text"},
		},
	}
	for _, c := range DeriveTableColumns(def) {
		switch c.Key {
		case "product_id":
			if c.OptionFitment == nil || c.OptionFitment.SubjectField != "vehicle_id" || c.OptionFitment.TargetModel != "TireSpec" || c.OptionFitment.Mode != "filter" {
				t.Errorf("table column lost option_fitment: %+v", c.OptionFitment)
			}
		case "note":
			if c.OptionFitment != nil {
				t.Errorf("plain column must not carry option_fitment")
			}
		}
	}
	for _, f := range DeriveFormFields(def) {
		switch f.Key {
		case "product_id":
			if f.OptionFitment == nil || f.OptionFitment.SubjectModel != "Vehicle" {
				t.Errorf("form field lost option_fitment: %+v", f.OptionFitment)
			}
		case "note":
			if f.OptionFitment != nil {
				t.Errorf("plain field must not carry option_fitment")
			}
		}
	}
}
