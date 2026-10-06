package dynamic

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"github.com/asteby/metacore-kernel/manifest"
	"github.com/asteby/metacore-kernel/metadata"
	"github.com/asteby/metacore-kernel/modelbase"
)

// Una póliza contabilizada no se edita ni se borra: la etapa `posted` se
// declara `locked`. Solo se acepta el cambio de etapa por una transición
// declarada (posted → cancelled), y los campos reenviados sin cambio no cuentan.
func setupLockedFixture(t *testing.T) (*Service, *fakeUser, uuid.UUID) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{DisableForeignKeyConstraintWhenMigrating: true})
	if err != nil {
		t.Fatal(err)
	}
	db.Exec(`CREATE TABLE IF NOT EXISTS test_orders (
		id TEXT PRIMARY KEY, organization_id TEXT, created_by_id TEXT,
		created_at DATETIME, updated_at DATETIME, deleted_at DATETIME,
		reference TEXT, status TEXT)`)
	modelbase.Register("test_orders", func() modelbase.ModelDefiner { return &TestOrder{} })
	sm := &StageMachine{
		Field: "status",
		Stages: []manifest.StageDef{
			{Key: "draft", Order: 0},
			{Key: "posted", Order: 1, Locked: true},
			{Key: "cancelled", Order: 2, IsFinal: true, Locked: true},
		},
		Transitions: []manifest.TransitionDef{
			{From: "draft", To: "posted"}, {From: "posted", To: "cancelled"}, {From: "draft", To: "cancelled"},
		},
	}
	svc := New(Config{
		DB:       db,
		Metadata: metadata.New(metadata.Config{CacheTTL: -1}),
		StageMachineResolver: func(_ context.Context, model string) (*StageMachine, bool) {
			return sm, model == "test_orders"
		},
	})
	user := newUser(uuid.New())
	out, err := svc.Create(context.Background(), "test_orders", user, map[string]any{"reference": "POL-1", "status": "draft"})
	if err != nil {
		t.Fatal(err)
	}
	return svc, user, uuid.MustParse(out["id"].(string))
}

func isLocked(err error) bool {
	var ve *ValidationError
	if !errors.As(err, &ve) {
		return false
	}
	for _, issues := range ve.Fields {
		for _, is := range issues {
			if is.Code == CodeRecordLocked {
				return true
			}
		}
	}
	return false
}

func TestStageLock_PostedIsReadOnly(t *testing.T) {
	svc, user, id := setupLockedFixture(t)
	ctx := context.Background()

	// En borrador se edita libremente.
	if _, err := svc.Update(ctx, "test_orders", user, id, map[string]any{"reference": "POL-1b"}); err != nil {
		t.Fatalf("draft edit: %v", err)
	}
	if _, err := svc.Update(ctx, "test_orders", user, id, map[string]any{"status": "posted"}); err != nil {
		t.Fatalf("post: %v", err)
	}
	// Contabilizada: cambiar un campo es record_locked.
	if _, err := svc.Update(ctx, "test_orders", user, id, map[string]any{"reference": "otra"}); !isLocked(err) {
		t.Fatalf("edit of a posted row: err = %v, want record_locked", err)
	}
	// El formulario reenvía todo sin cambios: no es una edición.
	if _, err := svc.Update(ctx, "test_orders", user, id, map[string]any{"reference": "POL-1b", "status": "posted"}); err != nil {
		t.Fatalf("unchanged resubmit: %v", err)
	}
	// No vuelve a borrador (no hay transición) ni se borra.
	if _, err := svc.Update(ctx, "test_orders", user, id, map[string]any{"status": "draft"}); err == nil {
		t.Fatal("posted → draft accepted")
	}
	if err := svc.Delete(ctx, "test_orders", user, id); !isLocked(err) {
		t.Fatalf("delete of a posted row: err = %v, want record_locked", err)
	}
	// La transición declarada sí pasa.
	if _, err := svc.Update(ctx, "test_orders", user, id, map[string]any{"status": "cancelled"}); err != nil {
		t.Fatalf("posted → cancelled: %v", err)
	}
}

func TestStageLock_DraftDeleteAllowed(t *testing.T) {
	svc, user, id := setupLockedFixture(t)
	if err := svc.Delete(context.Background(), "test_orders", user, id); err != nil {
		t.Fatalf("draft delete: %v", err)
	}
}

func TestFromV3_StageLockedCarried(t *testing.T) {
	sm := StageMachine{Field: "state", Stages: []manifest.StageDef{{Key: "posted", Locked: true}, {Key: "draft"}}}
	if !sm.LockedAt("posted") || sm.LockedAt("draft") || sm.LockedAt("") {
		t.Fatal("LockedAt")
	}
}
