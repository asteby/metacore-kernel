package query

import (
	"strings"
	"testing"

	"github.com/asteby/metacore-kernel/modelbase"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// QA-0922 UI-N12: the list search of work orders / sales orders only matched
// `notes`, so an order could not be found by its customer or its vehicle. A
// RefSearch lets the free-text search reach through a FK into the referenced
// record's own search columns.

func refMeta() *modelbase.TableMetadata {
	return &modelbase.TableMetadata{
		Columns: []modelbase.ColumnDef{
			{Key: "id", Type: "text"},
			{Key: "number", Type: "text"},
			{Key: "customer_id", Type: "text", Ref: "customers.Customer"},
		},
		SearchColumns: []string{"number"},
		SearchRefs:    []string{"customer_id"},
	}
}

func TestApply_RefSearchReachesReferencedRecord(t *testing.T) {
	b := New(refMeta()).WithTableName("orders").
		WithRefSearch(map[string]RefSearch{"customer_id": {Table: "customers", Columns: []string{"name", "tax_id"}}})
	sql := renderSQL(t, b.Apply(openDryDB(t).Model(&testRow{}), Params{Search: "acme"}))
	for _, want := range []string{
		"number ILIKE",
		"orders.customer_id IN (SELECT __rs.id FROM customers __rs WHERE __rs.name ILIKE",
		"OR __rs.tax_id ILIKE",
	} {
		if !strings.Contains(sql, want) {
			t.Errorf("want %q in %q", want, sql)
		}
	}
}

func TestApply_RefSearchUsesHostClause(t *testing.T) {
	b := New(refMeta(), WithSearchClause(func(col, q string) (string, any) {
		return "unaccent(" + col + ") ILIKE unaccent(?)", "%" + q + "%"
	})).WithTableName("orders").
		WithRefSearch(map[string]RefSearch{"customer_id": {Table: "customers", Columns: []string{"name"}}})
	sql := renderSQL(t, b.Apply(openDryDB(t).Model(&testRow{}), Params{Search: "piñata"}))
	if !strings.Contains(sql, "unaccent(__rs.name) ILIKE unaccent(") {
		t.Errorf("host clause not applied to the referenced column: %q", sql)
	}
}

func TestApply_RefSearchGuards(t *testing.T) {
	t.Run("unsafe identifiers and undeclared fks are dropped", func(t *testing.T) {
		b := New(refMeta()).WithTableName("orders").WithRefSearch(map[string]RefSearch{
			"customer_id":  {Table: "customers; DROP TABLE x", Columns: []string{"name"}},
			"not_a_column": {Table: "customers", Columns: []string{"name"}},
		})
		if len(b.refOrder) != 0 {
			t.Fatalf("want no ref search, got %v", b.refOrder)
		}
	})
	t.Run("no table name → relation search skipped, plain search kept", func(t *testing.T) {
		b := New(refMeta()).WithRefSearch(map[string]RefSearch{"customer_id": {Table: "customers", Columns: []string{"name"}}})
		sql := renderSQL(t, b.Apply(openDryDB(t).Model(&testRow{}), Params{Search: "acme"}))
		if strings.Contains(sql, "__rs") || !strings.Contains(sql, "number ILIKE") {
			t.Errorf("unexpected SQL: %q", sql)
		}
	})
	t.Run("no term → no clause", func(t *testing.T) {
		b := New(refMeta()).WithTableName("orders").
			WithRefSearch(map[string]RefSearch{"customer_id": {Table: "customers", Columns: []string{"name"}}})
		sql := renderSQL(t, b.Apply(openDryDB(t).Model(&testRow{}), Params{}))
		if strings.Contains(sql, "__rs") {
			t.Errorf("unexpected relation clause: %q", sql)
		}
	})
	t.Run("only ref search (no plain search columns) still filters", func(t *testing.T) {
		m := refMeta()
		m.SearchColumns = nil
		b := New(m).WithTableName("orders").
			WithRefSearch(map[string]RefSearch{"customer_id": {Table: "customers", Columns: []string{"name"}}})
		sql := renderSQL(t, b.Apply(openDryDB(t).Model(&testRow{}), Params{Search: "acme"}))
		if !strings.Contains(sql, "orders.customer_id IN (SELECT") {
			t.Errorf("want ref clause, got %q", sql)
		}
	})
}

// Live: the relation search returns exactly the orders whose customer matches.
func TestApply_RefSearchLive(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`CREATE TABLE customers (id TEXT PRIMARY KEY, name TEXT)`,
		`CREATE TABLE orders (id TEXT PRIMARY KEY, number TEXT, customer_id TEXT, created_at TEXT)`,
		`INSERT INTO customers VALUES ('c1','Escuela Kemper'),('c2','Taller Norte')`,
		`INSERT INTO orders VALUES ('o1','SO-1','c1',''),('o2','SO-2','c2',''),('o3','SO-KEMP','c2','')`,
	} {
		if err := db.Exec(stmt).Error; err != nil {
			t.Fatal(err)
		}
	}
	like := WithSearchClause(func(col, q string) (string, any) { return col + " LIKE ?", "%" + q + "%" })
	b := New(refMeta(), like).WithTableName("orders").
		WithRefSearch(map[string]RefSearch{"customer_id": {Table: "customers", Columns: []string{"name"}}})
	var ids []string
	if err := b.Apply(db.Table("orders"), Params{Search: "Kemp"}).Pluck("orders.id", &ids).Error; err != nil {
		t.Fatal(err)
	}
	got := strings.Join(ids, ",")
	if !strings.Contains(got, "o1") || !strings.Contains(got, "o3") || strings.Contains(got, "o2") {
		t.Fatalf("want o1 (customer) and o3 (number), got %v", ids)
	}
}
