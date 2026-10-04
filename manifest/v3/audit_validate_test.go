package v3

import (
	"strings"
	"testing"
)

func withAuditModel(extraCols []interface{}, audit interface{}) map[string]interface{} {
	m := baseValid()
	cols := []interface{}{
		map[string]interface{}{"name": "id", "type": "uuid"},
		map[string]interface{}{"name": "organization_id", "type": "uuid", "not_null": true},
	}
	cols = append(cols, extraCols...)
	model := map[string]interface{}{"key": "Item", "table": "items", "columns": cols}
	if audit != nil {
		model["audit"] = audit
	}
	m["models"] = []interface{}{model}
	return m
}

func TestAudit_FieldAndStandardTypesAccepted(t *testing.T) {
	if err := Validate(mustJSON(t, withAuditModel(nil, false))); err != nil {
		t.Fatalf("audit:false must be accepted: %v", err)
	}
	ok := []interface{}{
		map[string]interface{}{"name": "deleted_at", "type": "timestamptz"},
		map[string]interface{}{"name": "created_by_id", "type": "uuid"},
	}
	if err := Validate(mustJSON(t, withAuditModel(ok, true))); err != nil {
		t.Fatalf("a manifest declaring audit columns with the standard type must pass: %v", err)
	}
}

func TestAudit_ColumnTypeClash_CompatWindow(t *testing.T) {
	bad := []interface{}{map[string]interface{}{"name": "deleted_at", "type": "text"}}
	raw := mustJSON(t, withAuditModel(bad, nil))

	// Install / upgrade of an already-published bundle: a tagged warning.
	warns, err := ValidateWithOptions(raw, Options{})
	if err != nil {
		t.Fatalf("non-strict must tolerate: %v", err)
	}
	if len(warns) != 1 || !strings.Contains(warns[0], "["+RuleAuditColumnType+"]") || !strings.Contains(warns[0], "deleted_at") {
		t.Fatalf("want one tagged warning, got %v", warns)
	}
	// Publish: a clear hard error.
	if _, err := ValidateWithOptions(raw, Options{Strict: true}); err == nil || !strings.Contains(err.Error(), "must be timestamp or timestamptz") {
		t.Fatalf("strict must refuse with a clear message, got %v", err)
	}

	badBy := []interface{}{map[string]interface{}{"name": "updated_by_id", "type": "text"}}
	if _, err := ValidateWithOptions(mustJSON(t, withAuditModel(badBy, nil)), Options{Strict: true}); err == nil || !strings.Contains(err.Error(), "must be uuid") {
		t.Fatalf("*_by_id must be uuid, got %v", err)
	}
}
