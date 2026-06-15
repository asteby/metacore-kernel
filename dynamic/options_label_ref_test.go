package dynamic

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/asteby/metacore-kernel/modelbase"
)

// stockRow simulates a scope/ledger table (à la a stock ledger): each row is
// scoped by warehouse_id and carries a computed quantity, but its identity is a
// FOREIGN id (product_id) — there is no human-readable name on this table. The
// label has to be resolved from the related product model. This is exactly the
// dependent-relational-options case the kernel must handle generically.
type stockRow struct {
	ID          uuid.UUID `json:"id" gorm:"type:uuid;primaryKey"`
	WarehouseID uuid.UUID `json:"warehouse_id" gorm:"type:uuid"`
	ProductID   uuid.UUID `json:"product_id" gorm:"type:uuid"`
	Quantity    int       `json:"quantity"`
}

func (stockRow) TableName() string { return "test_stock" }
func (stockRow) DefineTable() modelbase.TableMetadata {
	return modelbase.TableMetadata{Title: "Stock"}
}
func (stockRow) DefineModal() modelbase.ModalMetadata {
	return modelbase.ModalMetadata{Title: "Stock"}
}

func setupStockTable(t *testing.T, db *gorm.DB) {
	t.Helper()
	db.Exec(`CREATE TABLE IF NOT EXISTS test_stock (
		id TEXT PRIMARY KEY,
		warehouse_id TEXT,
		product_id TEXT,
		quantity INTEGER
	)`)
	modelbase.Register("test_stock", func() modelbase.ModelDefiner { return &stockRow{} })
}

// TestOptionsFilterByLabelRef is the contract's headline case: a dependent
// picker scoped by warehouse (FilterBy + FilterValue) whose candidates come from
// the stock ledger (Source) — value = the product foreign id, description = the
// available quantity (a column ON the source row), and label resolved from the
// RELATED product model (LabelRef) by id, in a single batch query (no N+1).
func TestOptionsFilterByLabelRef(t *testing.T) {
	db := setupTestDB(t)
	setupStockTable(t, db)
	// Reuse TestProduct as the LabelRef model: its name column is the label.
	modelbase.Register("test_products", func() modelbase.ModelDefiner { return &TestProduct{} })

	whA := uuid.New()
	whB := uuid.New()
	prodWidget := uuid.New()
	prodGadget := uuid.New()
	prodOffWarehouse := uuid.New()

	// Products (the LabelRef model) — the human names live here, NOT on stock.
	db.Exec(`INSERT INTO test_products (id, name, price) VALUES (?,?,?),(?,?,?),(?,?,?)`,
		prodWidget.String(), "Widget", 10,
		prodGadget.String(), "Gadget", 20,
		prodOffWarehouse.String(), "OffWarehouse", 30)

	// Stock rows: two in warehouse A (Widget, Gadget), one in warehouse B.
	db.Exec(`INSERT INTO test_stock (id, warehouse_id, product_id, quantity) VALUES (?,?,?,?),(?,?,?,?),(?,?,?,?)`,
		uuid.NewString(), whA.String(), prodWidget.String(), 12,
		uuid.NewString(), whA.String(), prodGadget.String(), 7,
		uuid.NewString(), whB.String(), prodOffWarehouse.String(), 99)

	svc := newOptionsService(t, db, optionsConfigFor(OptionsConfig{
		Fields: map[string]FieldOptionsConfig{
			"product_id": {
				Type:        "dynamic",
				Source:      "test_stock",     // scope + qty live here
				FilterBy:    "warehouse_id",   // scoped by the cascade filter_value
				Value:       "product_id",     // option value = product foreign id
				Description: "quantity",       // option.description = available qty
				LabelRef:    "test_products",  // label resolved from product by id
			},
		},
	}), nil)

	res, err := svc.Options(context.Background(), nil, OptionsQuery{
		Model:       "test_stock",
		Field:       "product_id",
		FilterValue: whA.String(),
	})
	if err != nil {
		t.Fatalf("options: %v", err)
	}
	if res.Type != "dynamic" {
		t.Fatalf("type = %q, want dynamic", res.Type)
	}
	// Only the two warehouse-A stock rows are in scope.
	if len(res.Options) != 2 {
		t.Fatalf("scope by warehouse failed: got %d options, want 2", len(res.Options))
	}

	byValue := map[string]Option{}
	for _, o := range res.Options {
		byValue[strOf(o.Value)] = o
	}

	widget, ok := byValue[prodWidget.String()]
	if !ok {
		t.Fatalf("Widget stock row missing from scoped options")
	}
	// value = the product FOREIGN id (from the stock row, not the product pk row).
	if strOf(widget.Value) != prodWidget.String() {
		t.Errorf("Widget option value = %v, want product foreign id %s", widget.Value, prodWidget)
	}
	// label = the RELATED product's name, resolved via LabelRef (batch).
	if strOf(widget.Label) != "Widget" || strOf(widget.Name) != "Widget" {
		t.Errorf("Widget label/name = %v/%v, want Widget (resolved from label_ref)", widget.Label, widget.Name)
	}
	// description = the quantity column ON the source (stock) row.
	if i, _ := widget.Description.(int); i != 12 {
		t.Errorf("Widget description (quantity) = %v, want 12", widget.Description)
	}

	gadget := byValue[prodGadget.String()]
	if strOf(gadget.Label) != "Gadget" {
		t.Errorf("Gadget label = %v, want Gadget", gadget.Label)
	}
	if i, _ := gadget.Description.(int); i != 7 {
		t.Errorf("Gadget description = %v, want 7", gadget.Description)
	}

	// The warehouse-B product must NOT appear under warehouse A's scope.
	if _, leaked := byValue[prodOffWarehouse.String()]; leaked {
		t.Errorf("warehouse scope leaked: OffWarehouse product appeared under warehouse A")
	}
}

// TestOptionsLabelRefSkippedWhenLabelPresent proves enrichment is a no-op when
// the source already projects a real label (label != value): we must not clobber
// a good label with a ref lookup, and we must not query the ref model needlessly.
func TestOptionsLabelRefSkippedWhenLabelPresent(t *testing.T) {
	db := setupTestDB(t)
	setupCategoriesTable(t, db)
	// Point LabelRef at a table that does NOT exist as a model: if enrichment
	// fired it would error. It must not fire because Label already projects.
	cat := uuid.New()
	db.Exec(`INSERT INTO test_categories (id, name, color) VALUES (?,?,?)`,
		cat.String(), "Hardware", "blue")

	svc := newOptionsService(t, db, optionsConfigFor(OptionsConfig{
		Fields: map[string]FieldOptionsConfig{
			"category_id": {
				Type:     "dynamic",
				Source:   "test_categories",
				Value:    "id",
				Label:    "name", // projects a real label → enrichment skipped per-row
				LabelRef: "test_categories",
			},
		},
	}), nil)

	res, err := svc.Options(context.Background(), nil, OptionsQuery{
		Model: "test_categories",
		Field: "category_id",
	})
	if err != nil {
		t.Fatalf("options: %v", err)
	}
	if len(res.Options) != 1 {
		t.Fatalf("got %d options, want 1", len(res.Options))
	}
	if strOf(res.Options[0].Label) != "Hardware" {
		t.Errorf("label = %v, want Hardware (label_ref must not clobber a present label)", res.Options[0].Label)
	}
}

func strOf(v any) string {
	if v == nil {
		return ""
	}
	switch t := v.(type) {
	case string:
		return t
	case uuid.UUID:
		return t.String()
	default:
		return ""
	}
}
