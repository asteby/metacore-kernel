package query

import (
	"strings"
	"testing"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func TestBindInList_SQLiteStaysIN(t *testing.T) {
	db := openDryDB(t)
	expr, arg := bindInList(db, "status", []string{"a", "b"}, false, false)
	if expr != "status IN ?" {
		t.Fatalf("expr = %q", expr)
	}
	if _, ok := arg.([]string); !ok {
		t.Fatalf("sqlite bind = %T", arg)
	}
}

func TestBindInList_PostgresShapeIgnoresLength(t *testing.T) {
	db := openPostgresDry(t)
	short, shortArg := bindInList(db, "product_id", []string{"a", "b"}, false, true)
	long, longArg := bindInList(db, "product_id", []string{"a", "b", "c", "d", "e", "f", "g", "h"}, false, true)
	if short != long {
		t.Fatalf("shape changed with length:\n%s\n%s", short, long)
	}
	if short != "product_id = ANY(?::uuid[])" {
		t.Fatalf("expr = %q", short)
	}
	if _, ok := shortArg.(pgArray); !ok {
		t.Fatalf("arg = %T", shortArg)
	}
	if _, ok := longArg.(pgArray); !ok {
		t.Fatalf("long arg = %T", longArg)
	}

	not, _ := bindInList(db, "status", []string{"x"}, true, false)
	if not != "status <> ALL(?::text[])" {
		t.Fatalf("not-in = %q", not)
	}
}

func TestPgArrayValue_Escapes(t *testing.T) {
	v, err := pgArray{`a"b`, `c\d`}.Value()
	if err != nil {
		t.Fatal(err)
	}
	got, _ := v.(string)
	if got != `{"a\"b","c\\d"}` {
		t.Fatalf("literal = %s", got)
	}
}

func TestApply_PostgresInIsOnePlaceholder(t *testing.T) {
	db := openPostgresDry(t)
	b := New(testMeta()).WithUUIDColumns(map[string]struct{}{"status": {}})
	q := b.Apply(db.Table("test_rows"), Params{
		Filters: map[string]Filter{"status": {Op: OpIn, Value: []string{"active", "archived"}}},
	})
	stmt := q.Find(&[]testRow{}).Statement
	sql := stmt.SQL.String()
	if !strings.Contains(sql, "status = ANY(?::uuid[])") && !strings.Contains(sql, "status = ANY($1::uuid[])") {
		t.Fatalf("sql = %s vars=%d", sql, len(stmt.Vars))
	}
	if len(stmt.Vars) != 1 {
		t.Fatalf("vars = %d, want 1 (the array)", len(stmt.Vars))
	}
}

func openPostgresDry(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(postgres.New(postgres.Config{
		DSN: "host=127.0.0.1 user=q dbname=q sslmode=disable",
	}), &gorm.Config{
		DryRun:               true,
		DisableAutomaticPing: true,
		Logger:               logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("open postgres dry: %v", err)
	}
	return db
}
