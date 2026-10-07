package modelbase

import (
	"encoding/json"
	"testing"
)

// The served shape must equal what the SDK's DocumentFormsManifest types read.
func TestDocumentForms_WireShape(t *testing.T) {
	req := false
	in := TableMetadata{Title: "Docs", DocumentForms: &DocumentForms{
		TypeField: "type", LinesField: "lines",
		Types: []DocumentFormType{
			{Key: "invoice", Label: "Factura", Value: "I", Fields: []FieldDef{{Key: "customer", Label: "Cliente", Type: "text", Required: true}},
				Lines: &DocumentFormLines{Columns: []string{"tax"}, PriceSource: "cost", Required: &req, Title: "Partidas"}, SubmitLabel: "Emitir"},
			{Key: "rep", Label: "REP", Fields: []FieldDef{}, Lines: &DocumentFormLines{}},
			{Key: "global", Label: "Global", Fields: []FieldDef{}},
		},
	}}
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var back map[string]any
	_ = json.Unmarshal(raw, &back)
	df := back["document_forms"].(map[string]any)
	types := df["types"].([]any)
	lines := types[0].(map[string]any)["lines"].(map[string]any)
	if lines["price_source"] != "cost" || lines["required"] != false || lines["title"] != "Partidas" {
		t.Errorf("lines = %v", lines)
	}
	if types[1].(map[string]any)["lines"] != true {
		t.Errorf("option-less lines must be true: %v", types[1])
	}
	if _, has := types[2].(map[string]any)["lines"]; has {
		t.Errorf("absent lines must be omitted: %v", types[2])
	}
	// Round trip through the type.
	var out TableMetadata
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	if out.DocumentForms.Types[0].Lines.PriceSource != "cost" || out.DocumentForms.Types[1].Lines == nil {
		t.Errorf("round trip lost lines: %+v", out.DocumentForms.Types)
	}
	// And a metadata without the block omits the key.
	plain, _ := json.Marshal(TableMetadata{Title: "x"})
	var pm map[string]any
	_ = json.Unmarshal(plain, &pm)
	if _, has := pm["document_forms"]; has {
		t.Errorf("plain table leaked document_forms: %s", plain)
	}
}

func TestOptionFilter_JSON(t *testing.T) {
	var f FieldDef
	if err := json.Unmarshal([]byte(`{"key":"k","label":"L","type":"dynamic_select","option_filter":{"field":"status","not_in":["cancelada"]}}`), &f); err != nil {
		t.Fatal(err)
	}
	if len(f.OptionFilter) != 1 || f.OptionFilter[0].Field != "status" {
		t.Fatalf("object form = %+v", f.OptionFilter)
	}
	raw, _ := json.Marshal(f)
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	of, ok := m["option_filter"].([]any)
	if !ok || len(of) != 1 {
		t.Fatalf("served as list, got %s", raw)
	}
	var col ColumnDef
	if err := json.Unmarshal([]byte(`{"key":"c","label":"C","type":"text","option_filter":[{"field":"a","equals":"x"},{"field":"b","in":[1,true]}]}`), &col); err != nil {
		t.Fatal(err)
	}
	if len(col.OptionFilter) != 2 || col.OptionFilter[0].Equals != "x" || len(col.OptionFilter[1].In) != 2 {
		t.Fatalf("array form = %+v", col.OptionFilter)
	}
	empty, _ := json.Marshal(ColumnDef{Key: "c"})
	var em map[string]any
	_ = json.Unmarshal(empty, &em)
	if _, has := em["option_filter"]; has {
		t.Errorf("empty option_filter must be omitted: %s", empty)
	}
	cfg, _ := json.Marshal(FieldOptionsConfig{Type: "dynamic", Source: "s", ExtraColumns: []string{"status"}})
	var cm map[string]any
	_ = json.Unmarshal(cfg, &cm)
	if cm["extra_columns"].([]any)[0] != "status" {
		t.Errorf("extra_columns = %s", cfg)
	}
}

// A lines step carrying only the DocumentEditor options (kind/discount_mode/
// open_documents) must serve as an object, never collapse to `true`, and the
// editor block of the type rides along with snake_case keys.
func TestDocumentForms_EditorWireShape(t *testing.T) {
	ty := DocumentFormType{
		Key: "payment", Label: "Cobro", Fields: []FieldDef{{Key: "invoice_id", Label: "Factura", Type: "text", DefaultFromRecord: "id"}},
		Layout: "editor", SubmitAction: "collect_payment_create",
		Party:   &DocumentFormParty{Field: "customer_id", Model: "Customer"},
		Sources: []DocumentFormSource{{Key: "quote", Label: "Cotización", Model: "Quote", Lines: "items"}},
		Preview: &DocumentFormPreview{Action: "preview_receipt"},
		Lines:   &DocumentFormLines{Kind: "allocation"},
	}
	raw, err := json.Marshal(ty)
	if err != nil {
		t.Fatal(err)
	}
	var back map[string]any
	_ = json.Unmarshal(raw, &back)
	if l, ok := back["lines"].(map[string]any); !ok || l["kind"] != "allocation" {
		t.Errorf("lines = %v", back["lines"])
	}
	for _, k := range []string{"layout", "submit_action", "party", "sources", "preview"} {
		if back[k] == nil {
			t.Errorf("%s missing: %s", k, raw)
		}
	}
	if back["fields"].([]any)[0].(map[string]any)["default_from_record"] != "id" {
		t.Errorf("default_from_record missing: %s", raw)
	}
	// Without the editor block nothing new is emitted.
	plain, _ := json.Marshal(DocumentFormType{Key: "x", Label: "X", Fields: []FieldDef{}, Lines: &DocumentFormLines{}})
	if string(plain) != `{"key":"x","label":"X","fields":[],"lines":true}` {
		t.Errorf("plain type = %s", plain)
	}
}

// OptionFilter.Match mirrors runtime-react option-filter.ts: positive rules
// need the property, negative rules keep a row without it.
func TestOptionFilterMatch(t *testing.T) {
	var f OptionFilter
	if err := json.Unmarshal([]byte(`[{"field":"status","in":["sent","accepted"]},{"field":"converted_to","not_in":["order","layaway"]}]`), &f); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		row  map[string]any
		want bool
	}{
		{"sent, not converted", map[string]any{"status": "sent", "converted_to": nil}, true},
		{"case and spaces", map[string]any{"status": " Accepted ", "converted_to": ""}, true},
		{"converted", map[string]any{"status": "accepted", "converted_to": "order"}, false},
		{"draft", map[string]any{"status": "draft"}, false},
		{"positive rule needs the property", map[string]any{}, false},
	}
	for _, c := range cases {
		if got := f.Match(c.row); got != c.want {
			t.Errorf("%s: Match = %v, want %v", c.name, got, c.want)
		}
	}
	eq := OptionFilter{{Field: "n", Equals: 1.0}, {Field: "b", NotEquals: true}}
	if !eq.Match(map[string]any{"n": float64(1), "b": false}) || eq.Match(map[string]any{"n": 1.0, "b": "TRUE"}) {
		t.Error("equals / not_equals over numbers and booleans")
	}
	if !(OptionFilter{}).Match(nil) {
		t.Error("an empty filter matches every row")
	}
}
