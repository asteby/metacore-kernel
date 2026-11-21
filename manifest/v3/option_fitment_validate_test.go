package v3

import (
	"encoding/json"
	"strings"
	"testing"
)

// fitmentModels: a document header (SalesOrder, with the subject column
// asset_id) and its line model (SalesOrderItem, product_id picker).
func fitmentModels() []interface{} {
	line := snapModels()[0].(im)
	line["columns"].([]interface{})[1].(im)["option_fitment"] = im{"subject_field": "asset_id", "subject_model": "Subject", "target_model": "TargetSpec"}
	line["columns"] = append(line["columns"].([]interface{}), im{"name": "order_id", "type": "uuid", "ref": "orders"})
	line["rules"] = []interface{}{im{"kind": "ref_state", "ref": "order_id", "parent": "SalesOrder", "require": im{"status": []interface{}{"draft"}}, "error_key": "k"}}
	header := im{"key": "SalesOrder", "table": "sales_orders",
		"columns": []interface{}{
			im{"name": "organization_id", "type": "uuid", "not_null": true},
			im{"name": "asset_id", "type": "uuid"},
		},
		"relations": []interface{}{im{"name": "items", "kind": "one_to_many", "through": "SalesOrderItem", "foreign_key": "order_id", "embed": true}},
	}
	return []interface{}{header, line}
}

func fitmentSpec(models []interface{}) im {
	return models[1].(im)["columns"].([]interface{})[1].(im)["option_fitment"].(im)
}

func validateFitmentModels(t *testing.T, mutate func(models []interface{})) ([]string, error) {
	t.Helper()
	m := baseValid()
	models := fitmentModels()
	if mutate != nil {
		mutate(models)
	}
	m["models"] = models
	return ValidateWithOptions(mustJSON(t, m), Options{})
}

func TestValidate_OptionFitment_Valid(t *testing.T) {
	for _, mode := range []string{"", "flag", "filter"} {
		_, err := validateFitmentModels(t, func(ms []interface{}) {
			if mode != "" {
				fitmentSpec(ms)["mode"] = mode
			}
		})
		if err != nil {
			t.Errorf("mode %q rejected: %v", mode, err)
		}
	}
}

func TestValidate_OptionFitment_Rejections(t *testing.T) {
	col := func(ms []interface{}) im { return ms[1].(im)["columns"].([]interface{})[1].(im) }
	cases := map[string]struct {
		mutate func(ms []interface{})
		want   string
	}{
		"sin subject_field": {func(ms []interface{}) { delete(fitmentSpec(ms), "subject_field") }, "subject_field"},
		"sin subject_model": {func(ms []interface{}) { delete(fitmentSpec(ms), "subject_model") }, "subject_model"},
		"sin target_model":  {func(ms []interface{}) { delete(fitmentSpec(ms), "target_model") }, "target_model"},
		"modo desconocido":  {func(ms []interface{}) { fitmentSpec(ms)["mode"] = "hide" }, "mode"},
		"clave desconocida": {func(ms []interface{}) { fitmentSpec(ms)["extra"] = true }, "extra"},
		"identificador":     {func(ms []interface{}) { fitmentSpec(ms)["subject_field"] = "a b" }, "subject_field"},
		"columna sin picker": {func(ms []interface{}) {
			delete(col(ms), "ref")
		}, "needs a picker column"},
		"options estaticos no es un picker": {func(ms []interface{}) {
			delete(col(ms), "ref")
			col(ms)["options"] = []interface{}{im{"value": "a", "label": "A"}}
		}, "needs a picker column"},
		"options_source no es un picker": {func(ms []interface{}) {
			delete(col(ms), "ref")
			col(ms)["options_source"] = "installed_addons"
		}, "needs a picker column"},
		"subject_field con typo (cabecera local sin esa columna)": {func(ms []interface{}) {
			fitmentSpec(ms)["subject_field"] = "asset_idd"
		}, "asset_idd"},
	}
	for name, c := range cases {
		_, err := validateFitmentModels(t, c.mutate)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want it to mention %q", name, err, c.want)
		}
	}
}

