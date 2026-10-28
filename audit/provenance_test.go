package audit

import (
	"context"
	"testing"

	"github.com/asteby/metacore-kernel/dynamic"
	"github.com/asteby/metacore-kernel/provenance"
	"github.com/google/uuid"
)

func TestRecorder_StoresProvenance(t *testing.T) {
	db := setupTestDB(t, DefaultTableName)
	if err := db.Exec(`ALTER TABLE ` + DefaultTableName + ` ADD COLUMN provenance TEXT`).Error; err != nil {
		t.Fatal(err)
	}
	r := NewRecorder(db)
	user := uuid.New()
	p := provenance.Assisted("Asistente", user, provenance.ViaChat).WithSummary("Propuso crear")
	ev := &dynamic.CanonicalEvent{ID: uuid.NewString(), Model: "findings", Action: "created", ActorID: user.String(), After: map[string]any{"title": "x"}, Provenance: &p}
	if err := r.Handle(context.Background(), uuid.New(), ev); err != nil {
		t.Fatal(err)
	}
	var e Entry
	if err := db.Table(DefaultTableName).First(&e).Error; err != nil {
		t.Fatal(err)
	}
	if e.Provenance == nil {
		t.Fatal("provenance not stored")
	}
	if got := provenance.Parse(*e.Provenance); got.Mode != provenance.ModeAssisted || got.AuthorizedBy == nil || *got.AuthorizedBy != user {
		t.Fatalf("stored: %s", *e.Provenance)
	}
}

// Tables created before the column keep working when the event has none.
func TestRecorder_LegacyTableWithoutProvenanceColumn(t *testing.T) {
	db := setupTestDB(t, "activity_logs_legacy")
	r := NewRecorder(db, WithTableName("activity_logs_legacy"))
	ev := &dynamic.CanonicalEvent{ID: uuid.NewString(), Model: "products", Action: "created", After: map[string]any{"name": "x"}}
	if err := r.Handle(context.Background(), uuid.New(), ev); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, db, "activity_logs_legacy"); n != 1 {
		t.Fatalf("rows = %d", n)
	}
}
