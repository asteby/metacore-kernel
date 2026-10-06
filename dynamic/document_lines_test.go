package dynamic

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/asteby/metacore-kernel/manifest"
	v3 "github.com/asteby/metacore-kernel/manifest/v3"
	"github.com/asteby/metacore-kernel/metadata"
	"github.com/asteby/metacore-kernel/modelbase"
)

// «Crear desde»: una factura nace de una venta con sus renglones, la cantidad
// sugerida es lo pendiente (vendido − ya facturado) y el guardado rechaza el
// exceso. Arnés: venta (dl_orders/dl_order_items) → factura
// (dl_invoices/dl_invoice_items), modelos reflect como los de un addon.

func dlDefs() map[string]manifest.ModelDefinition {
	return map[string]manifest.ModelDefinition{
		"DlOrder": {
			ModelKey: "DlOrder", TableName: "dl_orders", OrgScoped: true,
			Columns:   []manifest.ColumnDef{{Name: "number", Type: "string"}},
			Relations: []manifest.RelationDef{{Name: "items", Kind: "one_to_many", Through: "DlOrderItem", ForeignKey: "order_id"}},
		},
		"DlOrderItem": {
			ModelKey: "DlOrderItem", TableName: "dl_order_items", OrgScoped: true,
			Columns: []manifest.ColumnDef{
				{Name: "order_id", Type: "uuid"}, {Name: "product_id", Type: "string"},
				{Name: "quantity", Type: "decimal"}, {Name: "unit_price", Type: "decimal"},
			},
		},
		"DlInvoice": {
			ModelKey: "DlInvoice", TableName: "dl_invoices", OrgScoped: true,
			Columns: []manifest.ColumnDef{
				{Name: "state", Type: "string"}, {Name: "kind", Type: "string"}, {Name: "order_id", Type: "uuid"},
			},
			Relations: []manifest.RelationDef{{Name: "items", Kind: "one_to_many", Through: "DlInvoiceItem", ForeignKey: "invoice_id"}},
			DocumentForms: &manifest.DocumentFormsDef{
				TypeField: "kind", LinesField: "items",
				Types: []manifest.DocumentFormTypeDef{{
					Key: "invoice", Label: "Factura", Value: "I", Lines: &v3.DocumentFormLines{},
					Sources: []v3.DocumentFormSource{{
						Key: "sale", Label: "Venta", Model: "DlOrder", Lines: "items",
						LinkField: "order_id", LineLinkField: "order_item_id",
						ExcludeStates: []string{"cancelled"},
					}},
				}},
			},
		},
		"DlInvoiceItem": {
			ModelKey: "DlInvoiceItem", TableName: "dl_invoice_items", OrgScoped: true,
			Columns: []manifest.ColumnDef{
				{Name: "invoice_id", Type: "uuid"}, {Name: "order_item_id", Type: "uuid"},
				{Name: "product_id", Type: "string"}, {Name: "quantity", Type: "decimal"}, {Name: "unit_price", Type: "decimal"},
			},
		},
	}
}

func TestDeriveDocumentLines_MapsTypeAndSource(t *testing.T) {
	defs := dlDefs()
	lookup := func(m string) (manifest.ModelDefinition, bool) { d, ok := defs[m]; return d, ok }
	sp := DeriveDocumentLines(defs["DlInvoice"], lookup)
	if sp == nil || len(sp.Types) != 1 {
		t.Fatalf("spec = %+v", sp)
	}
	ty := sp.Types[0]
	if ty.Value != "I" || ty.Field != "items" || ty.LineModel != "DlInvoiceItem" || ty.ForeignKey != "invoice_id" {
		t.Fatalf("type = %+v", ty)
	}
	src := ty.Sources[0]
	if src.LinesModel != "DlOrderItem" || src.LinesForeignKey != "order_id" || src.LineLinkField != "order_item_id" ||
		src.StateField != "state" || src.qtyField() != "quantity" || !src.tracksRemaining() {
		t.Fatalf("source = %+v", src)
	}
	// Sin relación one_to_many con el nombre del campo de renglones: no hay a dónde escribirlos.
	d := defs["DlInvoice"]
	d.Relations = nil
	if DeriveDocumentLines(d, lookup) != nil {
		t.Fatal("a lines field without a one_to_many relation must not derive a spec")
	}
	if DeriveDocumentLines(defs["DlOrder"], lookup) != nil {
		t.Fatal("a model without document_forms must not derive a spec")
	}
}

