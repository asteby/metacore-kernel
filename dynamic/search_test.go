package dynamic

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestSearchILIKE(t *testing.T) {
	db := setupTestDB(t)
	svc := newOptionsService(t, db, nil, searchConfigFor(SearchConfig{
		SearchIn: []string{"name"},
		Value:    "id",
		Label:    "name",
	}))
	user := newUser(uuid.New())

	createProduct(t, svc, user, "Red Widget", 1)
	createProduct(t, svc, user, "Blue Widget", 2)
	createProduct(t, svc, user, "Gadget", 3)

	hits, err := svc.Search(context.Background(), user, SearchQuery{
		Model: "test_products",
		Q:     "widget",
	})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(hits) != 2 {
		t.Fatalf("expected 2 widget hits, got %d", len(hits))
	}
	for _, h := range hits {
		label, _ := h.Label.(string)
		if !strings.Contains(strings.ToLower(label), "widget") {
			t.Errorf("label %q does not contain widget", label)
		}
	}
}

func TestSearchMatchClauseCalled(t *testing.T) {
	db := setupTestDB(t)

	// Capture the column the match clause sees so we can assert dialect
	// overrides (e.g. unaccent ILIKE for Postgres) get a chance to participate.
	var gotCol, gotQ string
	matcher := func(col, q string) (string, any) {
		gotCol, gotQ = col, q
		return col + " LIKE ?", "%" + q + "%"
	}

	svc := newOptionsService(t, db, nil, searchConfigFor(SearchConfig{
		SearchIn: []string{"name"},
		Value:    "id",
		Label:    "name",
	}))
	svc.matchClause = matcher

	user := newUser(uuid.New())
	createProduct(t, svc, user, "Hello", 1)

	hits, err := svc.Search(context.Background(), user, SearchQuery{
		Model: "test_products",
		Q:     "hello",
	})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if gotCol != "test_products.name" {
		t.Errorf("matcher got col %q, want test_products.name", gotCol)
	}
	if gotQ != "hello" {
		t.Errorf("matcher got q %q, want hello", gotQ)
	}
	if len(hits) != 1 {
		t.Fatalf("expected 1 hit, got %d", len(hits))
	}
}

func TestSearchEmptyQReturnsWindow(t *testing.T) {
	db := setupTestDB(t)
	svc := newOptionsService(t, db, nil, searchConfigFor(SearchConfig{
		SearchIn: []string{"name"},
		Value:    "id",
		Label:    "name",
	}))
	user := newUser(uuid.New())
	createProduct(t, svc, user, "A", 1)
	createProduct(t, svc, user, "B", 2)

	hits, err := svc.Search(context.Background(), user, SearchQuery{Model: "test_products"})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(hits) != 2 {
		t.Fatalf("expected both rows, got %d", len(hits))
	}
}

func TestSearchNoResolver(t *testing.T) {
	db := setupTestDB(t)
	svc := setupService(t, db)
	if _, err := svc.Search(context.Background(), nil, SearchQuery{Model: "test_products"}); err != ErrNoSearchConfig {
		t.Fatalf("want ErrNoSearchConfig, got %v", err)
	}
}

func TestBuildNestedJoins(t *testing.T) {
	alias, col, joins := buildNestedJoins("tickets", "patient.user.name")
	if col != "name" {
		t.Errorf("col = %q, want name", col)
	}
	if !strings.HasPrefix(alias, "search_user_") {
		t.Errorf("alias = %q", alias)
	}
	if len(joins) != 2 {
		t.Fatalf("joins = %d, want 2", len(joins))
	}
	if !strings.Contains(joins[0], "patient") || !strings.Contains(joins[1], "user") {
		t.Errorf("joins missing expected relations: %v", joins)
	}
}

// --- helpers ---------------------------------------------------------------

func searchConfigFor(cfg SearchConfig) SearchConfigResolver {
	return func(context.Context, string, any) (*SearchConfig, error) {
		return &cfg, nil
	}
}

