package v3

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestOptionFilterUnmarshalObjectAndArray(t *testing.T) {
	var one, many OptionFilter
	if err := json.Unmarshal([]byte(`{"field":"status","not_in":["cancelada"]}`), &one); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(`[{"field":"status","equals":"a"},{"field":"n","in":[1,2]}]`), &many); err != nil {
		t.Fatal(err)
	}
	if len(one) != 1 || one[0].Field != "status" || len(one[0].NotIn) != 1 {
		t.Errorf("object form = %+v", one)
	}
	if len(many) != 2 || many[0].Equals != "a" || len(many[1].In) != 2 {
		t.Errorf("array form = %+v", many)
	}
	out, _ := json.Marshal(one)
	if string(out) != `[{"field":"status","not_in":["cancelada"]}]` {
		t.Errorf("always served as a list, got %s", out)
	}
}

func TestValidateOptionFilterRules(t *testing.T) {
	bad := []struct {
		name string
		f    OptionFilter
		want string
	}{
		{"no field", OptionFilter{{Equals: "a"}}, "field is empty"},
		{"no operator", OptionFilter{{Field: "status"}}, "no operator"},
		{"empty in", OptionFilter{{Field: "status", In: []any{}}}, "in is empty"},
		{"non scalar", OptionFilter{{Field: "status", Equals: map[string]any{"a": 1}}}, "equals must be"},
		{"non scalar in", OptionFilter{{Field: "status", NotIn: []any{[]any{"x"}}}}, "not_in entries"},
	}
	for _, c := range bad {
		errs := validateOptionFilter("w", c.f)
		if len(errs) == 0 || !strings.Contains(strings.Join(errs, "|"), c.want) {
			t.Errorf("%s: errs = %v, want %q", c.name, errs, c.want)
		}
	}
	ok := OptionFilter{{Field: "status", NotIn: []any{"x", 1.0, true}}, {Field: "f", Equals: false}}
	if errs := validateOptionFilter("w", ok); len(errs) != 0 {
		t.Errorf("valid filter rejected: %v", errs)
	}
}

func TestDocumentFormLinesForms(t *testing.T) {
	var df DocumentFormType
	for in, want := range map[string]bool{`{"lines":true}`: true, `{"lines":{"columns":["tax"]}}`: true, `{"lines":false}`: false, `{}`: false} {
		df = DocumentFormType{}
		if err := json.Unmarshal([]byte(in), &df); err != nil {
			t.Fatal(err)
		}
		if df.Lines.Enabled() != want {
			t.Errorf("%s: Enabled = %v, want %v", in, df.Lines.Enabled(), want)
		}
	}
	// Round trip keeps the shape.
	for _, in := range []string{`true`, `false`, `{"columns":["tax"],"price_source":"cost"}`} {
		var l DocumentFormLines
		if err := json.Unmarshal([]byte(in), &l); err != nil {
			t.Fatal(err)
		}
		out, _ := json.Marshal(l)
		if string(out) != in {
			t.Errorf("round trip %s -> %s", in, out)
		}
	}
}