func TestPendingLines_RemainingIsSourceMinusConsumed(t *testing.T) {
	src := DocumentLineSource{LineLinkField: "order_item_id", ExcludeStates: []string{"cancelled"}}
	lines := []map[string]any{
		{"invoice_id": "inv1", "order_item_id": "a", "quantity": 2.0},
		{"invoice_id": "inv2", "order_item_id": "a", "quantity": 1.0},
		{"invoice_id": "inv3", "order_item_id": "a", "quantity": 4.0}, // cancelada
		{"invoice_id": "gone", "order_item_id": "b", "quantity": 9.0}, // documento borrado
	}
	states := map[string]string{"inv1": "issued", "inv2": "draft", "inv3": "cancelled"}
	consumed := sumConsumed(lines, states, src, "invoice_id", "")
	if consumed["a"] != 3 || consumed["b"] != 0 {
		t.Fatalf("consumed = %v", consumed)
	}
	// Al editar inv2 sus propios renglones no cuentan.
	if c := sumConsumed(lines, states, src, "invoice_id", "inv2"); c["a"] != 2 {
		t.Fatalf("consumed excluding inv2 = %v", c)
	}
	rows := pendingLines([]map[string]any{{"id": "a", "quantity": "5"}, {"id": "b", "quantity": 1.0}}, consumed, src)
	if rows[0]["remaining_quantity"] != 2.0 || rows[0]["consumed_quantity"] != 3.0 || rows[0]["source_line_id"] != "a" {
		t.Fatalf("row a = %v", rows[0])
	}
	if rows[1]["remaining_quantity"] != 1.0 {
		t.Fatalf("row b = %v", rows[1])
	}
	// remaining_qty_field del origen gana sobre el cálculo; nunca negativo.
	own := DocumentLineSource{RemainingQtyField: "pending"}
	r := pendingLines([]map[string]any{{"id": "x", "quantity": 5.0, "pending": -1.0}}, nil, own)
	if r[0]["remaining_quantity"] != 0.0 {
		t.Fatalf("remaining from column = %v", r[0])
	}
}

func TestAddQuantityErrors_SpanishMessagePerLine(t *testing.T) {
	src := DocumentLineSource{Label: "Venta", LineLinkField: "order_item_id"}
	rows := []map[string]any{
		{"order_item_id": "a", "quantity": 2.0},
		{"order_item_id": "a", "quantity": 1.0},  // el mismo renglón partido: 3 > 2
		{"quantity": 7.0},                        // renglón libre: no se valida
		{"order_item_id": "zz", "quantity": 1.0}, // origen inexistente
	}
	ve := NewValidationError()
	addQuantityErrors(ve, "items", rows, nil, map[string]float64{"a": 2}, src)
	if _, bad := ve.Fields["items.0.quantity"]; bad {
		t.Fatalf("first split line is within the pending quantity: %v", ve.Fields)
	}
	got := ve.Fields["items.1.quantity"]
	if len(got) != 1 || got[0].Code != "exceeds_remaining" ||
		!strings.Contains(got[0].Message, "Renglón 2: la cantidad 1 excede lo pendiente de «Venta» (0)") {
		t.Fatalf("items.1 = %+v", got)
	}
	if _, bad := ve.Fields["items.2.quantity"]; bad {
		t.Fatal("a free line must not be checked")
	}
	if nf := ve.Fields["items.3.quantity"]; len(nf) != 1 || nf[0].Code != "source_line_not_found" {
		t.Fatalf("items.3 = %+v", nf)
	}
}

