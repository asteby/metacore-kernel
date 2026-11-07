package dynamic

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/asteby/metacore-kernel/manifest"
	"github.com/asteby/metacore-kernel/metadata"
	"github.com/asteby/metacore-kernel/modelbase"
	"github.com/asteby/metacore-kernel/query"
)

// An optional bool column is created nullable, so NULL is a value the row can
// hold — mp_payments.sandbox stores it on purpose as "mode unknown". With a
// value-typed bool the generic CRUD loaded NULL as false and Update (First +
// Save) wrote that false back on the next edit or stage move of the row: an
// unknown-mode payment became a real one. These tests pin the three-state
// contract end to end through Service.Create/Update/Get/List.

type dynPayMeta struct {
	modelbase.BaseUUIDModel
}

func (dynPayMeta) TableName() string { return "dyn_pays" }
func (dynPayMeta) DefineTable() modelbase.TableMetadata {
	return modelbase.TableMetadata{Title: "Dyn Pays", Columns: []modelbase.ColumnDef{{Key: "status", Label: "Status"}}}
}
func (dynPayMeta) DefineModal() modelbase.ModalMetadata {
	return modelbase.ModalMetadata{Title: "Dyn Pay"}
}

// dynPayDef mirrors the shapes in play:
//   - sandbox: optional, default false (mp_payments.sandbox) → three states.
//   - paid: required, default false → NOT NULL value bool, unchanged.
//   - review: required, default true → the #569 pointer (explicit false on INSERT).
//   - tight: optional in the manifest but NOT NULL in the database (an addon
//     migration tightened it) → a JSON null must never reach it.
func dynPayDef() manifest.ModelDefinition {
	return manifest.ModelDefinition{
		TableName: "dyn_pays",
		OrgScoped: true,
		Columns: []manifest.ColumnDef{
			{Name: "status", Type: "string"},
			{Name: "sandbox", Type: "boolean", Default: "false"},
			{Name: "paid", Type: "boolean", Default: false, Required: true},
			{Name: "review", Type: "boolean", Default: true, Required: true},
			{Name: "tight", Type: "boolean", Default: false},
		},
	}
}

func TestColumnToField_OptionalBoolWithDefaultIsPointer(t *testing.T) {
	for _, def := range []any{false, "false", true, "true"} {
		f, err := columnToField(manifest.ColumnDef{Name: "sandbox", Type: "boolean", Default: def})
		if err != nil {
			t.Fatal(err)
		}
		if f.Type != reflect.PtrTo(boolType) {
			t.Fatalf("optional bool default %v = %v, want *bool", def, f.Type)
		}
		if !strings.Contains(string(f.Tag), "default:") || strings.Contains(string(f.Tag), "not null") {
			t.Fatalf("optional bool default %v tag = %s, want default and no not null", def, f.Tag)
		}
	}
	// Required + default false: NOT NULL value bool, exactly as before.
	f, err := columnToField(manifest.ColumnDef{Name: "paid", Type: "boolean", Default: false, Required: true})
	if err != nil {
		t.Fatal(err)
	}
	if f.Type != boolType {
		t.Fatalf("required bool default false = %v, want bool", f.Type)
	}
}

func TestBoolNullable_SQLite(t *testing.T) {
	db := setupTestDB(t)
	runBoolNullableScenario(t, db, `CREATE TABLE dyn_pays (
		id TEXT PRIMARY KEY,
		organization_id TEXT NOT NULL,
		status TEXT,
		sandbox BOOLEAN DEFAULT false,
		paid BOOLEAN NOT NULL DEFAULT false,
		review BOOLEAN NOT NULL DEFAULT true,
		tight BOOLEAN NOT NULL DEFAULT false,
		created_at DATETIME,
		updated_at DATETIME,
		deleted_at DATETIME
	)`)
}

