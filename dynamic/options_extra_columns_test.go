package dynamic

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"

	"github.com/asteby/metacore-kernel/modelbase"
)

// optInvoice is a relation target with a status column, the case an
// option_filter needs (hide cancelled invoices in a payments picker).
type optInvoice struct {
	ID     uuid.UUID `json:"id" gorm:"type:uuid;primaryKey"`
	Folio  string    `json:"folio"`
	Status string    `json:"status"`
	Total  float64   `json:"total"`
	Paid   bool      `json:"paid"`
	Note   *string   `json:"note"`
}

func (optInvoice) TableName() string { return "test_opt_invoices" }
func (optInvoice) DefineTable() modelbase.TableMetadata {
	return modelbase.TableMetadata{Title: "Invoices"}
}
func (optInvoice) DefineModal() modelbase.ModalMetadata {
	return modelbase.ModalMetadata{Title: "Invoices"}
}

func setupOptInvoices(t *testing.T) *Service {
	t.Helper()
	db := setupTestDB(t)
	db.Exec(`CREATE TABLE IF NOT EXISTS test_opt_invoices (
		id TEXT PRIMARY KEY, folio TEXT, status TEXT, total REAL, paid BOOLEAN, note TEXT)`)
	modelbase.Register("test_opt_invoices", func() modelbase.ModelDefiner { return &optInvoice{} })
	note := "n1"
	db.Exec(`INSERT INTO test_opt_invoices (id, folio, status, total, paid, note) VALUES (?,?,?,?,?,?),(?,?,?,?,?,?)`,
		uuid.NewString(), "FAC-1", "vigente", 100.5, true, note,
		uuid.NewString(), "FAC-2", "cancelada", 20.0, false, nil)

	return newOptionsService(t, db, optionsConfigFor(OptionsConfig{
		Fields: map[string]FieldOptionsConfig{
			"invoice_id": {
				Type:         "dynamic",
				Source:       "test_opt_invoices",
				Value:        "id",
				Label:        "folio",
				OrderBy:      "folio",
				ExtraColumns: []string{"status", "total", "paid", "note", "id", "label", "bad name", "missing_col", "status"},
			},
			"plain_id": {
				Type:   "dynamic",
				Source: "test_opt_invoices",
				Value:  "id",
				Label:  "folio",
			},
		},
	}), nil)
}

// TestOptionsExtraColumns: the manifest asks for extra columns and every option
// carries them as sibling JSON keys (where the SDK's option_filter reads them).
func TestOptionsExtraColumns(t *testing.T) {
	svc := setupOptInvoices(t)
	res, err := svc.Options(context.Background(), nil, OptionsQuery{Model: "test_opt_invoices", Field: "invoice_id"})
	if err != nil {
		t.Fatalf("options: %v", err)
	}
	if len(res.Options) != 2 {
		t.Fatalf("got %d options, want 2", len(res.Options))
	}
	raw, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	var body struct {
		Type    string           `json:"type"`
		Options []map[string]any `json:"options"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	first, second := body.Options[0], body.Options[1]
	if first["label"] != "FAC-1" || first["status"] != "vigente" {
		t.Errorf("first option = %v, want label FAC-1 / status vigente", first)
	}
	if first["total"] != 100.5 || first["paid"] != true || first["note"] != "n1" {
		t.Errorf("first option scalars = %v, want total 100.5, paid true, note n1", first)
	}
	if second["status"] != "cancelada" || second["paid"] != false {
		t.Errorf("second option = %v, want status cancelada, paid false", second)
	}
	if _, ok := second["note"]; ok {
		t.Errorf("a NULL extra column must be omitted, got note=%v", second["note"])
	}
	// Reserved / unsafe / unknown names are ignored, never overriding id/label.
	if first["label"] == nil || first["id"] == nil {
		t.Errorf("reserved keys lost: %v", first)
	}
	if _, ok := first["bad name"]; ok {
		t.Errorf("unsafe extra column leaked: %v", first)
	}
	if _, ok := first["missing_col"]; ok {
		t.Errorf("unknown extra column must be omitted: %v", first)
	}
}

// TestOptionsWithoutExtraColumnsUnchanged: retro-compat — no extra_columns, no
// extra keys; the payload is byte-identical to the pre-feature shape.
func TestOptionsWithoutExtraColumnsUnchanged(t *testing.T) {
	svc := setupOptInvoices(t)
	res, err := svc.Options(context.Background(), nil, OptionsQuery{Model: "test_opt_invoices", Field: "plain_id"})
	if err != nil {
		t.Fatalf("options: %v", err)
	}
	for _, o := range res.Options {
		if o.Extra != nil {
			t.Errorf("Extra = %v, want nil", o.Extra)
		}
		raw, _ := json.Marshal(o)
		var m map[string]any
		_ = json.Unmarshal(raw, &m)
		for k := range m {
			switch k {
			case "id", "value", "label", "name":
			default:
				t.Errorf("unexpected key %q in a plain option: %s", k, raw)
			}
		}
	}
}

// TestOptionMarshalExtraNeverOverridesDeclared: an Extra entry named like a
// declared key is dropped from the output; the declared value wins.
func TestOptionMarshalExtraNeverOverridesDeclared(t *testing.T) {
	o := Option{ID: "1", Value: "1", Label: "L", Name: "L", Extra: map[string]any{"label": "evil", "status": "ok", "zeta": 1, "alpha": true}}
	raw, err := json.Marshal(o)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"id":"1","value":"1","label":"L","name":"L","alpha":true,"status":"ok","zeta":1}`
	if string(raw) != want {
		t.Errorf("got  %s\nwant %s", raw, want)
	}
}

func TestStaticOptionsHaveNoExtra(t *testing.T) {
	out := renderStatic([]StaticOption{{Value: "a", Label: "A"}})
	raw, _ := json.Marshal(out[0])
	if string(raw) != `{"id":"a","value":"a","label":"A","name":"A"}` {
		t.Errorf("static option shape changed: %s", raw)
	}
}