// ---- de punta a punta sobre sqlite -------------------------------------------

type dlMeta struct{ table string }

func (m dlMeta) TableName() string { return m.table }
func (m dlMeta) DefineTable() modelbase.TableMetadata {
	return modelbase.TableMetadata{Title: m.table, Columns: []modelbase.ColumnDef{{Key: "id"}}}
}
func (m dlMeta) DefineModal() modelbase.ModalMetadata { return modelbase.ModalMetadata{Title: m.table} }

func newDocLinesService(t *testing.T, withResolver bool, hooks ...*HookRegistry) (*Service, *gorm.DB) {
	t.Helper()
	db := setupTestDB(t)
	defs := dlDefs()
	types := map[string]reflect.Type{}
	for name, def := range defs {
		cols := "id TEXT PRIMARY KEY, organization_id TEXT, created_at DATETIME, updated_at DATETIME, deleted_at DATETIME, created_by_id TEXT, updated_by_id TEXT, deleted_by_id TEXT"
		for _, c := range def.Columns {
			sqlType := "TEXT"
			if c.Type == "decimal" {
				sqlType = "REAL"
			}
			cols += ", " + c.Name + " " + sqlType
		}
		mustExec(t, db, fmt.Sprintf("CREATE TABLE %s (%s)", def.TableName, cols))
		typ, err := BuildStructType(def)
		if err != nil {
			t.Fatal(err)
		}
		types[name] = typ
		table := def.TableName
		modelbase.Register(name, func() modelbase.ModelDefiner { return dlMeta{table: table} })
	}
	cfg := Config{
		DB:       db,
		Metadata: metadata.New(metadata.Config{CacheTTL: -1}),
		ModelResolver: func(_ context.Context, name string) (any, bool) {
			typ, ok := types[name]
			if !ok {
				return nil, false
			}
			return reflect.New(typ).Interface(), true
		},
		TableNameResolver: func(_ context.Context, name string) (string, bool) {
			d, ok := defs[name]
			return d.TableName, ok
		},
	}
	if len(hooks) > 0 {
		cfg.Hooks = hooks[0]
	}
	if withResolver {
		lookup := func(m string) (manifest.ModelDefinition, bool) { d, ok := defs[m]; return d, ok }
		cfg.DocumentLinesResolver = func(_ context.Context, model string) (*DocumentLinesSpec, bool) {
			d, ok := defs[model]
			if !ok {
				return nil, false
			}
			sp := DeriveDocumentLines(d, lookup)
			return sp, sp != nil
		}
	}
	return New(cfg), db
}

// seedOrder crea una venta de 5 piezas a 100 y devuelve (venta, renglón).
func seedOrder(t *testing.T, svc *Service, user *fakeUser) (string, string) {
	t.Helper()
	ctx := context.Background()
	order := uuid.NewString()
	if _, err := svc.Create(ctx, "DlOrder", user, map[string]any{"id": order, "number": "V-1"}); err != nil {
		t.Fatalf("order: %v", err)
	}
	item := uuid.NewString()
	if _, err := svc.Create(ctx, "DlOrderItem", user, map[string]any{
		"id": item, "order_id": order, "product_id": "llanta", "quantity": 5, "unit_price": 100,
	}); err != nil {
		t.Fatalf("order item: %v", err)
	}
	return order, item
}

func invoiceFrom(order, item string, qty float64, state string) map[string]any {
	return map[string]any{
		"id": uuid.NewString(), "kind": "I", "state": state, "order_id": order,
		"items": []any{
			map[string]any{"kind": "section", "description": "Llantas"},
			map[string]any{"kind": "item", "order_item_id": item, "product_id": "llanta", "quantity": qty, "unit_price": 100.0},
		},
	}
}