// Run with a real Postgres (CI sets it; locally e.g.
// TEST_POSTGRES_DSN="host=localhost port=5432 user=postgres password=postgres dbname=kernel_test sslmode=disable").
func TestBoolNullable_Postgres(t *testing.T) {
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN not set")
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	schema := "bn_" + hex.EncodeToString(b)
	if err := db.Exec(`CREATE SCHEMA ` + schema).Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Exec(`DROP SCHEMA ` + schema + ` CASCADE`) })
	// search_path as a connection parameter: every pooled connection of the
	// service lands in the throwaway schema (no single-connection pool, which can
	// deadlock the cleanup when a scenario step fails mid-write).
	sdb, err := gorm.Open(postgres.Open(dsn+" search_path="+schema), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if sqlDB, err := sdb.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})
	runBoolNullableScenario(t, sdb, `CREATE TABLE dyn_pays (
		id uuid PRIMARY KEY,
		organization_id uuid NOT NULL,
		status varchar(255),
		sandbox boolean DEFAULT false,
		paid boolean NOT NULL DEFAULT false,
		review boolean NOT NULL DEFAULT true,
		tight boolean NOT NULL DEFAULT false,
		created_at timestamptz,
		updated_at timestamptz,
		deleted_at timestamptz
	)`)
}

func runBoolNullableScenario(t *testing.T, db *gorm.DB, ddl string) {
	t.Helper()
	if err := db.Exec(ddl).Error; err != nil {
		t.Fatal(err)
	}
	typ, err := BuildStructType(dynPayDef())
	if err != nil {
		t.Fatal(err)
	}
	modelbase.Register("dyn_pays", func() modelbase.ModelDefiner { return &dynPayMeta{} })
	svc := New(Config{
		AllowUnconfiguredPermissions: true,
		DB:       db,
		Metadata: metadata.New(metadata.Config{CacheTTL: -1}),
		ModelResolver: func(_ context.Context, name string) (any, bool) {
			if name != "dyn_pays" {
				return nil, false
			}
			return reflect.New(typ).Interface(), true
		},
		TableNameResolver: func(_ context.Context, name string) (string, bool) {
			return "dyn_pays", name == "dyn_pays"
		},
	})
	ctx := context.Background()
	user := newUser(uuid.New())

	// stored reads the physical value: nil = NULL.
	stored := func(id uuid.UUID, col string) *bool {
		t.Helper()
		var v *bool
		if err := db.Raw(`SELECT `+col+` FROM dyn_pays WHERE id = ?`, id).Scan(&v).Error; err != nil {
			t.Fatal(err)
		}
		return v
	}
	want := func(id uuid.UUID, col string, w *bool, step string) {
		t.Helper()
		got := stored(id, col)
		switch {
		case w == nil && got != nil:
			t.Fatalf("%s: %s = %v, want NULL", step, col, *got)
		case w != nil && got == nil:
			t.Fatalf("%s: %s = NULL, want %v", step, col, *w)
		case w != nil && *got != *w:
			t.Fatalf("%s: %s = %v, want %v", step, col, *got, *w)
		}
	}
	tru, fal := true, false
	create := func(in map[string]any) uuid.UUID {
		t.Helper()
		id := uuid.New()
		in["id"] = id.String()
		if _, err := svc.Create(ctx, "dyn_pays", user, in); err != nil {
			t.Fatalf("create %v: %v", in, err)
		}
		return id
	}
	update := func(id uuid.UUID, in map[string]any, step string) map[string]any {
		t.Helper()
		out, err := svc.Update(ctx, "dyn_pays", user, id, in)
		if err != nil {
			t.Fatalf("%s: update %v: %v", step, in, err)
		}
		return out
	}

	// --- Create: unset keys take the column DEFAULTs; explicit values are written.
	dflt := create(map[string]any{"status": "pending"})
	want(dflt, "sandbox", &fal, "create without sandbox")
	want(dflt, "paid", &fal, "create without paid")
	want(dflt, "review", &tru, "create without review")
	want(dflt, "tight", &fal, "create without tight")

	explicit := create(map[string]any{"status": "pending", "sandbox": true, "paid": true, "review": false})
	want(explicit, "sandbox", &tru, "create sandbox=true")
	want(explicit, "paid", &tru, "create paid=true")
	want(explicit, "review", &fal, "create review=false (#569 must stay fixed)")

	explicitFalse := create(map[string]any{"status": "pending", "sandbox": false})
	want(explicitFalse, "sandbox", &fal, "create sandbox=false")

	// --- The incident: a row whose sandbox is NULL ("mode unknown"), written
	// outside the generic CRUD (the addon's data_mutate).
	unknown := create(map[string]any{"status": "pending"})
	if err := db.Exec(`UPDATE dyn_pays SET sandbox = NULL WHERE id = ?`, unknown).Error; err != nil {
		t.Fatal(err)
	}

	// Read shape: Get and List serialize NULL as null, not false.
	got, err := svc.Get(ctx, "dyn_pays", user, unknown)
	if err != nil {
		t.Fatal(err)
	}
	if v, ok := got["sandbox"]; !ok || v != nil {
		t.Fatalf("Get sandbox = %#v (present=%v), want null", v, ok)
	}
	if got["paid"] != false || got["review"] != true {
		t.Fatalf("Get paid/review = %#v/%#v, want false/true", got["paid"], got["review"])
	}
	rows, _, err := svc.List(ctx, "dyn_pays", user, query.Params{Page: 1, PerPage: 50})
	if err != nil {
		t.Fatal(err)
	}
	seen := false
	for _, r := range rows {
		if r["id"] == unknown.String() {
			seen = true
			if r["sandbox"] != nil {
				t.Fatalf("List sandbox = %#v, want null", r["sandbox"])
			}
		}
		if r["id"] == explicitFalse.String() && r["sandbox"] != false {
			t.Fatalf("List sandbox of an explicit false row = %#v, want false", r["sandbox"])
		}
	}
	if !seen {
		t.Fatal("List did not return the NULL-sandbox row")
	}

	// Edit without the key / stage move (payload carries only the stage field):
	// sandbox stays NULL.
	out := update(unknown, map[string]any{"status": "paid"}, "edit without sandbox")
	want(unknown, "sandbox", nil, "edit without sandbox")
	if v, ok := out["sandbox"]; !ok || v != nil {
		t.Fatalf("Update response sandbox = %#v (present=%v), want null", v, ok)
	}
	update(unknown, map[string]any{"status": "shipped", "paid": true}, "second edit")
	want(unknown, "sandbox", nil, "second edit")
	want(unknown, "paid", &tru, "second edit")

	// Explicit false / true are written; explicit null clears back to NULL.
	update(unknown, map[string]any{"sandbox": false}, "sandbox=false")
	want(unknown, "sandbox", &fal, "sandbox=false")
	update(unknown, map[string]any{"sandbox": true}, "sandbox=true")
	want(unknown, "sandbox", &tru, "sandbox=true")
	update(unknown, map[string]any{"sandbox": nil}, "sandbox=null")
	want(unknown, "sandbox", nil, "sandbox=null")
	// String-encoded form (query-string / form posts) is coerced, not dropped.
	update(unknown, map[string]any{"sandbox": "false"}, `sandbox="false"`)
	want(unknown, "sandbox", &fal, `sandbox="false"`)

	// Edits that do not carry the key leave a stored true/false alone too.
	update(explicit, map[string]any{"status": "x"}, "edit true row")
	want(explicit, "sandbox", &tru, "edit true row")
	want(explicit, "review", &fal, "edit true row")

	// NOT NULL bools: unchanged behaviour. Explicit false is written...
	update(dflt, map[string]any{"paid": true, "review": false}, "not null set")
	want(dflt, "paid", &tru, "not null set")
	want(dflt, "review", &fal, "not null set")
	update(dflt, map[string]any{"paid": false, "review": true}, "not null reset")
	want(dflt, "paid", &fal, "not null reset")
	want(dflt, "review", &tru, "not null reset")
	// ...and a JSON null on a NOT NULL column never turns into a 23502: the
	// stored value is kept (a value bool ignored the null; a pointer whose
	// column is NOT NULL is omitted from the write).
	update(dflt, map[string]any{"paid": nil, "tight": nil, "status": "y"}, "null into not null")
	want(dflt, "paid", &fal, "null into not null")
	want(dflt, "tight", &fal, "null into not null")
	update(dflt, map[string]any{"tight": true}, "tight=true")
	want(dflt, "tight", &tru, "tight=true")
	update(dflt, map[string]any{"tight": nil}, "tight=null keeps true")
	want(dflt, "tight", &tru, "tight=null keeps true")
	nullCreate := create(map[string]any{"status": "pending", "tight": nil, "sandbox": nil})
	want(nullCreate, "tight", &fal, "create tight=null takes DEFAULT")
	// An explicit null on INSERT of a nullable column with a DEFAULT behaves as
	// before: GORM omits the nil default-tagged field, the DEFAULT applies.
	want(nullCreate, "sandbox", &fal, "create sandbox=null")
}
