package v3

import (
	"encoding/json"
	"strings"
	"testing"
)

func withEvents(ev map[string]interface{}) map[string]interface{} {
	m := baseValid()
	m["events"] = ev
	return m
}

func emit(name string, extra map[string]interface{}) map[string]interface{} {
	e := map[string]interface{}{"name": name}
	for k, v := range extra {
		e[k] = v
	}
	return e
}

func TestEvents_Valid(t *testing.T) {
	m := withEvents(map[string]interface{}{
		"emits": []interface{}{
			emit("sales.order.created", map[string]interface{}{
				"description": "events.sales.order.created", "aggregate": "order",
				"sensitivity": "personal", "since": "1.0.0",
				"payload_schema": map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"id":    map[string]interface{}{"type": "string"},
						"lines": map[string]interface{}{"type": "array", "items": map[string]interface{}{"$ref": "#/$defs/line"}},
					},
					"$defs": map[string]interface{}{"line": map[string]interface{}{"type": "object"}},
				},
			}),
			emit("sales.order.cancelled", nil),
		},
		"consumes": []interface{}{map[string]interface{}{"name": "payments.payment.captured"}},
	})
	if err := Validate(mustJSON(t, m)); err != nil {
		t.Fatalf("expected valid, got: %v", err)
	}
}

func TestEvents_AbsentIsBackwardCompatible(t *testing.T) {
	if err := Validate(mustJSON(t, baseValid())); err != nil {
		t.Fatal(err)
	}
}

func TestEvents_Rejects(t *testing.T) {
	deep := map[string]interface{}{"type": "object"}
	cur := deep
	for i := 0; i < MaxEventSchemaDepth+2; i++ {
		n := map[string]interface{}{}
		cur["properties"] = n
		cur = n
	}
	big := map[string]interface{}{"type": "object", "description": strings.Repeat("x", MaxEventPayloadSchemaLen)}
	cases := map[string]struct {
		emits []interface{}
		want  string
	}{
		"un punto de mas":   {[]interface{}{emit("a.b.c.d", nil)}, "name"},
		"punto doble":       {[]interface{}{emit("a..c", nil)}, "name"},
		"vacio":             {[]interface{}{emit("", nil)}, "name"},
		"punto final":       {[]interface{}{emit("a.b.", nil)}, "name"},
		"ultimo mayuscula":  {[]interface{}{emit("a.b.Created", nil)}, "name"},
		"dos con mayuscula": {[]interface{}{emit("a.Order", nil)}, "name"},
		"primero mayuscula": {[]interface{}{emit("Sales.order", nil)}, "name"},
		"ajeno CRUD":        {[]interface{}{emit("customers.Customer.created", nil)}, "canonical CRUD event of addon"},
		"mayusculas":        {[]interface{}{emit("Sales.Order.Created", nil)}, ""},
		"duplicado":         {[]interface{}{emit("a.b.c", nil), emit("a.b.c", nil)}, "duplicated"},
		"descripcion":       {[]interface{}{emit("a.b.c", map[string]interface{}{"description": "Pedido creado hoy"})}, "i18n key"},
		"sensibilidad":      {[]interface{}{emit("a.b.c", map[string]interface{}{"sensitivity": "secret"})}, ""},
		"since":             {[]interface{}{emit("a.b.c", map[string]interface{}{"since": "v1"})}, "semver"},
		"aggregate":         {[]interface{}{emit("a.b.c", map[string]interface{}{"aggregate": "Order Line"})}, ""},
		"ref remoto":        {[]interface{}{emit("a.b.c", map[string]interface{}{"payload_schema": map[string]interface{}{"type": "object", "properties": map[string]interface{}{"x": map[string]interface{}{"$ref": "https://evil.example/s.json"}}}})}, "no remote references"},
		"ref archivo":       {[]interface{}{emit("a.b.c", map[string]interface{}{"payload_schema": map[string]interface{}{"properties": map[string]interface{}{"x": map[string]interface{}{"$ref": "other.json"}}}})}, "no remote references"},
		"$id":               {[]interface{}{emit("a.b.c", map[string]interface{}{"payload_schema": map[string]interface{}{"$id": "https://x/y"}})}, "not allowed"},
		"raiz no objeto":    {[]interface{}{emit("a.b.c", map[string]interface{}{"payload_schema": map[string]interface{}{"type": "string"}})}, "root type"},
		"demasiado hondo":   {[]interface{}{emit("a.b.c", map[string]interface{}{"payload_schema": deep})}, "deeper"},
		"demasiado grande":  {[]interface{}{emit("a.b.c", map[string]interface{}{"payload_schema": big})}, "maximum"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			err := Validate(mustJSON(t, withEvents(map[string]interface{}{"emits": c.emits})))
			if err == nil {
				t.Fatal("expected rejection")
			}
			if c.want != "" && !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want %q in %v", c.want, err)
			}
		})
	}
}