func countInvoiceItems(t *testing.T, db *gorm.DB, invoiceID string) []map[string]any {
	t.Helper()
	var rows []map[string]any
	db.Raw(`SELECT * FROM dl_invoice_items WHERE invoice_id = ? AND deleted_at IS NULL`, invoiceID).Scan(&rows)
	return rows
}

func TestCreate_WritesDocumentLines(t *testing.T) {
	svc, db := newDocLinesService(t, true)
	user := newUser(uuid.New())
	order, item := seedOrder(t, svc, user)

	in := invoiceFrom(order, item, 3, "draft")
	out, err := svc.Create(context.Background(), "DlInvoice", user, in)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	rows := countInvoiceItems(t, db, fmt.Sprint(out["id"]))
	if len(rows) != 1 { // la sección no es un renglón
		t.Fatalf("invoice lines = %d, want 1 (%v)", len(rows), rows)
	}
	if fmt.Sprint(rows[0]["order_item_id"]) != item || numericValue(rows[0]["quantity"]) != 3 ||
		fmt.Sprint(rows[0]["organization_id"]) != user.orgID.String() {
		t.Fatalf("line = %v", rows[0])
	}
}

// Sin resolver (host que no lo cablea) el comportamiento previo no cambia.
func TestCreate_WithoutResolverIgnoresLines(t *testing.T) {
	svc, db := newDocLinesService(t, false)
	user := newUser(uuid.New())
	order, item := seedOrder(t, svc, user)
	out, err := svc.Create(context.Background(), "DlInvoice", user, invoiceFrom(order, item, 3, "draft"))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if rows := countInvoiceItems(t, db, fmt.Sprint(out["id"])); len(rows) != 0 {
		t.Fatalf("lines written without resolver: %v", rows)
	}
}

