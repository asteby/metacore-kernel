package metadata

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"

	"github.com/asteby/metacore-kernel/modelbase"
)

func testDocumentForms() *modelbase.DocumentForms {
	req := true
	return &modelbase.DocumentForms{
		TypeField: "type",
		Types: []modelbase.DocumentFormType{
			{
				Key: "invoice", Label: "docs.invoice", Value: "I",
				Fields: []modelbase.FieldDef{{
					Key: "invoice_id", Label: "docs.invoice_field", Type: "dynamic_select",
					OptionsConfig: &modelbase.FieldOptionsConfig{Type: "dynamic", Source: "invoices", ExtraColumns: []string{"status"}},
					OptionFilter:  modelbase.OptionFilter{{Field: "status", NotIn: []any{"cancelada"}}},
				}},
				Lines: &modelbase.DocumentFormLines{Title: "docs.lines", Required: &req, Columns: []string{"tax"}},
			},
			{Key: "global", Label: "Global", Fields: []modelbase.FieldDef{}},
		},
	}
}

// docModel serves its document flow through the HasDocumentForms interface.
type docModel struct{ fakeModel }

func (docModel) DefineDocumentForms() *modelbase.DocumentForms { return testDocumentForms() }

// docTableModel sets it directly in DefineTable (the host's addon definer path).
type docTableModel struct{ fakeModel }

func (m docTableModel) DefineTable() modelbase.TableMetadata {
	t := m.fakeModel.DefineTable()
	t.DocumentForms = &modelbase.DocumentForms{Types: []modelbase.DocumentFormType{{Key: "own", Label: "Own", Fields: []modelbase.FieldDef{}}}}
	return t
}

// docBothModel sets DefineTable AND HasDocumentForms: DefineTable wins.
type docBothModel struct{ docTableModel }

func (docBothModel) DefineDocumentForms() *modelbase.DocumentForms { return testDocumentForms() }

func registerDoc(t *testing.T, mk func(key string) modelbase.ModelDefiner) string {
	t.Helper()
	key := fmt.Sprintf("metadata_docforms_%s_%d", t.Name(), time.Now().UnixNano())
	modelbase.Register(key, func() modelbase.ModelDefiner { return mk(key) })
	return key
}

func TestGetTable_ServesDocumentFormsFromHasDocumentForms(t *testing.T) {
	key := registerDoc(t, func(k string) modelbase.ModelDefiner { return &docModel{fakeModel{key: k, title: "Docs"}} })
	app, _ := newTestHandler(t)
	status, env := doRequest(t, app, "GET", "/metadata/table/"+key)
	if status != fiber.StatusOK {
		t.Fatalf("status %d (%s)", status, env.Message)
	}
	var wire struct {
		DocumentForms struct {
			TypeField string `json:"type_field"`
			Types     []struct {
				Key    string `json:"key"`
				Value  string `json:"value"`
				Fields []struct {
					Key          string `json:"key"`
					OptionFilter []struct {
						Field string `json:"field"`
						NotIn []any  `json:"not_in"`
					} `json:"option_filter"`
					OptionsConfig struct {
						ExtraColumns []string `json:"extra_columns"`
					} `json:"optionsConfig"`
				} `json:"fields"`
				Lines json.RawMessage `json:"lines"`
			} `json:"types"`
		} `json:"document_forms"`
	}
	if err := json.Unmarshal(env.Data, &wire); err != nil {
		t.Fatal(err)
	}
	df := wire.DocumentForms
	if df.TypeField != "type" || len(df.Types) != 2 || df.Types[0].Value != "I" {
		t.Fatalf("document_forms = %s", env.Data)
	}
	f := df.Types[0].Fields[0]
	if f.Key != "invoice_id" || len(f.OptionFilter) != 1 || f.OptionFilter[0].NotIn[0] != "cancelada" || f.OptionsConfig.ExtraColumns[0] != "status" {
		t.Errorf("field = %+v", f)
	}
	var lines map[string]any
	if err := json.Unmarshal(df.Types[0].Lines, &lines); err != nil || lines["required"] != true {
		t.Errorf("lines = %s", df.Types[0].Lines)
	}
	if df.Types[1].Lines != nil {
		t.Errorf("type without lines served lines = %s", df.Types[1].Lines)
	}
}

func TestGetTable_DefineTableDocumentFormsWins(t *testing.T) {
	key := registerDoc(t, func(k string) modelbase.ModelDefiner {
		return &docBothModel{docTableModel{fakeModel{key: k, title: "Both"}}}
	})
	meta, err := New(Config{CacheTTL: -1}).GetTable(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	if meta.DocumentForms == nil || len(meta.DocumentForms.Types) != 1 || meta.DocumentForms.Types[0].Key != "own" {
		t.Fatalf("DefineTable's document_forms must win, got %+v", meta.DocumentForms)
	}
}

// Plain models serve no document_forms key at all (retro-compat).
func TestGetTable_NoDocumentFormsOmitted(t *testing.T) {
	key := registerFakeModel(t, "Plain")
	meta, err := New(Config{CacheTTL: -1}).GetTable(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(meta)
	var back map[string]any
	_ = json.Unmarshal(raw, &back)
	if _, has := back["document_forms"]; has {
		t.Fatalf("plain model leaked document_forms: %s", raw)
	}
}

type mapTranslator map[string]string

func (m mapTranslator) Translate(_ context.Context, key string, _ ...any) string {
	if v, ok := m[key]; ok {
		return v
	}
	return key
}

// The localized transformer translates the i18n keys of cards, fields and the
// lines step, and never mutates the model's own (shared) definition.
func TestGetTable_LocalizesDocumentForms(t *testing.T) {
	key := registerDoc(t, func(k string) modelbase.ModelDefiner { return &docModel{fakeModel{key: k, title: "Docs"}} })
	tr := mapTranslator{"docs.invoice": "Factura", "docs.invoice_field": "Factura pagada", "docs.lines": "Partidas"}
	svc := New(Config{CacheTTL: -1}).WithTableTransformer(NewLocalizedTableTransformer(tr, "docs."))
	meta, err := svc.GetTable(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	ty := meta.DocumentForms.Types[0]
	if ty.Label != "Factura" || ty.Fields[0].Label != "Factura pagada" || ty.Lines.Title != "Partidas" {
		t.Fatalf("not localized: %+v / %+v / %+v", ty.Label, ty.Fields[0].Label, ty.Lines)
	}
	if got := (&docModel{}).DefineDocumentForms().Types[0].Label; got != "docs.invoice" {
		t.Fatalf("model definition mutated: %q", got)
	}
}
