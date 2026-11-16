package agent

import (
	"github.com/asteby/metacore-kernel/modelbase"
)

// Spanish ERP fixture for the semantic catalog tests: the shapes ops passes
// through its ModelCatalog (modelbase column/field defs under Schema).

func erpFixtureCatalog() fakeCatalog {
	str := func(k, l string) modelbase.ColumnDef { return modelbase.ColumnDef{Key: k, Label: l, Type: "text"} }
	return fakeCatalog{
		models: []ModelMeta{
			{
				Key: "Customer", Table: "customers", Title: "Clientes",
				Columns:  []string{"id", "organization_id", "name", "email", "phone", "rfc", "branch_id", "created_at"},
				SearchIn: []string{"name", "email", "rfc"},
				Schema: map[string]any{
					"columns": []modelbase.ColumnDef{
						str("name", "Nombre"), str("email", "Correo"), str("phone", "Teléfono"), str("rfc", "RFC"),
						{Key: "branch_id", Label: "Sucursal", Type: "relation", Ref: "branches"},
					},
					"fields": []modelbase.FieldDef{{Key: "name", Label: "Nombre", Type: "text", Required: true}},
				},
			},
			{
				Key: "Product", Table: "products", Title: "Productos",
				Columns:  []string{"id", "name", "sku", "price", "active", "category"},
				SearchIn: []string{"name", "sku"},
				Schema: map[string]any{
					"columns": []modelbase.ColumnDef{
						str("name", "Nombre"), str("sku", "SKU"),
						{Key: "price", Label: "Precio", Type: "currency"},
						{Key: "active", Label: "Activo", Type: "boolean"},
						{Key: "category", Label: "Categoría", Type: "select", Options: []modelbase.OptionDef{{Value: "tires", Label: "Llantas"}, {Value: "service", Label: "Servicio"}}},
					},
					"synonyms": []string{"artículos", "llantas"},
				},
			},
			{
				Key: "sales_orders", Table: "sales_orders", ModelKey: "SalesOrder", Title: "Pedidos de venta",
				Columns:  []string{"id", "organization_id", "folio", "customer_id", "status", "total", "currency_id", "order_date", "notes", "created_at", "deleted_at"},
				SearchIn: []string{"folio"},
				Schema: map[string]any{
					"columns": []modelbase.ColumnDef{
						{Key: "folio", Label: "Folio", Type: "text"},
						{Key: "customer_id", Label: "Cliente", Type: "relation", Ref: "customers"},
						{Key: "status", Label: "Estado", Type: "badge", Options: []modelbase.OptionDef{
							{Value: "draft", Label: "Borrador"}, {Value: "confirmed", Label: "Confirmado"}, {Value: "cancelled", Label: "Cancelado"},
						}},
						{Key: "total", Label: "Total", Type: "currency"},
						{Key: "currency_id", Label: "Moneda", Type: "text"},
						{Key: "order_date", Label: "Fecha", Type: "date", Filterable: true},
						{Key: "notes", Label: "Notas", Type: "textarea"},
					},
					"fields": []modelbase.FieldDef{
						{Key: "customer_id", Label: "Cliente", Type: "dynamic_select", Ref: "customers", Required: true},
						{Key: "folio", Label: "Folio", Type: "text", Required: true, Readonly: true},
					},
					"synonyms":  "pedido, orden, venta",
					"relations": []modelbase.RelationMeta{{Name: "lines", Kind: "one_to_many", Through: "sales_order_lines", Label: "Partidas"}},
				},
			},
			{
				Key: "Stock", Table: "stock", Label: "inventory.model.stock", Title: "inventory.model.stock",
				Columns:          []string{"id", "product_id", "branch_id", "quantity", "reserved", "available"},
				GeneratedColumns: []string{"available"},
				Schema: map[string]any{
					"columns": []modelbase.ColumnDef{
						{Key: "product_id", Label: "Producto", Type: "relation", Ref: "products"},
						{Key: "branch_id", Label: "Sucursal", Type: "relation", Ref: "branches"},
						{Key: "quantity", Label: "Cantidad", Type: "number"},
						{Key: "reserved", Label: "Apartado", Type: "number"},
						{Key: "available", Label: "Disponible", Type: "number"},
					},
					"description": "Inventario de productos por sucursal",
					"synonyms":    []any{"inventario", "existencia"},
				},
			},
			{
				Key: "branches", Table: "branches", Title: "Sucursales",
				Columns: []string{"id", "name", "city"},
				Schema: map[string]any{
					"columns": []modelbase.ColumnDef{str("name", "Nombre"), str("city", "Ciudad")},
				},
			},
			{
				Key: "Invoice", Table: "invoices", Title: "Facturas",
				Columns: []string{"id", "number", "customer_id", "state", "amount_total", "due_date", "paid_at"},
				Schema: map[string]any{
					"columns": []modelbase.ColumnDef{
						{Key: "number", Label: "Número", Type: "text"},
						{Key: "customer_id", Label: "Cliente", Type: "relation", Ref: "customers"},
						{Key: "state", Label: "Estado", Type: "select", Options: []modelbase.OptionDef{{Value: "pending", Label: "Pendiente"}, {Value: "paid", Label: "Pagada"}}},
						{Key: "amount_total", Label: "Importe", Type: "currency"},
						{Key: "due_date", Label: "Vencimiento", Type: "date"},
						{Key: "paid_at", Label: "Pagada el", Type: "datetime"},
					},
				},
			},
		},
		i18n: map[string]string{"inventory.model.stock": "Existencias"},
	}
}

// fixtureModel returns the catalog model of name from a fresh index.
func erpFixtureModel(name string) CatalogModel {
	m, ok := NewCatalogIndex(erpFixtureCatalog(), CatalogOptions{}).Model(name)
	if !ok {
		panic("fixture model " + name)
	}
	return m
}