func TestSourceLines_ServesPendingQuantity(t *testing.T) {
	svc, _ := newDocLinesService(t, true)
	user := newUser(uuid.New())
	ctx := context.Background()
	order, item := seedOrder(t, svc, user)

	rows, err := svc.SourceLines(ctx, "DlInvoice", user, "sale", order, "")
	if err != nil || len(rows) != 1 || rows[0]["remaining_quantity"] != 5.0 {
		t.Fatalf("before invoicing: %v %v", rows, err)
	}
	first, err := svc.Create(ctx, "DlInvoice", user, invoiceFrom(order, item, 3, "draft"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Create(ctx, "DlInvoice", user, invoiceFrom(order, item, 2, "cancelled")); err != nil {
		t.Fatal(err) // una cancelada no consume
	}
	rows, _ = svc.SourceLines(ctx, "DlInvoice", user, "sale", order, "")
	if rows[0]["remaining_quantity"] != 2.0 || rows[0]["consumed_quantity"] != 3.0 || rows[0]["source_line_id"] != item {
		t.Fatalf("after invoicing 3 of 5: %v", rows[0])
	}
	// Editando la primera factura, su propia cantidad vuelve a estar disponible.
	rows, _ = svc.SourceLines(ctx, "DlInvoice", user, "sale", order, fmt.Sprint(first["id"]))
	if rows[0]["remaining_quantity"] != 5.0 {
		t.Fatalf("excluding the edited invoice: %v", rows[0])
	}
	// Otra organización no ve la venta.
	other, _ := svc.SourceLines(ctx, "DlInvoice", newUser(uuid.New()), "sale", order, "")
	if len(other) != 0 {
		t.Fatalf("cross-org source lines: %v", other)
	}
	if _, err := svc.SourceLines(ctx, "DlInvoice", user, "nope", order, ""); !errors.Is(err, ErrRecordNotFound) {
		t.Fatalf("unknown source: %v", err)
	}
}

func TestCreate_RejectsQuantityOverPending(t *testing.T) {
	svc, db := newDocLinesService(t, true)
	user := newUser(uuid.New())
	ctx := context.Background()
	order, item := seedOrder(t, svc, user)
	if _, err := svc.Create(ctx, "DlInvoice", user, invoiceFrom(order, item, 3, "issued")); err != nil {
		t.Fatal(err)
	}

	in := invoiceFrom(order, item, 3, "draft") // quedan 2
	_, err := svc.Create(ctx, "DlInvoice", user, in)
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("err = %v, want a 422 ValidationError", err)
	}
	fe := ve.Fields["items.1.quantity"] // [sección, renglón]
	if len(fe) != 1 || fe[0].Code != "exceeds_remaining" || !strings.Contains(fe[0].Message, "excede lo pendiente de «Venta» (2)") {
		t.Fatalf("field errors = %+v", ve.Fields)
	}
	var n int64
	db.Table("dl_invoices").Where("id = ?", in["id"]).Count(&n)
	if n != 0 {
		t.Fatal("a rejected invoice must not be inserted")
	}
	// Exactamente lo pendiente sí pasa.
	if _, err := svc.Create(ctx, "DlInvoice", user, invoiceFrom(order, item, 2, "draft")); err != nil {
		t.Fatalf("pending quantity rejected: %v", err)
	}
}

func TestUpdate_ReplacesLinesAndChecksExcludingItself(t *testing.T) {
	svc, db := newDocLinesService(t, true)
	user := newUser(uuid.New())
	ctx := context.Background()
	order, item := seedOrder(t, svc, user)
	out, err := svc.Create(ctx, "DlInvoice", user, invoiceFrom(order, item, 3, "draft"))
	if err != nil {
		t.Fatal(err)
	}
	id := uuid.MustParse(fmt.Sprint(out["id"]))
	upd := invoiceFrom(order, item, 5, "draft")
	delete(upd, "id")
	if _, err := svc.Update(ctx, "DlInvoice", user, id, upd); err != nil {
		t.Fatalf("raising its own draft to the full 5: %v", err)
	}
	rows := countInvoiceItems(t, db, id.String())
	if len(rows) != 1 || numericValue(rows[0]["quantity"]) != 5 {
		t.Fatalf("lines after update = %v", rows)
	}
	upd["items"] = []any{map[string]any{"order_item_id": item, "quantity": 6.0}}
	if _, err := svc.Update(ctx, "DlInvoice", user, id, upd); err == nil {
		t.Fatal("6 of 5 accepted on update")
	}
}

// Un renglón inválido no deja la factura sin renglones: se deshace el
// encabezado y el error queda en el renglón (items.<i>.<campo>).
func TestCreate_LineFailureUndoesTheDocument(t *testing.T) {
	hooks := NewHookRegistry()
	hooks.RegisterBeforeCreate("DlInvoiceItem", func(_ context.Context, _ HookContext, in map[string]any) error {
		if in["product_id"] == nil || in["product_id"] == "" {
			return NewValidationError().AddMessage("product_id", "Elige el producto").Err()
		}
		return nil
	})
	svc, db := newDocLinesService(t, true, hooks)
	user := newUser(uuid.New())
	order, item := seedOrder(t, svc, user)
	in := invoiceFrom(order, item, 1, "draft")
	in["items"] = append(in["items"].([]any), map[string]any{"description": "Flete", "quantity": 1.0, "unit_price": 50.0})

	_, err := svc.Create(context.Background(), "DlInvoice", user, in)
	var ve *ValidationError
	// Índice del arreglo enviado (la sección cuenta), el que usa el formulario.
	if !errors.As(err, &ve) || len(ve.Fields["items.2.product_id"]) != 1 {
		t.Fatalf("err = %v (%+v)", err, ve)
	}
	var docs, lines int64
	db.Table("dl_invoices").Where("id = ?", in["id"]).Count(&docs)
	db.Table("dl_invoice_items").Where("invoice_id = ?", in["id"]).Count(&lines)
	if docs != 0 || lines != 0 {
		t.Fatalf("left behind: %d invoices, %d lines", docs, lines)
	}
}
