package dynamic

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/asteby/metacore-kernel/manifest"
	"github.com/asteby/metacore-kernel/metadata"
	"github.com/asteby/metacore-kernel/modelbase"
	"github.com/asteby/metacore-kernel/query"
)

// tireSpecExt is products_tires' TireSpec extension of test_products as the
// host would resolve it (CONTRACT-item-master.md §3.1). SQLite has no generated
// columns here, so the size_key column is filled by the test like Postgres would.
func tireSpecExt() ExtensionTable {
	return ExtensionTable{
		Key:   "TireSpec",
		Table: "product_tire_specs",
		Columns: []manifest.ColumnDef{
			{Name: "section_width_mm", Type: "integer", Validation: &manifest.ValidationRule{Min: ptrF(125), Max: ptrF(355)}},
			{Name: "aspect_ratio", Type: "integer"},
			{Name: "rim_diameter_in", Type: "numeric"},
			{Name: "speed_rating", Type: "text", Options: []manifest.Option{{Value: "H"}, {Value: "V"}}},
			{Name: "size_key", Type: "text", Readonly: true, SearchKey: &manifest.SearchKeyDef{Parts: []manifest.SearchKeyPart{
				{Column: "section_width_mm", Type: "integer"}, {Literal: "/"},
				{Column: "aspect_ratio", Type: "integer"}, {Literal: "R"},
				{Column: "rim_diameter_in", Type: "numeric"},
			}}},
		},
	}
}

func ptrF(f float64) *float64 { return &f }

func setupExtensionService(t *testing.T) (*Service, *gorm.DB) {
	t.Helper()
	db := setupTestDB(t)
	if err := db.Exec(`CREATE TABLE product_tire_specs (
		id TEXT PRIMARY KEY,
		organization_id TEXT NOT NULL,
		section_width_mm INTEGER,
		aspect_ratio INTEGER,
		rim_diameter_in REAL,
		speed_rating TEXT,
		size_key TEXT,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
	)`).Error; err != nil {
		t.Fatal(err)
	}
	modelbase.Register("test_products", func() modelbase.ModelDefiner { return &TestProduct{} })
	svc := New(Config{
		DB:       db,
		Metadata: metadata.New(metadata.Config{CacheTTL: -1}),
		// SQLite has no ILIKE.
		SearchMatchClause: func(col, q string) (string, any) { return col + " LIKE ?", "%" + q + "%" },
		ExtensionResolver: func(_ context.Context, model string) []ExtensionTable {
			if model == "test_products" {
				return []ExtensionTable{tireSpecExt()}
			}
			return nil
		},
	})
	return svc, db
}