// A nested search path must not match through a soft-deleted parent row: the
// LEFT JOIN has to carry the joined table's deleted_at IS NULL in its ON.
func TestNestedJoinExcludesSoftDeletedParent(t *testing.T) {
	db := setupTestDB(t)
	for _, ddl := range []string{
		`CREATE TABLE tickets (id INTEGER PRIMARY KEY, patient_id INTEGER)`,
		`CREATE TABLE patients (id INTEGER PRIMARY KEY, name TEXT, deleted_at DATETIME)`,
		`CREATE TABLE users (id INTEGER PRIMARY KEY, name TEXT)`,
		`INSERT INTO patients (id, name, deleted_at) VALUES (1, 'alive', NULL), (2, 'alive-ghost', '2024-01-01')`,
		`INSERT INTO tickets (id, patient_id) VALUES (10, 1), (20, 2)`,
	} {
		if err := db.Exec(ddl).Error; err != nil {
			t.Fatalf("ddl %q: %v", ddl, err)
		}
	}

	hasDeletedAt := func(table string) bool {
		return db.Migrator().HasColumn(table, "deleted_at")
	}
	alias, col, joins := buildNestedJoinsWith("tickets", "patient.name", hasDeletedAt)
	q := db.Table("tickets").Select("tickets.id")
	for _, j := range joins {
		q = q.Joins(j)
	}
	var ids []int
	if err := q.Where(alias+"."+col+" LIKE ?", "%alive%").Pluck("tickets.id", &ids).Error; err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(ids) != 1 || ids[0] != 10 {
		t.Fatalf("ticket ids = %v, want [10] (soft-deleted patient must not match)", ids)
	}

	// A joined table without deleted_at keeps the original ON clause.
	_, _, joins = buildNestedJoinsWith("patients", "user.name", hasDeletedAt)
	if len(joins) != 1 || strings.Contains(joins[0], "deleted_at") {
		t.Fatalf("join over table without deleted_at changed: %v", joins)
	}
}

func searchNames(hits []Option) []string {
	var out []string
	for _, h := range hits {
		s, _ := h.Label.(string)
		out = append(out, s)
	}
	return out
}

func TestSearchBaseWhereRestrictsAndBindsArgs(t *testing.T) {
	db := setupTestDB(t)
	svc := newOptionsService(t, db, nil, searchConfigFor(SearchConfig{
		SearchIn:  []string{"name"},
		Value:     "id",
		Label:     "name",
		BaseWhere: "test_products.price >= ?",
		BaseArgs:  []any{2.0},
	}))
	user := newUser(uuid.New())
	createProduct(t, svc, user, "Cheap", 1)
	createProduct(t, svc, user, "Mid", 2)
	createProduct(t, svc, user, "Dear", 3)

	hits, err := svc.Search(context.Background(), user, SearchQuery{Model: "test_products", Limit: 10})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(hits) != 2 {
		t.Fatalf("BaseWhere should leave 2 rows, got %v", searchNames(hits))
	}
	// BaseWhere must also hold when a text query is present.
	hits, err = svc.Search(context.Background(), user, SearchQuery{Model: "test_products", Q: "cheap"})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(hits) != 0 {
		t.Fatalf("BaseWhere bypassed by Q: %v", searchNames(hits))
	}
}

func TestSearchJoinsApplied(t *testing.T) {
	db := setupTestDB(t)
	if err := db.Exec(`CREATE TABLE vendors (id INTEGER PRIMARY KEY, name TEXT, deleted_at DATETIME)`).Error; err != nil {
		t.Fatal(err)
	}
	db.Exec(`INSERT INTO vendors (id, name) VALUES (1, 'acme')`)
	svc := newOptionsService(t, db, nil, searchConfigFor(SearchConfig{
		SearchIn:  []string{"name"},
		Value:     "id",
		Label:     "name",
		Joins:     []string{"JOIN vendors ON vendors.name = test_products.name"},
		BaseWhere: "vendors.deleted_at IS NULL",
	}))
	user := newUser(uuid.New())
	createProduct(t, svc, user, "acme", 1)
	createProduct(t, svc, user, "other", 2)
	hits, err := svc.Search(context.Background(), user, SearchQuery{Model: "test_products"})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if got := searchNames(hits); len(got) != 1 || got[0] != "acme" {
		t.Fatalf("join should keep only acme, got %v", got)
	}
}

