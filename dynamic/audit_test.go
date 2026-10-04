package dynamic

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/asteby/metacore-kernel/manifest"
	"github.com/asteby/metacore-kernel/metadata"
	"github.com/asteby/metacore-kernel/modelbase"
)

func auditDef() manifest.ModelDefinition {
	return manifest.ModelDefinition{
		TableName: "audit_items", ModelKey: "AuditItem", OrgScoped: true,
		Columns: []manifest.ColumnDef{{Name: "label", Type: "string"}},
	}
}

func TestToDDL_DefaultIncludesSixAuditColumnsAndIndexes(t *testing.T) {
	stmts, err := ToDDL(auditDef(), DDLOptions{AddonKey: "aud", Isolation: IsolationShared})
	if err != nil {
		t.Fatal(err)
	}
	out := joined(stmts)
	for _, want := range []string{
		`"created_at" timestamptz NOT NULL DEFAULT NOW()`,
		`"updated_at" timestamptz NOT NULL DEFAULT NOW()`,
		`"deleted_at" timestamptz`,
		`"created_by_id" uuid`,
		`"updated_by_id" uuid`,
		`"deleted_by_id" uuid`,
		`"idx_audit_items_deleted" ON "addon_aud"."audit_items" ("deleted_at")`,
		`"idx_audit_items_org_deleted"`,
		`"idx_audit_items_created_by" ON "addon_aud"."audit_items" ("created_by_id")`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("default DDL missing %q:\n%s", want, out)
		}
	}
}

func TestToDDL_AppendOnlyKeepsLedgerSubset(t *testing.T) {
	def := auditDef()
	def.AppendOnly = true
	stmts, err := ToDDL(def, DDLOptions{AddonKey: "aud", Isolation: IsolationShared})
	if err != nil {
		t.Fatal(err)
	}
	out := joined(stmts)
	for _, want := range []string{`"created_at"`, `"created_by_id" uuid`} {
		if !strings.Contains(out, want) {
			t.Errorf("append-only DDL missing %q:\n%s", want, out)
		}
	}
	for _, bad := range []string{`"updated_at"`, `"deleted_at"`, `"updated_by_id"`, `"deleted_by_id"`} {
		if strings.Contains(out, bad) {
			t.Errorf("append-only DDL must not carry %s:\n%s", bad, out)
		}
	}
}

func TestToDDL_AuditFalseKeepsHistoricalShape(t *testing.T) {
	def := auditDef()
	def.NoAudit = true
	out := func(d manifest.ModelDefinition, o DDLOptions) string {
		s, err := ToDDL(d, o)
		if err != nil {
			t.Fatal(err)
		}
		return joined(s)
	}
	got := out(def, DDLOptions{AddonKey: "aud", Isolation: IsolationShared})
	for _, want := range []string{`"created_at"`, `"updated_at"`} {
		if !strings.Contains(got, want) {
			t.Errorf("audit:false DDL missing %s", want)
		}
	}
	for _, bad := range []string{`"deleted_at"`, `"created_by_id"`, `"updated_by_id"`, `"deleted_by_id"`} {
		if strings.Contains(got, bad) {
			t.Errorf("audit:false DDL must not carry %s:\n%s", bad, got)
		}
	}
	// The legacy host switches still force their column on.
	forced := out(def, DDLOptions{AddonKey: "aud", Isolation: IsolationShared, AlwaysSoftDelete: true, IncludeCreatedBy: true})
	if !strings.Contains(forced, `"deleted_at"`) || !strings.Contains(forced, `"created_by_id" uuid`) {
		t.Errorf("AlwaysSoftDelete/IncludeCreatedBy must still force their columns:\n%s", forced)
	}
	// soft_delete declared by the manifest keeps its tombstone.
	def.SoftDelete = true
	if !strings.Contains(out(def, DDLOptions{AddonKey: "aud", Isolation: IsolationShared}), `"deleted_at"`) {
		t.Error("audit:false + soft_delete must keep deleted_at")
	}
}