func TestExtensions_CreateGetUpdate(t *testing.T) {
	svc, db := setupExtensionService(t)
	ctx := context.Background()
	user := newUser(uuid.New())

	out, err := svc.Create(ctx, "test_products", user, map[string]any{
		"name":                      "LLANTA 205/55R16",
		"price":                     1200,
		"TireSpec.section_width_mm": "205",
		"TireSpec.aspect_ratio":     "55",
		"TireSpec.rim_diameter_in":  "16",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if out["TireSpec.section_width_mm"] == nil || out["name"] != "LLANTA 205/55R16" {
		t.Fatalf("create result lacks the extension: %v", out)
	}
	id, _ := uuid.Parse(out["id"].(string))

	var org string
	db.Raw(`SELECT organization_id FROM product_tire_specs WHERE id = ?`, id.String()).Scan(&org)
	if org != user.orgID.String() {
		t.Fatalf("extension row org = %q", org)
	}

	// PATCH one column: the others survive.
	if _, err := svc.Update(ctx, "test_products", user, id, map[string]any{"TireSpec": map[string]any{"speed_rating": "V"}}); err != nil {
		t.Fatalf("update: %v", err)
	}
	got, err := svc.Get(ctx, "test_products", user, id)
	if err != nil {
		t.Fatal(err)
	}
	if got["TireSpec.speed_rating"] != "V" || toFloat(got["TireSpec.section_width_mm"]) != 205 {
		t.Fatalf("get = %v", got)
	}
}

func TestExtensions_ValidationIsPrefixed(t *testing.T) {
	svc, _ := setupExtensionService(t)
	_, err := svc.Create(context.Background(), "test_products", newUser(uuid.New()), map[string]any{
		"name":                      "X",
		"TireSpec.section_width_mm": "999",
		"TireSpec.speed_rating":     "Z9",
		"TireSpec.size_key":         "hack",
		"TireSpec.bogus":            "1",
	})
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("want ValidationError, got %v", err)
	}
	for _, f := range []string{"TireSpec.section_width_mm", "TireSpec.speed_rating", "TireSpec.size_key", "TireSpec.bogus"} {
		if len(ve.Fields[f]) == 0 {
			t.Fatalf("missing issue on %s: %+v", f, ve.Fields)
		}
	}
}

func TestExtensions_ListFilterAndSearchKey(t *testing.T) {
	svc, db := setupExtensionService(t)
	ctx := context.Background()
	user := newUser(uuid.New())
	mk := func(name string, w, a int, r float64) string {
		out, err := svc.Create(ctx, "test_products", user, map[string]any{
			"name": name, "TireSpec.section_width_mm": w, "TireSpec.aspect_ratio": a, "TireSpec.rim_diameter_in": r,
		})
		if err != nil {
			t.Fatal(err)
		}
		id := out["id"].(string)
		// What the Postgres generated column stores.
		db.Exec(`UPDATE product_tire_specs SET size_key = ? WHERE id = ?`,
			NormalizeSearchKey(tireSpecExt().Columns[4].SearchKey, formatSize(w, a, r)), id)
		return id
	}
	a := mk("Everland A", 205, 55, 16)
	mk("Everland B", 185, 60, 15)
	svc.Create(ctx, "test_products", user, map[string]any{"name": "Cubeta sin ficha"})

	items, _, err := svc.List(ctx, "test_products", user, query.Params{
		RelationFilters: []query.RelationFilter{{Relation: "TireSpec", Field: "rim_diameter_in", Op: query.OpEq, Value: "16"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0]["id"] != a {
		t.Fatalf("filter by extension: %v", items)
	}

	for _, term := range []string{"205/55R16", "205 55 16", "2055516"} {
		items, _, err := svc.List(ctx, "test_products", user, query.Params{Search: term})
		if err != nil {
			t.Fatal(err)
		}
		if len(items) != 1 || items[0]["id"] != a {
			t.Fatalf("search %q: %v", term, items)
		}
	}

	// A product without an extension row still lists, with nil extension fields.
	items, _, _ = svc.List(ctx, "test_products", user, query.Params{Search: "Cubeta"})
	if len(items) != 1 {
		t.Fatalf("plain search: %v", items)
	}
	if v, ok := items[0]["TireSpec.section_width_mm"]; !ok || v != nil {
		t.Fatalf("missing extension must serve nil, got %v (present=%v)", v, ok)
	}
}

func formatSize(w, a int, r float64) string {
	return itoaTest(w) + "/" + itoaTest(a) + "R" + itoaTest(int(r))
}

func itoaTest(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// A host with its own owner write (ops' legacy update path) takes the
// extension input out, writes the owner, then writes the extensions.
func TestExtensions_HostWritePath(t *testing.T) {
	svc, _ := setupExtensionService(t)
	ctx := context.Background()
	user := newUser(uuid.New())
	owner, err := svc.Create(ctx, "test_products", user, map[string]any{"name": "sin ficha"})
	if err != nil {
		t.Fatal(err)
	}
	id, _ := uuid.Parse(owner["id"].(string))

	input := map[string]any{"name": "con ficha", "TireSpec.section_width_mm": "205"}
	if !svc.HasExtensionInput(ctx, "test_products", input) {
		t.Fatal("extension input not detected")
	}
	in, err := svc.TakeExtensionInput(ctx, "test_products", user, input, &id)
	if err != nil || in.Empty() {
		t.Fatalf("take: %v empty=%v", err, in.Empty())
	}
	if _, left := input["TireSpec.section_width_mm"]; left || input["name"] != "con ficha" {
		t.Fatalf("owner input after take: %v", input)
	}
	if err := svc.WriteExtensions(ctx, in, id, user.orgID); err != nil {
		t.Fatal(err)
	}
	rows := []map[string]any{{"id": id.String()}}
	if err := svc.ServeExtensions(ctx, "test_products", rows); err != nil {
		t.Fatal(err)
	}
	if toFloat(rows[0]["TireSpec.section_width_mm"]) != 205 {
		t.Fatalf("served = %v", rows[0])
	}

	bad := map[string]any{"TireSpec.section_width_mm": "999"}
	if _, err := svc.TakeExtensionInput(ctx, "test_products", user, bad, &id); err == nil {
		t.Fatal("out-of-range extension value must fail validation")
	}
}

// The product picker (/api/options) finds a tire by its size in any spelling:
// that is the search a cashier uses, not the list's.
func TestExtensions_OptionsSearchKey(t *testing.T) {
	svc, db := setupExtensionService(t)
	ctx := context.Background()
	user := newUser(uuid.New())
	out, err := svc.Create(ctx, "test_products", user, map[string]any{
		"name": "Everland A", "TireSpec.section_width_mm": 205, "TireSpec.aspect_ratio": 55, "TireSpec.rim_diameter_in": 16,
	})
	if err != nil {
		t.Fatal(err)
	}
	db.Exec(`UPDATE product_tire_specs SET size_key = '2055516' WHERE id = ?`, out["id"])
	if _, err := svc.Create(ctx, "test_products", user, map[string]any{"name": "Cubeta 205"}); err != nil {
		t.Fatal(err)
	}
	svc.optsResolver = optionsConfigFor(OptionsConfig{Fields: map[string]FieldOptionsConfig{
		"id": {Type: "dynamic", Source: "test_products", Value: "id", Label: "name"},
	}})

	for _, term := range []string{"205/55R16", "205 55 16"} {
		res, err := svc.Options(ctx, user, OptionsQuery{Model: "test_products", Field: "id", Q: term})
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Options) != 1 || res.Options[0].Label != "Everland A" {
			t.Fatalf("q=%q → %+v", term, res.Options)
		}
	}
	// The label search still works on its own.
	res, err := svc.Options(ctx, user, OptionsQuery{Model: "test_products", Field: "id", Q: "Cubeta"})
	if err != nil || len(res.Options) != 1 {
		t.Fatalf("label search: %+v %v", res, err)
	}
}

// Facets on "<Ext>.<col>" count the extension values of the caller's rows.
func TestExtensions_Facets(t *testing.T) {
	svc, _ := setupExtensionService(t)
	ctx := context.Background()
	user := newUser(uuid.New())
	other := newUser(uuid.New())
	for _, r := range []struct {
		u   *fakeUser
		rim int
	}{{user, 16}, {user, 16}, {user, 15}, {other, 16}} {
		if _, err := svc.Create(ctx, "test_products", r.u, map[string]any{"name": "x", "TireSpec.rim_diameter_in": r.rim}); err != nil {
			t.Fatal(err)
		}
	}
	buckets, err := svc.Facets(ctx, user, FacetsQuery{Model: "test_products", Field: "TireSpec.rim_diameter_in"})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]int64{}
	for _, b := range buckets {
		got[b.Value] = b.Count
	}
	if got["16"] != 2 || got["15"] != 1 || len(got) != 2 {
		t.Fatalf("buckets = %+v", buckets)
	}
	if _, err := svc.Facets(ctx, user, FacetsQuery{Model: "test_products", Field: "TireSpec.nope"}); err == nil {
		t.Fatal("undeclared extension column must not be a facet")
	}
}

// PIT-039: the edit form posts every field back, the read-only search key
// included. Echoing the persisted value must not fail the save; changing it must.
func TestExtensions_EchoedSearchKeyIsNoop(t *testing.T) {
	svc, db := setupExtensionService(t)
	ctx := context.Background()
	user := newUser(uuid.New())
	owner, err := svc.Create(ctx, "test_products", user, map[string]any{
		"name": "Llanta", "TireSpec.section_width_mm": 205, "TireSpec.aspect_ratio": 55, "TireSpec.rim_diameter_in": 16,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	id, _ := uuid.Parse(owner["id"].(string))
	db.Exec(`UPDATE product_tire_specs SET size_key = '2055516' WHERE id = ?`, id.String())

	if _, err := svc.Update(ctx, "test_products", user, id, map[string]any{
		"TireSpec.speed_rating": "V", "TireSpec.size_key": "2055516",
	}); err != nil {
		t.Fatalf("echoed size_key must be a no-op: %v", err)
	}
	_, err = svc.Update(ctx, "test_products", user, id, map[string]any{"TireSpec.size_key": "hack"})
	var ve *ValidationError
	if !errors.As(err, &ve) || len(ve.Fields["TireSpec.size_key"]) == 0 {
		t.Fatalf("changing size_key must be rejected, got %v", err)
	}
}

// match: normalized_prefix — the picker finds the tire while the size is still
// being typed ("205/55" → "205/55R16"), the default key still needs it whole.
func TestExtensions_OptionsSearchKeyPrefix(t *testing.T) {
	svc, db := setupExtensionService(t)
	ctx := context.Background()
	user := newUser(uuid.New())
	out, err := svc.Create(ctx, "test_products", user, map[string]any{
		"name": "Everland A", "TireSpec.section_width_mm": 205, "TireSpec.aspect_ratio": 55, "TireSpec.rim_diameter_in": 16,
	})
	if err != nil {
		t.Fatal(err)
	}
	db.Exec(`UPDATE product_tire_specs SET size_key = '2055516' WHERE id = ?`, out["id"])
	svc.optsResolver = optionsConfigFor(OptionsConfig{Fields: map[string]FieldOptionsConfig{
		"id": {Type: "dynamic", Source: "test_products", Value: "id", Label: "name"},
	}})
	countFor := func(term string) int {
		res, err := svc.Options(ctx, user, OptionsQuery{Model: "test_products", Field: "id", Q: term})
		if err != nil {
			t.Fatal(err)
		}
		return len(res.Options)
	}
	if n := countFor("205/55"); n != 0 {
		t.Fatalf("equality key must not match a partial size, got %d", n)
	}
	svc.extensions = func(_ context.Context, model string) []ExtensionTable {
		e := tireSpecExt()
		e.Columns[4].SearchKey.Match = "normalized_prefix"
		return []ExtensionTable{e}
	}
	for _, term := range []string{"205/55", "205 55 1", "2055516", "20"} {
		if n := countFor(term); n != 1 {
			t.Fatalf("prefix q=%q → %d options, want 1", term, n)
		}
	}
	if n := countFor("225/55"); n != 0 {
		t.Fatalf("a different size must not match, got %d", n)
	}
}
