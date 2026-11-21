package dynamic

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func marketDryDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	return db
}

func TestApplyMarketPredicateSQL(t *testing.T) {
	db := marketDryDB(t)
	got := ApplyMarketPredicate(db.Table("products"), "products", "us").Find(&[]map[string]any{}).Statement.SQL.String()
	for _, want := range []string{`"products"."markets" IS NULL`, `'[]'::jsonb`, `jsonb_exists("products"."markets", ?)`} {
		if !strings.Contains(got, want) {
			t.Fatalf("falta %q en %s", want, got)
		}
	}
	got = ApplyMarketPredicate(db.Table("s.products"), "s.products", "us").Find(&[]map[string]any{}).Statement.SQL.String()
	if !strings.Contains(got, `"s"."products"."markets"`) {
		t.Fatalf("tabla con esquema: %s", got)
	}
}

// Sin mercado, o sin columna markets jsonb (sqlite no tiene information_schema → ""), la consulta no se toca.
func TestApplyMarketScopeNoop(t *testing.T) {
	db := marketDryDB(t)
	base := db.Table("products")
	if ApplyMarketScope(base, "products", "") != base {
		t.Fatal("sin mercado: no-op")
	}
	if ApplyMarketScope(base, "products", "us") != base {
		t.Fatal("sin columna jsonb: no-op")
	}
}

func TestMarketScoperComposes(t *testing.T) {
	db := marketDryDB(t)
	org := uuid.New()
	u := &fakeUser{orgID: org}
	s := MarketScoper{} // por defecto envuelve OrganizationScoper
	q := db.WithContext(WithMarket(context.Background(), " US ")).Table("products")
	sql := s.ScopeQuery(q, u).Find(&[]map[string]any{}).Statement.SQL.String()
	if !strings.Contains(sql, "organization_id") {
		t.Fatalf("conserva el alcance de organización: %s", sql)
	}
	in := map[string]any{}
	s.InjectOnCreate(in, u)
	if in["organization_id"] != org {
		t.Fatalf("InjectOnCreate delega: %v", in)
	}
}
