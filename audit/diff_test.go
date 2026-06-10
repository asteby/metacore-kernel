package audit

import (
	"encoding/json"
	"testing"
)

func TestComputeChanges_NilOnCreateOrDelete(t *testing.T) {
	if computeChanges(nil, map[string]any{"a": 1}) != nil {
		t.Errorf("create (before=nil) must yield nil changes")
	}
	if computeChanges(map[string]any{"a": 1}, nil) != nil {
		t.Errorf("delete (after=nil) must yield nil changes")
	}
}

func TestComputeChanges_OnlyChangedBusinessFields(t *testing.T) {
	before := map[string]any{"name": "A", "qty": 5, "updated_at": "t0"}
	after := map[string]any{"name": "B", "qty": 5, "updated_at": "t1"}

	out := computeChanges(before, after)
	if out == nil {
		t.Fatalf("expected non-nil changes")
	}
	var changes map[string]map[string]any
	if err := json.Unmarshal([]byte(*out), &changes); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(changes) != 1 {
		t.Fatalf("expected exactly 1 changed field, got %d: %v", len(changes), changes)
	}
	d, ok := changes["name"]
	if !ok {
		t.Fatalf("missing 'name': %v", changes)
	}
	if d["before"] != "A" || d["after"] != "B" {
		t.Errorf("name diff = %v, want before=A after=B", d)
	}
}

func TestComputeChanges_NewFieldHasNilBefore(t *testing.T) {
	out := computeChanges(map[string]any{}, map[string]any{"extra": "x"})
	if out == nil {
		t.Fatalf("expected non-nil changes for new field")
	}
	var changes map[string]map[string]any
	_ = json.Unmarshal([]byte(*out), &changes)
	if d := changes["extra"]; d["before"] != nil || d["after"] != "x" {
		t.Errorf("new field diff = %v, want before=nil after=x", d)
	}
}

func TestComputeChanges_TimestampOnlyYieldsNil(t *testing.T) {
	before := map[string]any{"name": "A", "updated_at": "t0"}
	after := map[string]any{"name": "A", "updated_at": "t1"}
	if computeChanges(before, after) != nil {
		t.Errorf("timestamp-only change must yield nil")
	}
}