func TestToDDL_ManifestDeclaredAuditColumnWins(t *testing.T) {
	def := auditDef()
	def.Columns = append(def.Columns, manifest.ColumnDef{Name: "created_by_id", Type: "uuid"})
	stmts, err := ToDDL(def, DDLOptions{AddonKey: "aud", Isolation: IsolationShared})
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(joined(stmts), `"created_by_id" uuid`); n != 1 {
		t.Fatalf("created_by_id must appear once (declared wins, kernel adds only the missing), got %d:\n%s", n, joined(stmts))
	}
	if !strings.Contains(joined(stmts), `"updated_by_id" uuid`) {
		t.Error("kernel must still add the missing updated_by_id")
	}
}

func TestToDDL_SingleSchemaPresetUnchanged(t *testing.T) {
	// Redundant switches stay accepted: the ops preset still yields TIMESTAMP
	// audit columns, now with the who-columns as well.
	stmts, err := ToDDL(auditDef(), SingleSchemaDDLOptions("public"))
	if err != nil {
		t.Fatal(err)
	}
	out := joined(stmts)
	for _, want := range []string{`"deleted_at" timestamp`, `"created_by_id" uuid`, `"updated_by_id" uuid`, `"deleted_by_id" uuid`} {
		if !strings.Contains(out, want) {
			t.Errorf("ops preset DDL missing %q:\n%s", want, out)
		}
	}
}

