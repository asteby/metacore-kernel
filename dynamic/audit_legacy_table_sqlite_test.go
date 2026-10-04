package dynamic

// Same regression as audit_legacy_table_pg_test.go, on in-memory SQLite (runs
// without Postgres): hosts' tests create tables by hand without the newer audit
// columns and Service writes must not name them.

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

func newLegacySQLiteService(t *testing.T, ddl string) (*Service, *gorm.DB, string, uuid.UUID) {
	t.Helper()
	db := setupTestDB(t)
	table := "audit_legacy_sq_" + uuid.NewString()[:8]
	mustExec(t, db, fmt.Sprintf(ddl, table))
	model := "AuditLegacySQ" + table
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

func TestAuditLegacyTableSQLite_WritesWorkWithoutMissingAuditColumns(t *testing.T) {
	variants := map[string]string{
		"created_by_only": `CREATE TABLE %s (id TEXT PRIMARY KEY, organization_id TEXT, label TEXT,
			created_at DATETIME, updated_at DATETIME, deleted_at DATETIME, created_by_id TEXT)`,
		"id_org_created_at_only": `CREATE TABLE %s (id TEXT PRIMARY KEY, organization_id TEXT, label TEXT, created_at DATETIME)`,
	}
	for name, ddl := range variants {
		t.Run(name, func(t *testing.T) {
			svc, db, model, org := newLegacySQLiteService(t, ddl)
			table, _ := svc.tableNameResolver(context.Background(), model)
			user := newUser(org)
			ctx := context.Background()
			id := uuid.New()
			if _, err := svc.Create(ctx, model, user, map[string]any{"id": id.String(), "label": "a"}); err != nil {
				t.Fatalf("create: %v", err)
			}
			row := map[string]any{}
			db.Raw(fmt.Sprintf(`SELECT * FROM %s WHERE id = ?`, table), id.String()).Scan(&row)
			if _, ok := row["updated_by_id"]; ok {
				t.Errorf("updated_by_id appeared on a table that never had it")
			}
			if _, ok := row["created_by_id"]; ok && row["created_by_id"] != user.id.String() {
				t.Errorf("created_by_id = %v, want %s", row["created_by_id"], user.id)
			}
			if _, err := svc.Update(ctx, model, user, id, map[string]any{"label": "b"}); err != nil {
				t.Fatalf("update: %v", err)
			}
			if err := svc.Delete(ctx, model, user, id); err != nil {
				t.Fatalf("delete: %v", err)
			}
		})
	}
}
