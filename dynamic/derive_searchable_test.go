package dynamic

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/asteby/metacore-kernel/manifest"
	"github.com/asteby/metacore-kernel/modelbase"
)

func opts(n int) []manifest.Option {
	out := make([]manifest.Option, n)
	for i := range out {
		out[i] = manifest.Option{Value: fmt.Sprint(i), Label: fmt.Sprintf("Option %d", i)}
	}
	return out
}

func TestDeriveFormFieldsSearchableByOptionCount(t *testing.T) {
	def := manifest.ModelDefinition{Columns: []manifest.ColumnDef{
		{Name: "short", Type: "string", Options: opts(modelbase.SearchableSelectThreshold)},
		{Name: "long", Type: "string", Options: opts(modelbase.SearchableSelectThreshold + 1)},
		{Name: "plain", Type: "string"},
		{Name: "customer_id", Type: "uuid", Ref: "customers"},
	}}
	got := map[string]*bool{}
	for _, f := range DeriveFormFields(def) {
		got[f.Key] = f.Searchable
	}
	if got["short"] != nil {
		t.Errorf("short select (<= threshold) must stay nil, got %v", *got["short"])
	}
	if got["long"] == nil || !*got["long"] {
		t.Errorf("long select (> threshold) must be searchable=true")
	}
	if got["plain"] != nil || got["customer_id"] != nil {
		t.Errorf("non-select fields must not carry searchable")
	}
}

func TestSearchableForExplicitWins(t *testing.T) {
	f, tr := false, true
	if v := modelbase.SearchableFor(&f, 500); v == nil || *v {
		t.Errorf("explicit false must win over a long list")
	}
	if v := modelbase.SearchableFor(&tr, 2); v == nil || !*v {
		t.Errorf("explicit true must win over a short list")
	}
	if v := modelbase.SearchableFor(nil, 3); v != nil {
		t.Errorf("short list without explicit must be nil")
	}
}

func TestFieldDefSearchableJSONIsBackwardCompatible(t *testing.T) {
	b, _ := json.Marshal(modelbase.FieldDef{Key: "a", Type: "select"})
	if strings.Contains(string(b), "searchable") {
		t.Errorf("nil searchable must be omitted: %s", b)
	}
	f := false
	b, _ = json.Marshal(modelbase.FieldDef{Key: "a", Type: "select", Searchable: &f})
	if !strings.Contains(string(b), `"searchable":false`) {
		t.Errorf("explicit false must serialize: %s", b)
	}
	var back modelbase.FieldDef
	if err := json.Unmarshal([]byte(`{"key":"a","type":"select"}`), &back); err != nil || back.Searchable != nil {
		t.Errorf("legacy payload must decode with nil searchable (err=%v)", err)
	}
}
