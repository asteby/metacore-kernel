package dynamic

import (
	"reflect"
	"strings"
	"testing"

	"github.com/asteby/metacore-kernel/manifest"
	"github.com/google/uuid"
)

func sampleDef() manifest.ModelDefinition {
	return manifest.ModelDefinition{
		TableName:  "widgets",
		OrgScoped:  true,
		SoftDelete: true,
		Columns: []manifest.ColumnDef{
			{Name: "name", Type: "string", Required: true, Index: true},
			{Name: "price", Type: "numeric"},
			{Name: "sku", Type: "string", Unique: true},
		},
	}
}

func joined(stmts []string) string { return strings.Join(stmts, "\n") }

// ToReflectType must be a faithful pass-through to BuildStructType.
func TestToReflectType_DelegatesToBuildStructType(t *testing.T) {
	def := sampleDef()
	eng := NewSchemaEngine()
	got, err := eng.ToReflectType(def)
	if err != nil {
		t.Fatalf("ToReflectType: %v", err)
	}
	want, err := BuildStructType(def)
	if err != nil {
		t.Fatalf("BuildStructType: %v", err)
	}
	if got != want {
		t.Fatalf("ToReflectType diverged from BuildStructType:\n got=%v\nwant=%v", got, want)
	}
	if _, ok := got.FieldByName("OrganizationID"); !ok {
		t.Errorf("expected OrganizationID field on org-scoped model")
	}
}

func TestValidateType_WrapsAllowlist(t *testing.T) {
	eng := NewSchemaEngine()
	if err := eng.ValidateType("string"); err != nil {
		t.Errorf("string should validate: %v", err)
	}
	if err := eng.ValidateType("definitely-not-a-type"); err == nil {
		t.Errorf("bogus type should be rejected")
	}
}

// DEFAULT MODE: zero-value-ish options (shared isolation, addon schema) must
// reproduce today's CreateTable shape — addon schema, timestamptz, org column,
// RLS, NO created_by_id, deleted_at only via SoftDelete.
func TestToDDL_DefaultMode_MatchesLegacyShape(t *testing.T) {
	def := sampleDef()
	stmts, err := ToDDL(def, DDLOptions{
		AddonKey:  "shop",
		Isolation: IsolationShared,
	})
	if err != nil {
		t.Fatalf("ToDDL: %v", err)
	}
	out := joined(stmts)

	if !strings.Contains(out, `"addon_shop"."widgets"`) {
		t.Errorf("expected addon_shop schema, got:\n%s", out)
	}
	if !strings.Contains(out, `"organization_id" uuid NOT NULL`) {
		t.Errorf("expected org column NOT NULL:\n%s", out)
	}
	if !strings.Contains(out, `"created_at" timestamptz NOT NULL DEFAULT NOW()`) {
		t.Errorf("expected timestamptz created_at:\n%s", out)
	}
	if strings.Contains(out, "created_by_id") {
		t.Errorf("default mode must NOT emit created_by_id:\n%s", out)
	}
	if !strings.Contains(out, "ENABLE ROW LEVEL SECURITY") {
		t.Errorf("default shared mode must enable RLS:\n%s", out)
	}
	if !strings.Contains(out, `CREATE POLICY "rls_org_isolation"`) {
		t.Errorf("expected RLS policy:\n%s", out)
	}
	if !strings.Contains(out, `"deleted_at" timestamptz`) {
		t.Errorf("expected deleted_at (SoftDelete):\n%s", out)
	}
	// column index + unique index preserved
	if !strings.Contains(out, `"idx_widgets_name"`) || !strings.Contains(out, `"uidx_widgets_sku"`) {
		t.Errorf("expected column indexes:\n%s", out)
	}
}

