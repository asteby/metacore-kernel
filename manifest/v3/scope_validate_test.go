package v3

import (
	"strings"
	"testing"
)

func scopeModel(scope string, extraCols ...map[string]interface{}) map[string]interface{} {
	cols := []interface{}{
		map[string]interface{}{"name": "id", "type": "uuid"},
		map[string]interface{}{"name": "organization_id", "type": "uuid", "not_null": true},
	}
	for _, c := range extraCols {
		cols = append(cols, c)
	}
	m := map[string]interface{}{"key": "Product", "table": "products", "columns": cols}
	if scope != "" {
		m["scope"] = scope
	}
	return m
}

func TestModelScope_Validation(t *testing.T) {
	markets := map[string]interface{}{"name": "markets", "type": "jsonb"}
	valid := []struct {
		name  string
		model map[string]interface{}
	}{
		{"sin declarar (compatibilidad)", scopeModel("")},
		{"merchant", scopeModel("merchant")},
		{"shared", scopeModel("shared")},
		{"group", scopeModel("group")},
		{"market con su columna markets jsonb", scopeModel("market", markets)},
	}
	for _, c := range valid {
		if err := Validate(mustJSON(t, aliasManifest(c.model))); err != nil {
			t.Errorf("%s: %v", c.name, err)
		}
	}

	cases := map[string]struct {
		model map[string]interface{}
		want  string
	}{
		"valor desconocido":                  {scopeModel("region"), "must be one of"},
		"market sin columna markets":         {scopeModel("market"), `needs a "markets" column`},
		"market con markets que no es jsonb": {scopeModel("market", map[string]interface{}{"name": "markets", "type": "text"}), `must be jsonb`},
	}
	for name, c := range cases {
		err := Validate(mustJSON(t, aliasManifest(c.model)))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want substring %q", name, err, c.want)
		}
	}
}

func TestScopeKind_Valid(t *testing.T) {
	for _, k := range ScopeKinds {
		if !k.Valid() {
			t.Errorf("%q debería ser válido", k)
		}
	}
	if ScopeKind("").Valid() || ScopeKind("Merchant").Valid() {
		t.Error("el vacío y las mayúsculas no son valores declarables")
	}
}