func TestValidate_OptionFitment_SubjectFieldResolution(t *testing.T) {
	// a column of the line itself resolves
	if _, err := validateFitmentModels(t, func(ms []interface{}) { fitmentSpec(ms)["subject_field"] = "quantity" }); err != nil {
		t.Errorf("own column rejected: %v", err)
	}
	// a standalone model (no header, not a line) with a typo is an error
	_, err := validateFitmentModels(t, func(ms []interface{}) {
		ms[1].(im)["rules"] = []interface{}{}
		ms[0].(im)["relations"] = []interface{}{}
		fitmentSpec(ms)["subject_field"] = "asset_id"
	})
	if err == nil || !strings.Contains(err.Error(), "asset_id") {
		t.Errorf("standalone model with unknown subject_field must fail, got %v", err)
	}
	// a line whose header lives elsewhere (no local relation): warning, not error
	warns, err := validateFitmentModels(t, func(ms []interface{}) {
		ms[0].(im)["relations"] = []interface{}{}
	})
	if err != nil {
		t.Fatalf("header in another addon must not fail: %v", err)
	}
	if !strings.Contains(strings.Join(warns, "|"), "header looks to live in another addon") {
		t.Errorf("expected a warning, got %v", warns)
	}
}

func TestValidate_OptionFitment_NonLocalModelsWarn(t *testing.T) {
	warns, err := validateFitmentModels(t, nil)
	if err != nil {
		t.Fatalf("valid manifest rejected: %v", err)
	}
	joined := strings.Join(warns, "|")
	if !strings.Contains(joined, `subject_model "Subject" is not a model of this manifest`) || !strings.Contains(joined, `target_model "TargetSpec" is not a model of this manifest`) {
		t.Errorf("cross-addon models must warn, got %v", warns)
	}
	// a local model name resolves silently
	warns, err = validateFitmentModels(t, func(ms []interface{}) {
		fitmentSpec(ms)["subject_model"] = "SalesOrder"
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(warns, "|"), "subject_model") {
		t.Errorf("a local subject_model must not warn: %v", warns)
	}
}

func TestValidate_OptionFitment_OnActionField(t *testing.T) {
	// shape only (an action form has no owning model to resolve against)
	field := im{"key": "pick", "type": "dynamic_select", "ref": "things", "option_fitment": im{"subject_field": "s", "subject_model": "Subject", "target_model": "TargetSpec"}}
	errs := validateActionFieldPickerMeta("w", func() ActionField {
		var f ActionField
		b, _ := json.Marshal(field)
		if err := json.Unmarshal(b, &f); err != nil {
			t.Fatal(err)
		}
		return f
	}(), nil)
	if len(errs) != 0 {
		t.Errorf("valid action-field fitment rejected: %v", errs)
	}
	delete(field["option_fitment"].(im), "target_model")
	var f ActionField
	b, _ := json.Marshal(field)
	_ = json.Unmarshal(b, &f)
	if errs := validateActionFieldPickerMeta("w", f, nil); len(errs) == 0 {
		t.Errorf("missing target_model must be rejected on an action field")
	}
	delete(field, "ref")
	b, _ = json.Marshal(field)
	_ = json.Unmarshal(b, &f)
	if errs := validateActionFieldPickerMeta("w", f, nil); len(errs) == 0 {
		t.Errorf("a non-picker action field must be rejected")
	}
}

func TestOptionFitmentRoundTrip(t *testing.T) {
	var c Column
	in := `{"name":"product_id","type":"uuid","ref":"products","option_fitment":{"subject_field":"asset_id","subject_model":"Subject","target_model":"TargetSpec","mode":"filter"}}`
	if err := json.Unmarshal([]byte(in), &c); err != nil {
		t.Fatal(err)
	}
	if c.OptionFitment == nil || c.OptionFitment.Mode != OptionFitmentFilter || c.OptionFitment.SubjectModel != "Subject" {
		t.Fatalf("decoded = %+v", c.OptionFitment)
	}
	out, _ := json.Marshal(c)
	if !strings.Contains(string(out), `"option_fitment":{"subject_field":"asset_id","subject_model":"Subject","target_model":"TargetSpec","mode":"filter"}`) {
		t.Errorf("round-trip lost it: %s", out)
	}
	var none Column
	_ = json.Unmarshal([]byte(`{"name":"x","type":"text"}`), &none)
	if b, _ := json.Marshal(none); strings.Contains(string(b), "option_fitment") {
		t.Errorf("absent field must not be emitted: %s", b)
	}
}
