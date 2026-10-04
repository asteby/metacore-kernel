package dynamic

// Regression (kernel v0.188.0): a table that pre-dates the audit standard and
// lacks updated_by_id / deleted_by_id (ops' SyncDynamicTableSchema only
// guarantees deleted_at + created_by_id) made EVERY Service write fail with
// SQLSTATE 42703. The runtime now stamps only the audit columns the table really
// has; EnsureAuditColumns brings the table to the standard and the stamping
// starts. Needs TEST_POSTGRES_DSN (skips otherwise).

import (
	"context"
	"fmt"
	"reflect"
	"testing"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/asteby/metacore-kernel/metadata"
	"github.com/asteby/metacore-kernel/modelbase"
)

func newLegacyAuditService(t *testing.T, createSQL string) (*Service, *gorm.DB, string, uuid.UUID) {
	t.Helper()
	db, sfx := pgTestDB(t)
	table := "audit_legacy_" + sfx
	mustExec(t, db, fmt.Sprintf(createSQL, table))
	t.Cleanup(func() { db.Exec("DROP TABLE IF EXISTS " + table) })
	model := "AuditLegacy" + sfx
	modelbase.Register(model, func() modelbase.ModelDefiner { return &auditItemsMeta{} })
	def := auditDef()
	def.TableName = table
	typ, err := BuildStructType(def)
	if err != nil {
		t.Fatal(err)
	}
	svc := New(Config{
		DB:       db,
		Metadata: metadata.New(metadata.Config{CacheTTL: -1}),
		ModelResolver: func(_ context.Context, name string) (any, bool) {
			if name != model {
				return nil, false
			}
			return reflect.New(typ).Interface(), true
		},
		TableNameResolver: func(_ context.Context, name string) (string, bool) { return table, name == model },
	})
	return svc, db, model, uuid.New()
}

func rawRow(t *testing.T, db *gorm.DB, table, id string) map[string]any {
	t.Helper()
	row := map[string]any{}
	if err := db.Raw(fmt.Sprintf(`SELECT * FROM %s WHERE id = ?`, table), id).Scan(&row).Error; err != nil {
		t.Fatal(err)
	}
	return row
}

func TestPostgresAuditLegacyTable_WritesWorkWithoutMissingAuditColumns(t *testing.T) {
	variants := map[string]struct {
		ddl        string
		hasCreator bool
	}{
		"created_by_only": {`CREATE TABLE %s (id uuid PRIMARY KEY, organization_id uuid, label text,
			created_at timestamptz NOT NULL DEFAULT NOW(), updated_at timestamptz NOT NULL DEFAULT NOW(),
			deleted_at timestamptz, created_by_id uuid)`, true},
		"id_org_created_at_only": {`CREATE TABLE %s (id uuid PRIMARY KEY, organization_id uuid, label text,
			created_at timestamptz NOT NULL DEFAULT NOW())`, false},
	}
	for name, v := range variants {
		t.Run(name, func(t *testing.T) {
			svc, db, model, org := newLegacyAuditService(t, v.ddl)
			table := svc.mustTable(t, model)
			user := newUser(org)
			ctx := context.Background()
			id := uuid.New()

			if _, err := svc.Create(ctx, model, user, map[string]any{"id": id.String(), "label": "a"}); err != nil {
				t.Fatalf("create on a legacy table: %v", err)
			}
			row := rawRow(t, db, table, id.String())
			if v.hasCreator && row["created_by_id"] != user.id.String() {
				t.Errorf("created_by_id = %v, want %s (the column exists: it must be stamped)", row["created_by_id"], user.id)
			}
			for _, absent := range []string{"updated_by_id", "deleted_by_id"} {
				if _, ok := row[absent]; ok {
					t.Errorf("%s appeared on a table that never had it", absent)
				}
			}
			if _, err := svc.Update(ctx, model, user, id, map[string]any{"label": "b"}); err != nil {
				t.Fatalf("update on a legacy table: %v", err)
			}
			if got := rawRow(t, db, table, id.String())["label"]; got != "b" {
				t.Errorf("label after update = %v, want b", got)
			}
			if err := svc.Delete(ctx, model, user, id); err != nil {
				t.Fatalf("delete on a legacy table: %v", err)
			}
			var n int64
			db.Raw(fmt.Sprintf(`SELECT count(*) FROM %s WHERE id = ? AND deleted_at IS NULL`, table), id.String()).Scan(&n)
			if v.hasCreator && n != 0 { // has deleted_at: tombstoned
				t.Errorf("row still live after delete (n=%d)", n)
			}
		})
	}
}

func TestPostgresAuditLegacyTable_EnsureAuditColumnsStartsStamping(t *testing.T) {
	svc, db, model, org := newLegacyAuditService(t, `CREATE TABLE %s (id uuid PRIMARY KEY, organization_id uuid, label text,
		created_at timestamptz NOT NULL DEFAULT NOW(), updated_at timestamptz NOT NULL DEFAULT NOW(),
		deleted_at timestamptz, created_by_id uuid)`)
	table := svc.mustTable(t, model)
	creator, editor := newUser(org), newUser(org)
	ctx := context.Background()
	id1, id2 := uuid.New(), uuid.New()

	// Before: works, nothing to stamp (and the cache now holds the legacy shape).
	if _, err := svc.Create(ctx, model, creator, map[string]any{"id": id1.String(), "label": "a"}); err != nil {
		t.Fatalf("create before: %v", err)
	}

	def := auditDef()
	def.TableName = table
	if err := EnsureAuditColumns(db, "public", def); err != nil {
		t.Fatalf("EnsureAuditColumns: %v", err)
	}

	// After: the cache was invalidated, so the new columns are stamped at once.
	if _, err := svc.Create(ctx, model, creator, map[string]any{"id": id2.String(), "label": "c"}); err != nil {
		t.Fatalf("create after: %v", err)
	}
	if got := rawRow(t, db, table, id2.String())["updated_by_id"]; got != creator.id.String() {
		t.Errorf("updated_by_id after Ensure = %v, want %s", got, creator.id)
	}
	if _, err := svc.Update(ctx, model, editor, id2, map[string]any{"label": "d"}); err != nil {
		t.Fatalf("update after: %v", err)
	}
	if got := rawRow(t, db, table, id2.String())["updated_by_id"]; got != editor.id.String() {
		t.Errorf("updated_by_id after update = %v, want %s", got, editor.id)
	}
	if err := svc.Delete(ctx, model, editor, id2); err != nil {
		t.Fatalf("delete after: %v", err)
	}
	if got := rawRow(t, db, table, id2.String())["deleted_by_id"]; got != editor.id.String() {
		t.Errorf("deleted_by_id after delete = %v, want %s", got, editor.id)
	}
}

func (s *Service) mustTable(t *testing.T, model string) string {
	t.Helper()
	tb, ok := s.tableNameResolver(context.Background(), model)
	if !ok {
		t.Fatalf("no table for %s", model)
	}
	return tb
}
