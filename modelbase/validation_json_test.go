package modelbase

import (
	"encoding/json"
	"testing"
)

func TestValidationRule_UnmarshalJSON_ObjectAndString(t *testing.T) {
	var obj ValidationRule
	if err := json.Unmarshal([]byte(`{"min":2,"max":10,"regex":"^[A-Z]+$","custom":"email"}`), &obj); err != nil {
		t.Fatal(err)
	}
	if obj.Min == nil || *obj.Min != 2 || obj.Custom != "email" || obj.Regex != `^[A-Z]+$` {
		t.Fatalf("object: %+v", obj)
	}

	var fromLaravel ValidationRule
	if err := json.Unmarshal([]byte(`"required,min=2,max=100"`), &fromLaravel); err != nil {
		t.Fatal(err)
	}
	if fromLaravel.Min == nil || *fromLaravel.Min != 2 || fromLaravel.Max == nil || *fromLaravel.Max != 100 {
		t.Fatalf("laravel string: %+v", fromLaravel)
	}

	var slug ValidationRule
	if err := json.Unmarshal([]byte(`"$org.tax_id_validator"`), &slug); err != nil {
		t.Fatal(err)
	}
	if slug.Custom != "$org.tax_id_validator" {
		t.Fatalf("slug: %+v", slug)
	}
}

func TestFieldDef_ValidationJSON(t *testing.T) {
	var fd FieldDef
	if err := json.Unmarshal([]byte(`{"key":"sku","label":"SKU","type":"text","validation":{"min":3}}`), &fd); err != nil {
		t.Fatal(err)
	}
	if fd.Validation == nil || fd.Validation.Min == nil || *fd.Validation.Min != 3 {
		t.Fatalf("got %+v", fd.Validation)
	}
}
