package metadata

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/asteby/metacore-kernel/modelbase"
)

type auditedModel struct {
	modelbase.BaseUUIDModel
	Name string `json:"name"`
}

func (auditedModel) TableName() string { return "audited" }
func (auditedModel) DefineTable() modelbase.TableMetadata {
	return modelbase.TableMetadata{Title: "Audited"}
}
func (auditedModel) DefineModal() modelbase.ModalMetadata { return modelbase.ModalMetadata{} }

// The served table metadata names the audit row keys so the SDK can render
// "Creado por / Modificado por" without the manifest declaring the columns.
func TestGetTable_ProjectsAuditMeta(t *testing.T) {
	key := fmt.Sprintf("audited_%d", time.Now().UnixNano())
	modelbase.Register(key, func() modelbase.ModelDefiner { return &auditedModel{} })
	meta, err := New(Config{CacheTTL: -1}).GetTable(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	if meta.Audit == nil || meta.Audit.CreatedAt != "created_at" || meta.Audit.CreatedBy != "created_by_id" || meta.Audit.DeletedAt != "deleted_at" {
		t.Fatalf("audit = %+v", meta.Audit)
	}
	raw, _ := json.Marshal(meta)
	var back map[string]any
	_ = json.Unmarshal(raw, &back)
	a, _ := back["audit"].(map[string]any)
	if a["created_by"] != "created_by_id" || a["created_at"] != "created_at" {
		t.Fatalf("wire shape = %s", raw)
	}

	// A model without audit fields keeps the payload unchanged (no `audit`).
	plain := registerFakeModel(t, "Plain")
	pm, _ := New(Config{CacheTTL: -1}).GetTable(context.Background(), plain)
	if pm.Audit != nil {
		t.Fatalf("model without audit columns must serve no audit block, got %+v", pm.Audit)
	}
}
