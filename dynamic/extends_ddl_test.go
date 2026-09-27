package dynamic

import (
	"fmt"
	"strings"
	"testing"

	"github.com/asteby/metacore-kernel/manifest"
	"github.com/google/uuid"
)

// tireSpecDef is products_tires' TireSpec as the v3 conversion lowers it:
// extends products.Product, one searchable column and a size search key.
func tireSpecDef() manifest.ModelDefinition {
	return manifest.ModelDefinition{
		TableName: "product_tire_specs",
		ModelKey:  "TireSpec",
		Extends:   "products.Product",
		OrgScoped: true,
		Columns: []manifest.ColumnDef{
			{Name: "section_width_mm", Type: "integer", Index: true},
			{Name: "aspect_ratio", Type: "integer"},
			{Name: "rim_diameter_in", Type: "numeric"},
			{Name: "size_key", Type: "text", Index: true, Readonly: true, SearchKey: &manifest.SearchKeyDef{Parts: []manifest.SearchKeyPart{
				{Column: "section_width_mm", Type: "integer"}, {Literal: "/"},
				{Column: "aspect_ratio", Type: "integer"}, {Literal: "R"},
				{Column: "rim_diameter_in", Type: "numeric"},
			}}},
		},
	}
}

func productsTarget(accepts bool) ModelTargetResolver {
	return func(ref string) (ModelTarget, bool) {
		if ref != "products.Product" {
			return ModelTarget{}, false
		}
		return ModelTarget{Schema: "public", Table: "products", AcceptsExtensions: accepts}, true
	}
}

func TestToDDL_ExtendsTable(t *testing.T) {
	stmts, err := ToDDL(tireSpecDef(), DDLOptions{Schema: "addon_products_tires", Isolation: IsolationShared, ResolveModelTarget: productsTarget(true)})
	if err != nil {
		t.Fatal(err)
	}
	all := strings.Join(stmts, "\n")
	for _, want := range []string{
		`"id" uuid PRIMARY KEY,`,
		`"organization_id" uuid NOT NULL`,
		`"size_key" text GENERATED ALWAYS AS (upper(regexp_replace("section_width_mm"::text || "aspect_ratio"::text || trim_scale("rim_diameter_in")::text, '[^A-Za-z0-9]', '', 'g'))) STORED`,
		`CREATE INDEX IF NOT EXISTS "idx_product_tire_specs_size_key"`,
		`CREATE INDEX IF NOT EXISTS "idx_product_tire_specs_section_width_mm"`,
		`ADD CONSTRAINT "fk_product_tire_specs_extends" FOREIGN KEY ("id") REFERENCES "public"."products" ("id") ON DELETE CASCADE`,
	} {
		if !strings.Contains(all, want) {
			t.Fatalf("missing %q in:\n%s", want, all)
		}
	}
	if strings.Contains(all, "gen_random_uuid") {
		t.Fatalf("an extension id must not default to a fresh uuid:\n%s", all)
	}
}

func TestToDDL_ExtendsTargetNotAccepting(t *testing.T) {
	_, err := ToDDL(tireSpecDef(), DDLOptions{Schema: "s", ResolveModelTarget: productsTarget(false)})
	if err == nil || !strings.Contains(err.Error(), "model_extensions_accepted") {
		t.Fatalf("err = %v", err)
	}
}

func TestToDDL_ExtendsUnresolvedTargetHasNoFK(t *testing.T) {
	for _, opts := range []DDLOptions{{Schema: "s"}, {Schema: "s", ResolveModelTarget: func(string) (ModelTarget, bool) { return ModelTarget{}, false }}} {
		stmts, err := ToDDL(tireSpecDef(), opts)
		if err != nil {
			t.Fatal(err)
		}
		if all := strings.Join(stmts, "\n"); strings.Contains(all, "FOREIGN KEY") || !strings.Contains(all, `"id" uuid PRIMARY KEY,`) {
			t.Fatalf("unexpected DDL:\n%s", all)
		}
	}
}

