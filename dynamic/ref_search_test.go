package dynamic

import (
	"context"
	"fmt"
	"testing"

	"github.com/google/uuid"

	"github.com/asteby/metacore-kernel/metadata"
	"github.com/asteby/metacore-kernel/modelbase"
	"github.com/asteby/metacore-kernel/query"
)

// QA-0922 UI-N12: a list search must find an order by its customer's name
// (SearchRefs → the referenced model's own search columns), not only by notes.

type TestRefCustomer struct {
	modelbase.BaseUUIDModel
	Name string `json:"name"`
}

func (TestRefCustomer) TableName() string { return "test_ref_customers" }
func (TestRefCustomer) DefineTable() modelbase.TableMetadata {
	return modelbase.TableMetadata{
		Columns:       []modelbase.ColumnDef{{Key: "name", Label: "Name"}},
		SearchColumns: []string{"name"},
	}
}
func (TestRefCustomer) DefineModal() modelbase.ModalMetadata { return modelbase.ModalMetadata{} }

type TestRefOrder struct {
	modelbase.BaseUUIDModel
	Number     string `json:"number"`
	CustomerID string `json:"customer_id"`
}

func (TestRefOrder) TableName() string { return "test_ref_orders" }
func (TestRefOrder) DefineTable() modelbase.TableMetadata {
	return modelbase.TableMetadata{
		Columns: []modelbase.ColumnDef{
			{Key: "number", Label: "Number"},
			// Addon-qualified ref, as manifests declare it.
			{Key: "customer_id", Label: "Customer", Ref: "refaddon.test_ref_customers"},
		},
		SearchColumns: []string{"number"},
		SearchRefs:    []string{"customer_id"},
	}
}
func (TestRefOrder) DefineModal() modelbase.ModalMetadata { return modelbase.ModalMetadata{} }

func TestList_SearchReachesThroughSearchRefs(t *testing.T) {
	db := setupTestDB(t)
	for _, stmt := range []string{
		`CREATE TABLE test_ref_customers (id TEXT PRIMARY KEY, organization_id TEXT, created_by_id TEXT, created_at DATETIME, updated_at DATETIME, deleted_at DATETIME, name TEXT)`,
		`CREATE TABLE test_ref_orders (id TEXT PRIMARY KEY, organization_id TEXT, created_by_id TEXT, created_at DATETIME, updated_at DATETIME, deleted_at DATETIME, number TEXT, customer_id TEXT)`,
	} {
		if err := db.Exec(stmt).Error; err != nil {
			t.Fatal(err)
		}
	}
	modelbase.Register("test_ref_customers", func() modelbase.ModelDefiner { return &TestRefCustomer{} })
	modelbase.Register("test_ref_orders", func() modelbase.ModelDefiner { return &TestRefOrder{} })
	svc := New(Config{
		DB:       db,
		Metadata: metadata.New(metadata.Config{CacheTTL: -1}),
		SearchMatchClause: func(col, q string) (string, any) {
			return fmt.Sprintf("%s LIKE ?", col), "%" + q + "%"
		},
	})
	org := uuid.New()
	user := newUser(org)
	c1, c2 := uuid.NewString(), uuid.NewString()
	for _, stmt := range []string{
		fmt.Sprintf(`INSERT INTO test_ref_customers (id, organization_id, name) VALUES ('%s','%s','Escuela Kemper'),('%s','%s','Taller Norte')`, c1, org, c2, org),
		fmt.Sprintf(`INSERT INTO test_ref_orders (id, organization_id, number, customer_id, created_at) VALUES ('%s','%s','SO-1','%s','2026-01-01'),('%s','%s','SO-2','%s','2026-01-02')`, uuid.NewString(), org, c1, uuid.NewString(), org, c2),
	} {
		if err := db.Exec(stmt).Error; err != nil {
			t.Fatal(err)
		}
	}

	rows, _, err := svc.List(context.Background(), "test_ref_orders", user, query.Params{Search: "Kemper", Page: 1, PerPage: 10})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 1 || rows[0]["number"] != "SO-1" {
		t.Fatalf("search by customer name: want only SO-1, got %v", rows)
	}

	// The plain search columns keep working alongside.
	rows, _, err = svc.List(context.Background(), "test_ref_orders", user, query.Params{Search: "SO-2", Page: 1, PerPage: 10})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 1 || rows[0]["number"] != "SO-2" {
		t.Fatalf("search by number: want only SO-2, got %v", rows)
	}
}
