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

// --- ops-compat DDL divergences (dual-run ops#847) ---

// 1. float/double/real → double precision (numeric/decimal unaffected).
func TestToDDL_FloatAsDoublePrecision(t *testing.T) {
	// Baseline uses only kernel-native types (float/double); "real" is NOT a
	// native kernel type and is only resolvable under FloatAsDoublePrecision.
	base := manifest.ModelDefinition{
		TableName: "readings",
		Columns: []manifest.ColumnDef{
			{Name: "temp", Type: "float"},
			{Name: "ratio", Type: "double"},
			{Name: "amount", Type: "numeric"},
		},
	}
	off, err := ToDDL(base, DDLOptions{AddonKey: "k", Isolation: IsolationShared})
	if err != nil {
		t.Fatalf("baseline ToDDL: %v", err)
	}
	if !strings.Contains(joined(off), `"temp" numeric(18,4)`) {
		t.Errorf("without option float should stay numeric(18,4):\n%s", joined(off))
	}
	def := manifest.ModelDefinition{
		TableName: "readings",
		Columns: []manifest.ColumnDef{
			{Name: "temp", Type: "float"},
			{Name: "ratio", Type: "double"},
			{Name: "rate", Type: "real"},
			{Name: "amount", Type: "numeric"},
		},
	}
	on, _ := ToDDL(def, DDLOptions{Schema: "public", FloatAsDoublePrecision: true})
	out := joined(on)
	for _, col := range []string{`"temp" double precision`, `"ratio" double precision`, `"rate" double precision`} {
		if !strings.Contains(out, col) {
			t.Errorf("expected %s:\n%s", col, out)
		}
	}
	if !strings.Contains(out, `"amount" numeric(18,4)`) {
		t.Errorf("numeric must be unaffected by FloatAsDoublePrecision:\n%s", out)
	}
}

// 2. bool without declared default → DEFAULT false.
func TestToDDL_ImplicitBoolDefaultFalse(t *testing.T) {
	def := manifest.ModelDefinition{
		TableName: "flags",
		Columns: []manifest.ColumnDef{
			{Name: "active", Type: "bool"},
			{Name: "verified", Type: "bool", Default: true},
		},
	}
	off, _ := ToDDL(def, DDLOptions{Schema: "public"})
	if strings.Contains(joined(off), `"active" boolean DEFAULT`) {
		t.Errorf("without option bool must stay bare:\n%s", joined(off))
	}
	on, _ := ToDDL(def, DDLOptions{Schema: "public", ImplicitBoolDefaultFalse: true})
	out := joined(on)
	if !strings.Contains(out, `"active" boolean DEFAULT false`) {
		t.Errorf("expected implicit DEFAULT false:\n%s", out)
	}
	if !strings.Contains(out, `"verified" boolean DEFAULT true`) {
		t.Errorf("declared bool default must win:\n%s", out)
	}
}

// 3. jsonb without declared default → DEFAULT '{}'.
func TestToDDL_ImplicitJsonbDefaultObject(t *testing.T) {
	def := manifest.ModelDefinition{
		TableName: "docs",
		Columns:   []manifest.ColumnDef{{Name: "meta", Type: "jsonb"}},
	}
	off, _ := ToDDL(def, DDLOptions{Schema: "public"})
	if strings.Contains(joined(off), `"meta" jsonb DEFAULT`) {
		t.Errorf("without option jsonb must stay bare:\n%s", joined(off))
	}
	on, _ := ToDDL(def, DDLOptions{Schema: "public", ImplicitJsonbDefaultObject: true})
	if !strings.Contains(joined(on), `"meta" jsonb DEFAULT '{}'`) {
		t.Errorf("expected jsonb DEFAULT '{}':\n%s", joined(on))
	}
}

// 4. bareword string default → DEFAULT '<escaped>'.
func TestToDDL_QuoteBarewordStringDefaults(t *testing.T) {
	def := manifest.ModelDefinition{
		TableName: "orders",
		Columns: []manifest.ColumnDef{
			{Name: "status", Type: "string", Default: "draft"},
			{Name: "note", Type: "text", Default: "n'a"},
		},
	}
	off, _ := ToDDL(def, DDLOptions{Schema: "public"})
	if strings.Contains(joined(off), "DEFAULT 'draft'") {
		t.Errorf("without option bareword default must be dropped (DefaultLiteral rejects it):\n%s", joined(off))
	}
	on, _ := ToDDL(def, DDLOptions{Schema: "public", QuoteBarewordStringDefaults: true})
	out := joined(on)
	if !strings.Contains(out, `"status" varchar(255) DEFAULT 'draft'`) {
		t.Errorf("expected quoted bareword default:\n%s", out)
	}
	if !strings.Contains(out, `"note" text DEFAULT 'n''a'`) {
		t.Errorf("expected single-quote escaping:\n%s", out)
	}
}

// 5. unique index prefix idx_ (legacy/ops) vs uidx_ (kernel default).
func TestToDDL_LegacyUniqueIndexName(t *testing.T) {
	def := manifest.ModelDefinition{
		TableName: "users",
		Columns:   []manifest.ColumnDef{{Name: "email", Type: "string", Unique: true}},
	}
	off, _ := ToDDL(def, DDLOptions{Schema: "public"})
	if !strings.Contains(joined(off), `"uidx_users_email"`) {
		t.Errorf("default must use uidx_ prefix:\n%s", joined(off))
	}
	on, _ := ToDDL(def, DDLOptions{Schema: "public", LegacyUniqueIndexName: true})
	out := joined(on)
	if !strings.Contains(out, `CREATE UNIQUE INDEX IF NOT EXISTS "idx_users_email"`) {
		t.Errorf("expected legacy idx_ unique prefix:\n%s", out)
	}
	if strings.Contains(out, "uidx_") {
		t.Errorf("must not leak uidx_ under LegacyUniqueIndexName:\n%s", out)
	}
}

// Full ops-compat preset over a representative model exercising all 5 quirks.
func TestToDDL_SingleSchemaPreset_OpsCompatShape(t *testing.T) {
	def := manifest.ModelDefinition{
		TableName: "work_orders",
		Columns: []manifest.ColumnDef{
			{Name: "customer_id", Type: "uuid"}, // optional uuid → nullable
			{Name: "total", Type: "float"},
			{Name: "is_paid", Type: "bool"},
			{Name: "attributes", Type: "jsonb"},
			{Name: "status", Type: "string", Default: "draft"},
			{Name: "code", Type: "string", Unique: true},
		},
	}
	stmts, err := ToDDL(def, SingleSchemaDDLOptions("public"))
	if err != nil {
		t.Fatalf("ToDDL: %v", err)
	}
	out := joined(stmts)
	checks := []string{
		`"customer_id" uuid`,
		`"total" double precision`,
		`"is_paid" boolean DEFAULT false`,
		`"attributes" jsonb DEFAULT '{}'`,
		`"status" varchar(255) DEFAULT 'draft'`,
		`CREATE UNIQUE INDEX IF NOT EXISTS "idx_work_orders_code"`,
	}
	for _, want := range checks {
		if !strings.Contains(out, want) {
			t.Errorf("ops-compat preset missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "uidx_") {
		t.Errorf("ops-compat preset leaked uidx_:\n%s", out)
	}
	if strings.Contains(out, `"customer_id" uuid NOT NULL`) {
		t.Errorf("optional uuid must stay nullable:\n%s", out)
	}
	if strings.Contains(out, "numeric(18,4)") {
		t.Errorf("float should not remain numeric under preset:\n%s", out)
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