func TestSearchKeySQL_Exact(t *testing.T) {
	got, err := searchKeySQL(&manifest.SearchKeyDef{Match: "exact", Parts: []manifest.SearchKeyPart{
		{Column: "a", Type: "integer"}, {Literal: "x'y"}, {Column: "b", Type: "text"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if got != `("a"::text || 'x''y' || "b"::text)` {
		t.Fatalf("got %s", got)
	}
	if _, err := searchKeySQL(&manifest.SearchKeyDef{Parts: []manifest.SearchKeyPart{{Literal: "R"}}}); err == nil {
		t.Fatal("a key without columns must fail")
	}
}

func TestNormalizeSearchKey(t *testing.T) {
	sk := tireSpecDef().Columns[3].SearchKey
	for _, in := range []string{"205/55R16", "205/55r16", "2055516", "205 55 16", " 205-55-R-16 "} {
		if got := NormalizeSearchKey(sk, in); got != "2055516" {
			t.Fatalf("%q → %q, want 2055516", in, got)
		}
	}
	// A letter inside a value (not between digits) survives.
	if got := NormalizeSearchKey(sk, "LT245/75R16"); got != "LT2457516" {
		t.Fatalf("got %q", got)
	}
}

// TestPostgresExtendsTable runs the real DDL: the generated key, the FK and
// the cascade. CI job "postgres" sets TEST_POSTGRES_DSN.
func TestPostgresExtendsTable(t *testing.T) {
	db, sfx := pgTestDB(t)
	base := "base_" + sfx
	addon := "ext_" + sfx
	schema := SchemaName(addon, uuid.Nil, IsolationShared)
	mustExec(t, db, fmt.Sprintf(`CREATE TABLE public.%q (id uuid PRIMARY KEY)`, base))
	t.Cleanup(func() {
		_ = db.Exec(fmt.Sprintf(`DROP SCHEMA IF EXISTS %q CASCADE`, schema)).Error
		_ = db.Exec(fmt.Sprintf(`DROP TABLE IF EXISTS public.%q`, base)).Error
	})

	def := tireSpecDef()
	def.TableName = "specs_" + sfx
	resolve := func(string) (ModelTarget, bool) {
		return ModelTarget{Schema: "public", Table: base, AcceptsExtensions: true}, true
	}
	if err := EnsureSchema(db, addon, uuid.Nil, IsolationShared); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ { // idempotent
		if err := CreateTableWithOptions(db, addon, uuid.Nil, IsolationShared, def, TableOptions{ResolveModelTarget: resolve}); err != nil {
			t.Fatalf("create #%d: %v", i+1, err)
		}
	}

	id, org := uuid.New(), uuid.New()
	mustExec(t, db, fmt.Sprintf(`INSERT INTO public.%q (id) VALUES ('%s')`, base, id))
	// RLS is enabled; the table owner bypasses it, so the insert is direct.
	mustExec(t, db, fmt.Sprintf(`INSERT INTO %q.%q (id, organization_id, section_width_mm, aspect_ratio, rim_diameter_in) VALUES ('%s', '%s', 205, 55, 16.0)`,
		schema, def.TableName, id, org))
	var key string
	if err := db.Raw(fmt.Sprintf(`SELECT size_key FROM %q.%q WHERE id = ?`, schema, def.TableName), id).Scan(&key).Error; err != nil {
		t.Fatal(err)
	}
	if key != "2055516" {
		t.Fatalf("size_key = %q", key)
	}

	if err := db.Exec(fmt.Sprintf(`INSERT INTO %q.%q (id, organization_id) VALUES ('%s', '%s')`, schema, def.TableName, uuid.New(), org)).Error; err == nil {
		t.Fatal("an extension row without its target row must be rejected by the FK")
	}

	mustExec(t, db, fmt.Sprintf(`DELETE FROM public.%q WHERE id = '%s'`, base, id))
	var n int64
	if err := db.Raw(fmt.Sprintf(`SELECT count(*) FROM %q.%q`, schema, def.TableName)).Scan(&n).Error; err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("delete of the target must cascade, %d rows left", n)
	}
}
