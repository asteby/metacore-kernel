package manifest

import (
	"testing"

	"github.com/asteby/metacore-kernel/manifest/v3"
)

// enforce:"always" must survive the v3 -> host projection: the dynamic Service
// and the wasm data_mutate path read CrossRuleDef, never the v3 struct.
func TestFromV3CrossRuleCarriesEnforce(t *testing.T) {
	m := &v3.Manifest{Models: []v3.Model{{
		Key: "QuoteItem", Table: "quote_items",
		Columns: []v3.Column{{Name: "quote_id", Type: "uuid"}},
		Rules: []v3.CrossRule{{Kind: "ref_state", ErrorKey: "quotes.locked", Ref: "quote_id", Parent: "Quote",
			Require: map[string]any{"status": []any{"draft", "sent"}}, Enforce: "always"}},
	}}}
	def := FromV3(m).ModelDefinitions[0]
	if len(def.Rules) != 1 || def.Rules[0].Enforce != "always" {
		t.Fatalf("enforce not carried: %+v", def.Rules)
	}
}
