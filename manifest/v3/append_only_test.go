package v3

import (
	"strings"
	"testing"
)

func ledgerModel(extra map[string]interface{}) map[string]interface{} {
	mod := map[string]interface{}{
		"key":   "Entry",
		"table": "entries",
		"columns": []interface{}{
			map[string]interface{}{"name": "id", "type": "uuid"},
			map[string]interface{}{"name": "organization_id", "type": "uuid", "not_null": true},
			map[string]interface{}{"name": "amount", "type": "numeric(12,2)"},
			map[string]interface{}{"name": "state", "type": "text"},
		},
	}
	for k, v := range extra {
		mod[k] = v
	}
	return mod
}

func withLedger(extra map[string]interface{}) map[string]interface{} {
	m := baseValid()
	m["models"] = []interface{}{ledgerModel(extra)}
	return m
}

func TestAppendOnly_ValidAndParsed(t *testing.T) {
	m := withLedger(map[string]interface{}{"append_only": true})
	if err := Validate(mustJSON(t, m)); err != nil {
		t.Fatalf("append_only ledger should validate: %v", err)
	}
	parsed, err := Parse(mustJSON(t, m))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !parsed.Models[0].AppendOnly {
		t.Fatal("Model.AppendOnly not parsed")
	}
}

func TestAppendOnly_RejectsStageMachine(t *testing.T) {
	m := withLedger(map[string]interface{}{
		"append_only": true,
		"stage_field": "state",
		"stages":      []interface{}{map[string]interface{}{"key": "open", "label": "Open"}},
	})
	err := Validate(mustJSON(t, m))
	if err == nil || !strings.Contains(err.Error(), "append_only cannot be combined with a stage machine") {
		t.Fatalf("want stage-machine conflict, got %v", err)
	}
}

func TestAppendOnly_RejectsNonBoolean(t *testing.T) {
	m := withLedger(map[string]interface{}{"append_only": "yes"})
	if err := Validate(mustJSON(t, m)); err == nil {
		t.Fatal("a non-boolean append_only must fail the schema")
	}
}
