package v3

import (
	"encoding/json"
	"testing"
)

// Los campos del DocumentEditor deben sobrevivir al round-trip: el kernel sirve
// la metadata desde estas structs, así que un campo sin tag se pierde camino
// al SDK (patches 07/09 del plan Pitsline).
func TestDocumentFormType_EditorFieldsRoundTrip(t *testing.T) {
	raw := []byte(`{"key":"payment","label":"Cobro","fields":[{"key":"invoice_id","type":"dynamic_select","default_from_record":"id"}],
	 "layout":"editor","submit_action":"collect_multi_payment_create",
	 "party":{"field":"customer_id","model":"customers.Customer","credit":{"limit":"credit_limit"}},
	 "sources":[{"key":"quote","label":"Cotización","model":"quotes.Quote","lines":"items","option_filter":[{"field":"status","equals":"accepted"}]}],
	 "preview":{"action":"preview_rep","requires_addon":"fiscal_mexico"},
	 "lines":{"kind":"allocation","discount_mode":"amount","open_documents":{"model":"customers.Invoice","party_field":"customer_id","balance_field":"amount_due","number_field":"number","line_document_field":"invoice_id","line_amount_field":"amount"}}}`)
	var dt DocumentFormType
	if err := json.Unmarshal(raw, &dt); err != nil {
		t.Fatal(err)
	}
	if dt.Layout != "editor" || dt.Party == nil || dt.Party.Credit["limit"] != "credit_limit" || len(dt.Sources) != 1 || dt.Preview.Action != "preview_rep" || dt.SubmitAction == "" {
		t.Fatalf("editor fields lost: %+v", dt)
	}
	if !dt.Lines.Enabled() || dt.Lines.Kind != "allocation" || dt.Lines.OpenDocuments == nil || dt.Lines.DiscountMode != "amount" {
		t.Fatalf("lines fields lost: %+v", dt.Lines)
	}
	if dt.Fields[0].DefaultFromRecord != "id" {
		t.Fatalf("default_from_record lost: %+v", dt.Fields[0])
	}
	out, _ := json.Marshal(dt)
	var back map[string]any
	_ = json.Unmarshal(out, &back)
	if back["layout"] != "editor" || back["submit_action"] == nil {
		t.Fatalf("marshal dropped fields: %s", out)
	}
}

// A lines step with only kind/discount_mode/open_documents must re-marshal as
// an object: collapsing it to `true` dropped them on the way to the SDK.
func TestDocumentFormLines_EditorOptionsMarshalAsObject(t *testing.T) {
	for _, l := range []DocumentFormLines{
		{Kind: "allocation"},
		{DiscountMode: "amount"},
		{OpenDocuments: &DocumentFormOpenDocuments{Model: "customers.Invoice"}},
	} {
		out, err := json.Marshal(l)
		if err != nil {
			t.Fatal(err)
		}
		if string(out) == "true" {
			t.Errorf("%+v collapsed to true", l)
		}
	}
	if out, _ := json.Marshal(DocumentFormLines{}); string(out) != "true" {
		t.Errorf("option-less lines = %s, want true", out)
	}
}
