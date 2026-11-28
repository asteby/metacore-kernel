package dynamic

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/google/uuid"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

func TestMarketAccessContext(t *testing.T) {
	ctx := WithMarket(context.Background(), "us")
	if _, ok := MarketAccessFromContext(ctx); ok {
		t.Fatal("legacy operational market should not set an explicit access set")
	}
	input := []string{" MX ", "us", "mx", ""}
	ctx = WithMarketAccess(ctx, input)
	input[0] = "ca"
	markets, ok := MarketAccessFromContext(ctx)
	if !ok || !slices.Equal(markets, []string{"mx", "us"}) {
		t.Fatalf("authorized markets = %v, present = %t", markets, ok)
	}
	markets[0] = "ca"
	other, _ := MarketAccessFromContext(ctx)
	if other[0] != "mx" || MarketFromContext(ctx) != "us" {
		t.Fatal("the explicit set should remain immutable and independent of the operational market")
	}
	if empty, ok := MarketAccessFromContext(WithMarketAccess(ctx, nil)); !ok || len(empty) != 0 {
		t.Fatalf("explicit empty scope = %v, present = %t", empty, ok)
	}
}

func TestMarketAccessScopeQuery(t *testing.T) {
	for _, tc := range []struct {
		name    string
		markets []string
		sql     string
	}{
		{"one market", []string{"mx"}, `jsonb_exists\("products"\."markets", \$1\)`},
		{"union of permitted markets", []string{"mx", "us"}, `jsonb_exists_any\("products"\."markets", ARRAY\[\$1, \$2\]::text\[\]\)`},
		{"empty permission set", nil, `WHERE 1 = 0`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, mock := marketAccessDB(t)
			mock.ExpectQuery("SELECT data_type FROM information_schema.columns").WithArgs("products", "markets").
				WillReturnRows(sqlmock.NewRows([]string{"data_type"}).AddRow("jsonb"))
			query := mock.ExpectQuery(tc.sql)
			switch len(tc.markets) {
			case 1:
				query.WithArgs("mx")
			case 2:
				query.WithArgs("mx", "us")
			case 0:
				query.WithoutArgs()
			}
			query.WillReturnRows(sqlmock.NewRows([]string{"id"}))

			ctx := WithMarketAccess(WithMarket(context.Background(), "legacy"), tc.markets)
			if err := ApplyMarketScopeFromContext(ctx, db.Table("products"), "products").Find(&[]map[string]any{}).Error; err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestMarketAccessFailsWhenSchemaLookupFails(t *testing.T) {
	db, mock := marketAccessDB(t)
	want := errors.New("schema unavailable")
	mock.ExpectQuery("SELECT data_type FROM information_schema.columns").WithArgs("products", "markets").WillReturnError(want)
	err := ApplyMarketAccessScope(db.Table("products"), "products", []string{"mx"}).Find(&[]map[string]any{}).Error
	if !errors.Is(err, want) {
		t.Fatalf("schema lookup error = %v", err)
	}
}

func marketAccessDB(t *testing.T) (*gorm.DB, sqlmock.Sqlmock) {
	t.Helper()
	conn, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Error(err)
		}
		_ = conn.Close()
	})
	db, err := gorm.Open(postgres.New(postgres.Config{Conn: conn}), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	return db, mock
}

func marketDryDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, _ := marketAccessDB(t)
	return db.Session(&gorm.Session{DryRun: true})
}

func TestApplyMarketPredicateSQL(t *testing.T) {
	db := marketDryDB(t)
	got := ApplyMarketPredicate(db.Table("products"), "products", "us").Find(&[]map[string]any{}).Statement.SQL.String()
	for _, want := range []string{`"products"."markets" IS NULL`, `'[]'::jsonb`, `jsonb_exists("products"."markets", $1)`} {
		if !strings.Contains(got, want) {
			t.Fatalf("falta %q en %s", want, got)
		}
	}
	got = ApplyMarketPredicate(db.Table("s.products"), "s.products", "us").Find(&[]map[string]any{}).Statement.SQL.String()
	if !strings.Contains(got, `"s"."products"."markets"`) {
		t.Fatalf("tabla con esquema: %s", got)
	}
}

// Sin mercado, o sin columna markets jsonb, la consulta no se toca.
func TestApplyMarketScopeNoop(t *testing.T) {
	db, mock := marketAccessDB(t)
	base := db.Table("products")
	if ApplyMarketScope(base, "products", "") != base {
		t.Fatal("sin mercado: no-op")
	}
	mock.ExpectQuery("SELECT data_type FROM information_schema.columns").WithArgs("products", "markets").
		WillReturnRows(sqlmock.NewRows([]string{"data_type"}))
	if ApplyMarketScope(base, "products", "us") != base {
		t.Fatal("sin columna jsonb: no-op")
	}
}

func TestMarketScoperComposes(t *testing.T) {
	db, mock := marketAccessDB(t)
	org := uuid.New()
	u := &fakeUser{orgID: org}
	s := MarketScoper{} // por defecto envuelve OrganizationScoper
	ctx := WithMarketAccess(WithMarket(context.Background(), "us"), []string{"mx"})
	q := db.WithContext(ctx).Table("products")
	mock.ExpectQuery("SELECT data_type FROM information_schema.columns").WithArgs("products", "markets").
		WillReturnRows(sqlmock.NewRows([]string{"data_type"}).AddRow("jsonb"))
	mock.ExpectQuery(`organization_id.*jsonb_exists\("products"\."markets", \$2\)`).WithArgs(org.String(), "mx").
		WillReturnRows(sqlmock.NewRows([]string{"id"}))
	if err := s.ScopeQuery(q, u).Find(&[]map[string]any{}).Error; err != nil {
		t.Fatal(err)
	}
	in := map[string]any{}
	s.InjectOnCreate(in, u)
	if in["organization_id"] != org {
		t.Fatalf("InjectOnCreate delega: %v", in)
	}
}
