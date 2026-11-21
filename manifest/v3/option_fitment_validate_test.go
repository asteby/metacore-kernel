package v3

import (
	"encoding/json"
	"strings"
	"testing"
)

func validateFitment(t *testing.T, mutate func(col im)) error {
	t.Helper()
	m := baseValid()
	models := snapModels()
	col := models[0].(im)["columns"].([]interface{})[1].(im) // product_id (ref)
	col["option_fitment"] = im{"subject_field": "vehicle_id", "subject_model": "Vehicle", "target_model": "TireSpec"}
	if mutate != nil {
		mutate(col)
	}
	m["models"] = models
	return Validate(mustJSON(t, m))
}

func TestValidate_OptionFitment_Valid(t *testing.T) {
	for _, mode := range []string{"", "flag", "filter"} {
		err := validateFitment(t, func(col im) {
			if mode != "" {
				col["option_fitment"].(im)["mode"] = mode
			}
		})
		if err != nil {
			t.Errorf("mode %q rejected: %v", mode, err)
		}
	}
}

func TestValidate_OptionFitment_Rejections(t *testing.T) {
	cases := map[string]struct {
		mutate func(col im)
		want   string
	}{
		"sin subject_field": {func(col im) { delete(col["option_fitment"].(im), "subject_field") }, "subject_field"},
		"sin subject_model": {func(col im) { delete(col["option_fitment"].(im), "subject_model") }, "subject_model"},
		"sin target_model":  {func(col im) { delete(col["option_fitment"].(im), "target_model") }, "target_model"},
		"modo desconocido":  {func(col im) { col["option_fitment"].(im)["mode"] = "hide" }, "mode"},
		"clave desconocida": {func(col im) { col["option_fitment"].(im)["extra"] = true }, "extra"},
		"identificador":     {func(col im) { col["option_fitment"].(im)["subject_field"] = "a b" }, "subject_field"},
		"columna sin picker": {func(col im) {
			delete(col, "ref")
		}, "needs a picker column"},
	}
	for name, c := range cases {
		err := validateFitment(t, c.mutate)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want it to mention %q", name, err, c.want)
		}
	}
}

func TestOptionFitmentRoundTrip(t *testing.T) {
	var c Column
	in := `{"name":"product_id","type":"uuid","ref":"products","option_fitment":{"subject_field":"vehicle_id","subject_model":"Vehicle","target_model":"TireSpec","mode":"filter"}}`
	if err := json.Unmarshal([]byte(in), &c); err != nil {
		t.Fatal(err)
	}
	if c.OptionFitment == nil || c.OptionFitment.Mode != OptionFitmentFilter || c.OptionFitment.SubjectModel != "Vehicle" {
		t.Fatalf("decoded = %+v", c.OptionFitment)
	}
	out, _ := json.Marshal(c)
	if !strings.Contains(string(out), `"option_fitment":{"subject_field":"vehicle_id","subject_model":"Vehicle","target_model":"TireSpec","mode":"filter"}`) {
		t.Errorf("round-trip lost it: %s", out)
	}
	var none Column
	_ = json.Unmarshal([]byte(`{"name":"x","type":"text"}`), &none)
	if b, _ := json.Marshal(none); strings.Contains(string(b), "option_fitment") {
		t.Errorf("absent field must not be emitted: %s", b)
	}
}
