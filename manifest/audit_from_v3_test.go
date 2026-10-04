package manifest

import (
	"testing"

	"github.com/asteby/metacore-kernel/manifest/v3"
)

func TestFromV3_AuditFalseAndDeclaredColumns(t *testing.T) {
	off := false
	m := &v3.Manifest{Models: []v3.Model{
		{Key: "A", Table: "a", Audit: &off, Columns: []v3.Column{{Name: "id", Type: "uuid"}}},
		{Key: "B", Table: "b", Columns: []v3.Column{
			{Name: "id", Type: "uuid"},
			{Name: "deleted_at", Type: "timestamptz"},
			{Name: "created_by_id", Type: "uuid"},
		}},
	}}
	defs := FromV3(m).ModelDefinitions
	if len(defs) != 2 {
		t.Fatalf("defs = %d", len(defs))
	}
	if !defs[0].NoAudit {
		t.Error("audit:false must set NoAudit")
	}
	if defs[1].NoAudit {
		t.Error("audit defaults to on")
	}
	// deleted_at folds into SoftDelete; the declared created_by_id stays a
	// column (the manifest declaration wins over the kernel's).
	if !defs[1].SoftDelete {
		t.Error("declared deleted_at must keep mapping to SoftDelete")
	}
	found := false
	for _, c := range defs[1].Columns {
		if c.Name == "created_by_id" {
			found = true
		}
	}
	if !found {
		t.Error("declared created_by_id must stay as a column")
	}
}