func TestEvents_ConsumesRejectsBadName(t *testing.T) {
	err := Validate(mustJSON(t, withEvents(map[string]interface{}{
		"consumes": []interface{}{map[string]interface{}{"name": "Payments.x.y"}},
	})))
	if err == nil {
		t.Fatal("expected rejection")
	}
}

func parseEvents(t *testing.T, key string, ev map[string]interface{}) *Manifest {
	t.Helper()
	m := withEvents(ev)
	m["metadata"].(map[string]interface{})["key"] = key
	p, _, err := ParseWithOptions(mustJSON(t, m), Options{Strict: true})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestCatalog_AggregatesFiltersAndSorts(t *testing.T) {
	sales := parseEvents(t, "sales", map[string]interface{}{"emits": []interface{}{
		emit("sales.order.created", map[string]interface{}{"sensitivity": "personal"}),
		emit("sales.invoice.issued", map[string]interface{}{"sensitivity": "public"}),
	}})
	crm := parseEvents(t, "crm", map[string]interface{}{"emits": []interface{}{emit("crm.contact.created", nil)}})
	none := &Manifest{}

	all := Catalog([]*Manifest{sales, nil, none, crm}, CatalogFilter{})
	var got []string
	for _, e := range all {
		got = append(got, e.Addon+"/"+e.Name+"/"+string(e.Sensitivity))
	}
	want := "crm/crm.contact.created/internal,sales/sales.invoice.issued/public,sales/sales.order.created/personal"
	if strings.Join(got, ",") != want {
		t.Fatalf("got %v", got)
	}
	if n := len(Catalog([]*Manifest{sales, crm}, CatalogFilter{Addons: []string{"crm"}})); n != 1 {
		t.Fatalf("addon filter: %d", n)
	}
	if n := len(Catalog([]*Manifest{sales, crm}, CatalogFilter{Sensitivity: []Sensitivity{SensitivityPersonal}})); n != 1 {
		t.Fatalf("sensitivity filter: %d", n)
	}
	if out := Catalog(nil, CatalogFilter{}); out == nil || len(out) != 0 {
		t.Fatal("empty catalog must be a non-nil empty slice (serializes as [])")
	}
	b, _ := json.Marshal(Catalog(nil, CatalogFilter{}))
	if string(b) != "[]" {
		t.Fatal(string(b))
	}
}

// Names exactly as the bus publishes them today.
var realEventNames = []string{
	"channel_orders.order_ready", "order_imported", "payment_updated",
	"customers.invoice_paid", "storefront_cart.order_placed", "checkout_paid",
	"inventory.low_stock_detected", "stock.moved",
	"inventory.Transfer.created", "inventory.SalesOrder.updated",
}

func TestEvents_RealPublishedNamesValidate(t *testing.T) {
	for _, n := range realEventNames {
		m := withEvents(map[string]interface{}{"emits": []interface{}{emit(n, nil)}})
		m["capabilities"] = append(m["capabilities"].([]interface{}),
			map[string]interface{}{"kind": "event:emit", "target": n})
		if err := Validate(mustJSON(t, m)); err != nil {
			t.Errorf("%s: %v", n, err)
		}
	}
	// consumes accepts other addons' names, CRUD included.
	m := withEvents(map[string]interface{}{"consumes": []interface{}{
		map[string]interface{}{"name": "customers.Customer.created"},
		map[string]interface{}{"name": "checkout_paid"},
	}})
	if err := Validate(mustJSON(t, m)); err != nil {
		t.Fatal(err)
	}
}

func TestEvents_CRUDMustBeOwnAddon(t *testing.T) {
	m := withEvents(map[string]interface{}{"emits": []interface{}{emit("inventory.Transfer.created", nil)}})
	if err := Validate(mustJSON(t, m)); err != nil { // addon key is "inventory"
		t.Fatalf("own CRUD event rejected: %v", err)
	}
}

func TestEvents_EmitCapabilityCoverageWarns(t *testing.T) {
	m := withEvents(map[string]interface{}{"emits": []interface{}{emit("stock.moved", nil), emit("stock.counted", nil)}})
	m["capabilities"] = append(m["capabilities"].([]interface{}),
		map[string]interface{}{"kind": "event:emit", "target": "stock.moved"})
	warns, err := ValidateWithOptions(mustJSON(t, m), Options{Strict: true})
	if err != nil {
		t.Fatalf("a missing capability must only warn: %v", err)
	}
	if len(warns) != 1 || !strings.Contains(warns[0], "[event_emit_capability]") || !strings.Contains(warns[0], "stock.counted") {
		t.Fatalf("warnings: %v", warns)
	}
	m["capabilities"] = append(m["capabilities"].([]interface{}),
		map[string]interface{}{"kind": "event:emit", "target": "stock.*"})
	if warns, _ := ValidateWithOptions(mustJSON(t, m), Options{Strict: true}); len(warns) != 0 {
		t.Fatalf("prefix capability must cover: %v", warns)
	}
}
