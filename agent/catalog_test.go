package agent

import (
	"strings"
	"testing"
)

type fakeCatalog struct {
	models  []ModelMeta
	aliases map[string]string
	hints   map[string][]string
	i18n    map[string]string
	icons   map[string]string
}

func (f fakeCatalog) Lookup(key string) (ModelMeta, bool) {
	for _, m := range f.models {
		if m.Key == key || (m.Table != "" && m.Table == key) {
			return m, true
		}
	}
	return ModelMeta{}, false
}
func (f fakeCatalog) All() []ModelMeta { return f.models }
func (f fakeCatalog) AliasSource(k string) (string, bool) {
	v, ok := f.aliases[k]
	return v, ok
}
func (f fakeCatalog) Hints(l string) []string { return f.hints[l] }
func (f fakeCatalog) Translate(raw string) string {
	if v, ok := f.i18n[raw]; ok {
		return v
	}
	return raw
}
func (f fakeCatalog) NavIcon(k string) string { return f.icons[k] }

func testCatalog() fakeCatalog {
	return fakeCatalog{
		models: []ModelMeta{
			{Key: "Stock", Table: "stock", Label: "inventory.model.stock", Title: "inventory.model.stock"},
			{Key: "customers", Table: "customers", Title: "Clientes"},
			{Key: "sales_orders", Table: "sales_orders", ModelKey: "SalesOrder"},
		},
		aliases: map[string]string{"alias_cust": "customers"},
		hints:   map[string][]string{"cliente": {"customers"}, "venta": {"sales_orders"}},
		i18n:    map[string]string{"inventory.model.stock": "Existencias"},
		icons:   map[string]string{"stock": "Warehouse", "customers": "users"},
	}
}

func TestResolveModelKey(t *testing.T) {
	c := testCatalog()
	cases := map[string]string{
		"stock":                       "stock",
		"Stock":                       "stock", // registry key prefers its table
		"customer":                    "customers",
		"sales_order":                 "sales_orders",
		"SalesOrder":                  "sales_orders",
		"cliente":                     "customers",
		"venta":                       "sales_orders",
		"alias_cust":                  "customers",
		"inventory.model.stock":       "stock", // exact label
		"customers.model.customer":    "customers",
		"nothing.model.unknown":       "",
		"totally_unknown":             "totally_unknown",
		"  ":                          "",
		"customers.model.customer.x?": "customers",
	}
	for in, want := range cases {
		if got := ResolveModelKey(c, in); got != want {
			t.Errorf("ResolveModelKey(%q)=%q want %q", in, got, want)
		}
	}
}

func TestModelLabelAndIcon(t *testing.T) {
	c := testCatalog()
	if got := ModelLabel(c, "stock"); got != "Existencias" {
		t.Errorf("label %q", got)
	}
	if got := ModelLabel(c, "customers"); got != "Clientes" {
		t.Errorf("label %q", got)
	}
	if got := ModelLabel(c, "sales_orders"); got != "Sales Orders" {
		t.Errorf("humanized %q", got)
	}
	if ModelLabel(c, "") != "datos" {
		t.Error("empty key must be datos")
	}
	if ModelIcon(c, "stock") != "warehouse" || ModelIcon(c, "sales_orders") != "database" || ModelIcon(c, "") != "database" {
		t.Error("icon resolution wrong")
	}
	for in, want := range map[string]string{"ShoppingCart": "shopping-cart", "shopping_cart": "shopping-cart", "package-search": "package-search"} {
		if got := lucideKebab(in); got != want {
			t.Errorf("lucideKebab(%q)=%q", in, got)
		}
	}
}

func TestCatalogRegistryImplementsModelRegistry(t *testing.T) {
	var r ModelRegistry = CatalogRegistry{Catalog: testCatalog()}
	if r.ResolveModelKey("cliente") != "customers" || r.FieldLabel("m", "k", "Raw") != "Raw" || r.EnumValueLabel("m", "f", "v") != "v" {
		t.Fatal("registry adapter wrong")
	}
	if !strings.Contains(r.ModelLabel("stock"), "Existencias") {
		t.Fatal("label")
	}
}
