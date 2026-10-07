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
