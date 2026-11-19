package v3

import (
	"strings"
	"testing"
)

func seqModel(seq map[string]interface{}) map[string]interface{} {
	m := baseValid()
	m["models"] = []interface{}{
		map[string]interface{}{
			"key": "Invoice", "table": "invoices",
			"stage_field": "status",
			"stages": []interface{}{
				map[string]interface{}{"key": "draft"},
				map[string]interface{}{"key": "issued"},
			},
			"transitions": []interface{}{map[string]interface{}{"from": "draft", "to": "issued"}},
			"sequences":   []interface{}{seq},
			"columns": []interface{}{
				map[string]interface{}{"name": "id", "type": "uuid"},
				map[string]interface{}{"name": "organization_id", "type": "uuid", "not_null": true},
				map[string]interface{}{"name": "status", "type": "text"},
				map[string]interface{}{"name": "issuer_id", "type": "uuid"},
				map[string]interface{}{"name": "folio", "type": "text", "sequence": "folio"},
			},
		},
	}
	return m
}

func TestSequenceV2_Valid(t *testing.T) {
	m := seqModel(map[string]interface{}{
		"key": "folio", "scope": "column", "scope_column": "issuer_id", "period": "year",
		"gapless": true, "assign_on": "transition:issued", "format": "{scope}-{yyyy}-{seq:06}",
	})
	if err := Validate(mustJSON(t, m)); err != nil {
		t.Fatalf("sequences v2 should validate: %v", err)
	}
}

func TestSequenceV2_Invalid(t *testing.T) {
	cases := map[string]struct {
		seq  map[string]interface{}
		want string
	}{
		"column needs scope_column":  {map[string]interface{}{"key": "folio", "scope": "column", "format": "{seq}"}, "scope_column is required"},
		"scope_column wrong column":  {map[string]interface{}{"key": "folio", "scope": "column", "scope_column": "ghost", "format": "{seq}"}, "not a declared column"},
		"scope_column without scope": {map[string]interface{}{"key": "folio", "scope_column": "issuer_id", "format": "{seq}"}, "only valid with scope"},
		"date token without period":  {map[string]interface{}{"key": "folio", "format": "{yyyy}-{seq}"}, "needs period"},
		"dd with period month":       {map[string]interface{}{"key": "folio", "period": "month", "format": "{yyyy}{mm}{dd}-{seq}"}, "needs period day"},
		"scope token without scope":  {map[string]interface{}{"key": "folio", "format": "{scope}-{seq}"}, "needs scope"},
		"assign_on unknown stage":    {map[string]interface{}{"key": "folio", "assign_on": "transition:ghost", "format": "{seq}"}, "does not declare"},
	}
	for name, c := range cases {
		err := Validate(mustJSON(t, seqModel(c.seq)))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: want error containing %q, got %v", name, c.want, err)
		}
	}
}
