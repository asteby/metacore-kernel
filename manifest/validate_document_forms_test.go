package manifest

import (
	"strings"
	"testing"
)

func TestValidateDocumentFormsLegacy(t *testing.T) {
	cols := map[string]struct{}{"type": {}}
	ok := &DocumentFormsDef{TypeField: "type", Types: []DocumentFormTypeDef{
		{Key: "invoice", Label: "Factura", Fields: []FieldDef{{Key: "a"}, {Name: "b"}}},
	}}
	if err := validateDocumentForms(ok, cols); err != nil {
		t.Fatalf("valid rejected: %v", err)
	}
	if err := validateDocumentForms(nil, cols); err != nil {
		t.Fatalf("nil must be a no-op: %v", err)
	}
	bad := map[string]*DocumentFormsDef{
		"type_field":    {TypeField: "nope", Types: ok.Types},
		"empty":         {Types: nil},
		"invalid":       {Types: []DocumentFormTypeDef{{Key: "Bad", Label: "x"}}},
		"duplicated":    {Types: []DocumentFormTypeDef{{Key: "a", Label: "x"}, {Key: "a", Label: "y"}}},
		"label":         {Types: []DocumentFormTypeDef{{Key: "a"}}},
		"fields[1].key": {Types: []DocumentFormTypeDef{{Key: "a", Label: "x", Fields: []FieldDef{{Key: "k"}, {}}}}},
	}
	for want, df := range bad {
		err := validateDocumentForms(df, cols)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v", want, err)
		}
	}
}
