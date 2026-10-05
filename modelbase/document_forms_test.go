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