// The customers@037 case: a table materialized WITHOUT deleted_at /
// created_by_id must be brought up to the standard before migrations run.
func TestAuditColumnsDDL_UpgradeAddsMissingColumnsAndIndexes(t *testing.T) {
	existing := map[string]struct{}{"id": {}, "organization_id": {}, "label": {}, "created_at": {}, "updated_at": {}}
	stmts := SchemaEngine{}.AuditColumnsDDL("public", auditDef(), existing, DDLOptions{})
	out := joined(stmts)
	for _, want := range []string{
		`ALTER TABLE "public"."audit_items" ADD COLUMN IF NOT EXISTS "deleted_at" timestamptz`,
		`ALTER TABLE "public"."audit_items" ADD COLUMN IF NOT EXISTS "created_by_id" uuid`,
		`ALTER TABLE "public"."audit_items" ADD COLUMN IF NOT EXISTS "updated_by_id" uuid`,
		`ALTER TABLE "public"."audit_items" ADD COLUMN IF NOT EXISTS "deleted_by_id" uuid`,
		`CREATE INDEX IF NOT EXISTS "idx_audit_items_deleted"`,
		`CREATE INDEX IF NOT EXISTS "idx_audit_items_created_by"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("upgrade DDL missing %q:\n%s", want, out)
		}
	}
	// Columns the table already has are not re-added; ADD COLUMN precedes the
	// indexes (the order matters: an index on a missing column would 42703).
	for _, bad := range []string{`ADD COLUMN IF NOT EXISTS "created_at"`, `ADD COLUMN IF NOT EXISTS "updated_at"`} {
		if strings.Contains(out, bad) {
			t.Errorf("must not re-add an existing column: %s", bad)
		}
	}
	if strings.Index(out, "ADD COLUMN") > strings.Index(out, "CREATE INDEX") {
		t.Errorf("ADD COLUMN must come before CREATE INDEX:\n%s", out)
	}
	// Idempotent: a fully up-to-date table only (re)asserts indexes.
	full := map[string]struct{}{}
	for _, c := range AuditColumns(auditDef(), false, false) {
		full[c.Name] = struct{}{}
	}
	full["organization_id"] = struct{}{}
	for _, s := range (SchemaEngine{}).AuditColumnsDDL("public", auditDef(), full, DDLOptions{}) {
		if strings.HasPrefix(s, "ALTER") {
			t.Errorf("up-to-date table must emit no ALTER, got %s", s)
		}
	}
}

func TestBuildStructType_AuditFields(t *testing.T) {
	has := func(typ reflect.Type, name string) bool { _, ok := typ.FieldByName(name); return ok }
	typ, err := BuildStructType(auditDef())
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"CreatedAt", "UpdatedAt", "DeletedAt", "CreatedByID", "UpdatedByID", "DeletedByID"} {
		if !has(typ, n) {
			t.Errorf("default struct missing %s", n)
		}
	}
	if f, _ := typ.FieldByName("DeletedAt"); f.Type != reflect.TypeOf(gorm.DeletedAt{}) {
		t.Errorf("DeletedAt must be gorm.DeletedAt (soft delete), got %v", f.Type)
	}
	ao := auditDef()
	ao.AppendOnly = true
	typ, _ = BuildStructType(ao)
	if !has(typ, "CreatedByID") || has(typ, "UpdatedAt") || has(typ, "DeletedAt") || has(typ, "UpdatedByID") {
		t.Error("append-only struct must carry only created_at/created_by_id")
	}
	na := auditDef()
	na.NoAudit = true
	typ, _ = BuildStructType(na)
	if has(typ, "CreatedByID") || has(typ, "DeletedAt") || !has(typ, "UpdatedAt") {
		t.Error("audit:false struct must keep only created_at/updated_at")
	}
}

func TestActorOrSystem(t *testing.T) {
	id := uuid.New()
	if got := ActorOrSystem(id.String()); got != id.String() {
		t.Errorf("valid actor must pass through, got %s", got)
	}
	for _, in := range []string{"", "not-a-uuid", uuid.Nil.String()} {
		if got := ActorOrSystem(in); got != SystemActorID.String() {
			t.Errorf("ActorOrSystem(%q) = %s, want SystemActorID", in, got)
		}
	}
}

func TestAuditMetaFor(t *testing.T) {
	m := AuditMetaFor(auditDef(), false, false)
	want := modelbase.AuditMeta{CreatedAt: "created_at", CreatedBy: "created_by_id", UpdatedAt: "updated_at", UpdatedBy: "updated_by_id", DeletedAt: "deleted_at", DeletedBy: "deleted_by_id"}
	if *m != want {
		t.Errorf("AuditMetaFor = %+v, want %+v", *m, want)
	}
	typ, _ := BuildStructType(auditDef())
	if d := modelbase.DeriveAuditMeta(reflect.New(typ).Interface()); d == nil || *d != want {
		t.Errorf("DeriveAuditMeta over the built struct = %+v, want %+v", d, want)
	}
	// BaseUUIDModel (json:"-" DeletedAt) is recognised by field name.
	if d := modelbase.DeriveAuditMeta(&TestProduct{}); d == nil || d.DeletedAt != "deleted_at" || d.CreatedBy != "created_by_id" || d.UpdatedBy != "" {
		t.Errorf("DeriveAuditMeta(BaseUUIDModel) = %+v", d)
	}
}

// ---- runtime: Service stamps actor + dates, ignores forged values ----------

type auditItemsMeta struct {
	modelbase.BaseUUIDModel
	Label string `json:"label"`
}

func (auditItemsMeta) TableName() string { return "audit_items" }
func (auditItemsMeta) DefineTable() modelbase.TableMetadata {
	return modelbase.TableMetadata{Title: "Audit Items", Columns: []modelbase.ColumnDef{{Key: "label", Label: "Label"}}}
}
func (auditItemsMeta) DefineModal() modelbase.ModalMetadata {
	return modelbase.ModalMetadata{Title: "Audit Item"}
}

func newAuditService(t *testing.T) (*Service, *gorm.DB, uuid.UUID) {
	t.Helper()
	db := setupTestDB(t)
	if err := db.Exec(`CREATE TABLE audit_items (
		id TEXT PRIMARY KEY, organization_id TEXT, label TEXT,
		created_at DATETIME, updated_at DATETIME, deleted_at DATETIME,
		created_by_id TEXT, updated_by_id TEXT, deleted_by_id TEXT)`).Error; err != nil {
		t.Fatal(err)
	}
	modelbase.Register("audit_items", func() modelbase.ModelDefiner { return &auditItemsMeta{} })
	typ, err := BuildStructType(auditDef())
	if err != nil {
		t.Fatal(err)
	}
	svc := New(Config{
		DB:       db,
		Metadata: metadata.New(metadata.Config{CacheTTL: -1}),
		ModelResolver: func(_ context.Context, name string) (any, bool) {
			if name != "audit_items" {
				return nil, false
			}
			return reflect.New(typ).Interface(), true
		},
		TableNameResolver: func(_ context.Context, name string) (string, bool) { return "audit_items", name == "audit_items" },
	})
	return svc, db, uuid.New()
}

func rawAudit(t *testing.T, db *gorm.DB, id string) map[string]any {
	t.Helper()
	row := map[string]any{}
	if err := db.Raw(`SELECT * FROM audit_items WHERE id = ?`, id).Scan(&row).Error; err != nil {
		t.Fatal(err)
	}
	return row
}

func TestService_StampsActorAndDatesAndIgnoresForgedValues(t *testing.T) {
	svc, db, org := newAuditService(t)
	creator := newUser(org)
	editor := newUser(org)
	deleter := newUser(org)
	ctx := context.Background()
	forged := uuid.New().String()
	id := uuid.New()

	// Create: client-supplied audit values are discarded.
	if _, err := svc.Create(ctx, "audit_items", creator, map[string]any{
		"id": id.String(), "label": "a",
		"created_by_id": forged, "updated_by_id": forged, "deleted_by_id": forged,
		"created_at": "2001-01-01T00:00:00Z", "deleted_at": "2001-01-01T00:00:00Z",
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	row := rawAudit(t, db, id.String())
	if row["created_by_id"] != creator.id.String() || row["updated_by_id"] != creator.id.String() {
		t.Fatalf("created_by/updated_by = %v/%v, want creator %s", row["created_by_id"], row["updated_by_id"], creator.id)
	}
	if row["deleted_by_id"] != nil || row["deleted_at"] != nil {
		t.Fatalf("a forged tombstone must be discarded: %v / %v", row["deleted_by_id"], row["deleted_at"])
	}
	if ts, ok := row["created_at"].(time.Time); !ok || ts.Year() < 2020 {
		t.Fatalf("created_at must be server time, got %v", row["created_at"])
	}

	// Update: updated_by follows the editor, created_by is untouched, forged
	// values are discarded.
	if _, err := svc.Update(ctx, "audit_items", editor, id, map[string]any{
		"label": "b", "created_by_id": forged, "updated_by_id": forged, "deleted_at": "2001-01-01T00:00:00Z",
	}); err != nil {
		t.Fatalf("update: %v", err)
	}
	row = rawAudit(t, db, id.String())
	if row["updated_by_id"] != editor.id.String() {
		t.Fatalf("updated_by_id = %v, want editor %s", row["updated_by_id"], editor.id)
	}
	if row["created_by_id"] != creator.id.String() {
		t.Fatalf("created_by_id changed on update: %v", row["created_by_id"])
	}
	if row["deleted_at"] != nil {
		t.Fatalf("forged deleted_at must be discarded on update, got %v", row["deleted_at"])
	}

	// Delete: soft, stamped with deleted_at + deleted_by_id.
	if err := svc.Delete(ctx, "audit_items", deleter, id); err != nil {
		t.Fatalf("delete: %v", err)
	}
	row = rawAudit(t, db, id.String())
	if row["deleted_at"] == nil || row["deleted_by_id"] != deleter.id.String() {
		t.Fatalf("soft delete must stamp deleted_at+deleted_by_id, got %v / %v", row["deleted_at"], row["deleted_by_id"])
	}
	if _, err := svc.Get(ctx, "audit_items", creator, id); err != ErrRecordNotFound {
		t.Fatalf("a tombstoned row must be invisible, got %v", err)
	}

	// Restore: tombstone cleared, restorer recorded as the updater.
	restorer := newUser(org)
	if _, err := svc.Restore(ctx, "audit_items", restorer, id); err != nil {
		t.Fatalf("restore: %v", err)
	}
	row = rawAudit(t, db, id.String())
	if row["deleted_at"] != nil || row["deleted_by_id"] != nil || row["updated_by_id"] != restorer.id.String() {
		t.Fatalf("restore must clear the tombstone and stamp updated_by: %v", row)
	}
	if _, err := svc.Restore(ctx, "audit_items", restorer, id); err != ErrRecordNotFound {
		t.Fatalf("restoring a live row = %v, want ErrRecordNotFound", err)
	}
}

func TestService_SystemCallerStampsSystemActor(t *testing.T) {
	svc, db, org := newAuditService(t)
	id := uuid.New()
	if _, err := svc.Create(context.Background(), "audit_items", NewSystemCaller(org), map[string]any{"id": id.String(), "label": "cron"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	row := rawAudit(t, db, id.String())
	if row["created_by_id"] != SystemActorID.String() {
		t.Fatalf("created_by_id = %v, want SystemActorID", row["created_by_id"])
	}
}