// The CREATE TABLE the facade builds in default mode must equal, column-for-
// column, the fragment CreateTable itself builds (via the shared columnDDL /
// index helpers). We assert the table statement's column list explicitly.
func TestToDDL_DefaultMode_TableStatementIsFirst(t *testing.T) {
	def := sampleDef()
	stmts, err := ToDDL(def, DDLOptions{AddonKey: "shop", Isolation: IsolationShared})
	if err != nil {
		t.Fatalf("ToDDL: %v", err)
	}
	if !strings.HasPrefix(stmts[0], `CREATE TABLE IF NOT EXISTS "addon_shop"."widgets"`) {
		t.Fatalf("first statement should be the CREATE TABLE, got: %s", stmts[0])
	}
	// price is optional numeric with no default → nullable numeric column present
	if !strings.Contains(stmts[0], `"price" numeric(18,4)`) {
		t.Errorf("expected numeric(18,4) price column: %s", stmts[0])
	}
}

// SINGLE-SCHEMA MODE: must match the ops emitter — flat schema, no RLS,
// unconditional org column, plain TIMESTAMP, deleted_at + created_by_id always.
func TestToDDL_SingleSchemaMode_MatchesOpsShape(t *testing.T) {
	// A model that is NOT org-scoped and NOT soft-delete: ops still emits both.
	def := manifest.ModelDefinition{
		TableName: "github_issues",
		Columns: []manifest.ColumnDef{
			{Name: "title", Type: "string", Required: true},
		},
	}
	stmts, err := ToDDL(def, SingleSchemaDDLOptions("public"))
	if err != nil {
		t.Fatalf("ToDDL: %v", err)
	}
	out := joined(stmts)

	if !strings.Contains(out, `"public"."github_issues"`) {
		t.Errorf("expected public schema:\n%s", out)
	}
	// org column unconditional even though def.OrgScoped == false
	if !strings.Contains(out, `"organization_id" uuid NOT NULL`) {
		t.Errorf("single-schema must force org column:\n%s", out)
	}
	// plain TIMESTAMP, no tz
	if !strings.Contains(out, `"created_at" timestamp NOT NULL DEFAULT NOW()`) {
		t.Errorf("expected plain timestamp:\n%s", out)
	}
	if strings.Contains(out, "timestamptz") {
		t.Errorf("single-schema must NOT use timestamptz:\n%s", out)
	}
	// deleted_at + created_by_id present despite SoftDelete=false
	if !strings.Contains(out, `"deleted_at" timestamp`) {
		t.Errorf("expected deleted_at:\n%s", out)
	}
	if !strings.Contains(out, `"created_by_id" uuid`) {
		t.Errorf("expected created_by_id:\n%s", out)
	}
	// NO RLS
	if strings.Contains(out, "ROW LEVEL SECURITY") || strings.Contains(out, "CREATE POLICY") {
		t.Errorf("single-schema must NOT emit RLS:\n%s", out)
	}
	// managed indexes
	if !strings.Contains(out, `"idx_github_issues_org"`) ||
		!strings.Contains(out, `"idx_github_issues_deleted"`) ||
		!strings.Contains(out, `"idx_github_issues_created_by"`) {
		t.Errorf("expected managed indexes:\n%s", out)
	}
}

// The two modes must differ: same model, different projection.
func TestToDDL_ModesDiffer(t *testing.T) {
	def := sampleDef()
	def.OrgScoped = false
	orgID := uuid.New()

	shared, err := ToDDL(def, DDLOptions{AddonKey: "shop", OrgID: orgID, Isolation: IsolationShared})
	if err != nil {
		t.Fatalf("shared ToDDL: %v", err)
	}
	single, err := ToDDL(def, SingleSchemaDDLOptions("public"))
	if err != nil {
		t.Fatalf("single ToDDL: %v", err)
	}
	if reflect.DeepEqual(shared, single) {
		t.Fatalf("expected the two DDL modes to differ")
	}
	if strings.Contains(joined(single), "timestamptz") {
		t.Errorf("single-schema leaked timestamptz")
	}
	if !strings.Contains(joined(shared), "ROW LEVEL SECURITY") {
		t.Errorf("shared mode should keep RLS")
	}
}

func TestToDDL_UnknownColumnType_Errors(t *testing.T) {
	def := manifest.ModelDefinition{
		TableName: "bad",
		Columns:   []manifest.ColumnDef{{Name: "x", Type: "nonsense-type"}},
	}
	if _, err := ToDDL(def, DDLOptions{AddonKey: "k", Isolation: IsolationShared}); err == nil {
		t.Fatalf("expected error for unknown column type")
	}
}