func TestSearchAllowFilters(t *testing.T) {
	db := setupTestDB(t)
	svc := newOptionsService(t, db, nil, searchConfigFor(SearchConfig{
		SearchIn:     []string{"name"},
		Value:        "id",
		Label:        "name",
		AllowFilters: []string{"name", "bad name; DROP TABLE test_products"},
	}))
	user := newUser(uuid.New())
	createProduct(t, svc, user, "A", 1)
	createProduct(t, svc, user, "B", 2)
	ctx := context.Background()

	// Allowed column filters by equality.
	hits, err := svc.Search(ctx, user, SearchQuery{Model: "test_products", Filters: map[string]string{"name": "B"}})
	if err != nil || len(hits) != 1 || searchNames(hits)[0] != "B" {
		t.Fatalf("allowed filter: hits=%v err=%v", searchNames(hits), err)
	}
	// Column outside the list is ignored (all rows come back, no error).
	hits, err = svc.Search(ctx, user, SearchQuery{Model: "test_products", Filters: map[string]string{"price": "2"}})
	if err != nil || len(hits) != 2 {
		t.Fatalf("non-listed filter must be ignored: hits=%v err=%v", searchNames(hits), err)
	}
	// Malicious identifier: neither listed-but-unsafe nor client-only keys reach SQL.
	for _, k := range []string{"bad name; DROP TABLE test_products", "name; DROP TABLE test_products--", "1=1 OR name"} {
		hits, err = svc.Search(ctx, user, SearchQuery{Model: "test_products", Filters: map[string]string{k: "x"}})
		if err != nil || len(hits) != 2 {
			t.Fatalf("malicious key %q: hits=%v err=%v", k, searchNames(hits), err)
		}
	}
	// Quotes / injection in the VALUE are bound, not interpolated.
	for _, v := range []string{"B' OR '1'='1", "x'; DROP TABLE test_products;--"} {
		hits, err = svc.Search(ctx, user, SearchQuery{Model: "test_products", Filters: map[string]string{"name": v}})
		if err != nil || len(hits) != 0 {
			t.Fatalf("injection value %q: hits=%v err=%v", v, searchNames(hits), err)
		}
	}
	if hits, _ = svc.Search(ctx, user, SearchQuery{Model: "test_products"}); len(hits) != 2 {
		t.Fatalf("table damaged: %d rows", len(hits))
	}
}

func TestSearchExtraFieldsAndOrderLimit(t *testing.T) {
	db := setupTestDB(t)
	svc := newOptionsService(t, db, nil, searchConfigFor(SearchConfig{
		SearchIn:    []string{"name"},
		Value:       "id",
		Label:       "name",
		ExtraFields: []string{"price", "bad;col", "id"},
		OrderBy:     "price",
		OrderDir:    "desc",
	}))
	user := newUser(uuid.New())
	createProduct(t, svc, user, "A", 1)
	createProduct(t, svc, user, "B", 2)
	createProduct(t, svc, user, "C", 3)
	hits, err := svc.Search(context.Background(), user, SearchQuery{Model: "test_products", Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if got := searchNames(hits); len(got) != 2 || got[0] != "C" || got[1] != "B" {
		t.Fatalf("order/limit broken: %v", got)
	}
	if hits[0].Extra["price"] != 3.0 {
		t.Fatalf("extra price missing: %v", hits[0].Extra)
	}
	if _, ok := hits[0].Extra["bad;col"]; ok {
		t.Fatal("unsafe extra field leaked")
	}
	if _, ok := hits[0].Extra["id"]; ok {
		t.Fatal("reserved key must be skipped")
	}
}

func TestSearchOrderByNonPlainColumnIgnored(t *testing.T) {
	for _, ob := range []string{"vendors.name", "price; DROP TABLE test_products", "p.price"} {
		db := setupTestDB(t)
		svc := newOptionsService(t, db, nil, searchConfigFor(SearchConfig{
			SearchIn: []string{"name"},
			Value:    "id",
			Label:    "name",
			OrderBy:  ob,
		}))
		user := newUser(uuid.New())
		createProduct(t, svc, user, "A", 1)
		createProduct(t, svc, user, "B", 2)
		hits, err := svc.Search(context.Background(), user, SearchQuery{Model: "test_products", Limit: 10})
		if err != nil {
			t.Fatalf("OrderBy %q should be ignored, not fail: %v", ob, err)
		}
		if len(hits) != 2 {
			t.Fatalf("OrderBy %q: want 2 hits, got %v", ob, searchNames(hits))
		}
	}
}