func TestValidateDocumentFormsRules(t *testing.T) {
	cols := map[string]struct{}{"type": {}}
	cm := map[string]map[string]struct{}{}
	good := &DocumentForms{TypeField: "type", Types: []DocumentFormType{
		{Key: "invoice", Label: "Factura", Value: "I", Fields: []ActionField{{Key: "a", Type: "text"}}, Lines: &DocumentFormLines{Columns: []string{"tax"}}},
		{Key: "credit", Label: "NC", Value: "E", Fields: []ActionField{{Key: "a", Type: "text"}}},
	}}
	if errs := validateDocumentForms("w", good, cols, cm); len(errs) != 0 {
		t.Fatalf("valid rejected: %v", errs)
	}
	mut := func(f func(d *DocumentForms)) []string {
		d := *good
		d.Types = append([]DocumentFormType(nil), good.Types...)
		d.Types[0].Fields = append([]ActionField(nil), good.Types[0].Fields...)
		f(&d)
		return validateDocumentForms("w", &d, cols, cm)
	}
	cases := []struct {
		name string
		f    func(d *DocumentForms)
		want string
	}{
		{"empty types", func(d *DocumentForms) { d.Types = nil }, "types is empty"},
		{"bad key", func(d *DocumentForms) { d.Types[0].Key = "Bad Key" }, "snake_case"},
		{"no label", func(d *DocumentForms) { d.Types[0].Label = " " }, "label is empty"},
		{"dup value", func(d *DocumentForms) { d.Types[1].Value = "I" }, "already written"},
		{"dup field", func(d *DocumentForms) { d.Types[0].Fields = append(d.Types[0].Fields, ActionField{Key: "a"}) }, "duplicated within"},
		{"lines clash", func(d *DocumentForms) { d.Types[0].Fields = append(d.Types[0].Fields, ActionField{Key: "lines"}) }, "collides"},
		{"bad lines_field", func(d *DocumentForms) { d.LinesField = "Bad" }, "lines_field"},
		{"column dup", func(d *DocumentForms) { d.Types[0].Lines = &DocumentFormLines{Columns: []string{"tax", "tax"}} }, "duplicated"},
	}
	for _, c := range cases {
		errs := mut(c.f)
		if len(errs) == 0 || !strings.Contains(strings.Join(errs, "|"), c.want) {
			t.Errorf("%s: errs = %v, want %q", c.name, errs, c.want)
		}
	}
}

// «Crear desde»: las columnas de cantidad pendiente del origen se validan al
// publicar (un error de dedo dejaría el restante sin calcular en silencio).
func TestValidateDocumentFormSources(t *testing.T) {
	good := DocumentFormSource{
		Key: "sale", Label: "Venta", Model: "customers.SalesOrder", Lines: "items",
		LinkField: "sales_order_id", LineLinkField: "sales_order_item_id", QtyField: "quantity",
		ExcludeStates: []string{"cancelled"},
	}
	if errs := validateDocumentFormSources("w", []DocumentFormSource{good}); len(errs) != 0 {
		t.Fatalf("valid rejected: %v", errs)
	}
	cases := []struct {
		name string
		f    func(s *DocumentFormSource)
		want string
	}{
		{"bad link", func(s *DocumentFormSource) { s.LineLinkField = "Sales Item" }, "line_link_field"},
		{"bad remaining col", func(s *DocumentFormSource) { s.RemainingQtyField = "qty-left" }, "remaining_qty_field"},
		{"relative endpoint", func(s *DocumentFormSource) { s.RemainingEndpoint = "data/x" }, "absolute path"},
		{"exclude without link", func(s *DocumentFormSource) { s.LineLinkField = "" }, "needs line_link_field"},
		{"no lines", func(s *DocumentFormSource) { s.Lines = "" }, "model and lines"},
	}
	for _, c := range cases {
		s := good
		c.f(&s)
		errs := validateDocumentFormSources("w", []DocumentFormSource{s})
		if len(errs) == 0 || !strings.Contains(strings.Join(errs, "|"), c.want) {
			t.Errorf("%s: errs = %v, want %q", c.name, errs, c.want)
		}
	}
	if errs := validateDocumentFormSources("w", []DocumentFormSource{good, good}); len(errs) == 0 {
		t.Error("duplicated source key accepted")
	}
}

// Los campos nuevos viajan en el round-trip JSON hasta la metadata servida.
func TestDocumentFormSource_RemainingFieldsRoundTrip(t *testing.T) {
	raw := `{"key":"sale","label":"Venta","model":"customers.SalesOrder","lines":"items","qty_field":"qty","line_link_field":"sales_order_item_id","remaining_qty_field":"pending","remaining_endpoint":"/x","exclude_states":["cancelled"]}`
	var s DocumentFormSource
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		t.Fatal(err)
	}
	if s.QtyField != "qty" || s.LineLinkField != "sales_order_item_id" || s.RemainingQtyField != "pending" || s.RemainingEndpoint != "/x" || len(s.ExcludeStates) != 1 {
		t.Fatalf("fields lost: %+v", s)
	}
}
