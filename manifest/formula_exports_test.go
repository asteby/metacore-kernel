package manifest_test

import (
	"testing"

	"github.com/asteby/metacore-kernel/manifest"
	v3 "github.com/asteby/metacore-kernel/manifest/v3"
)

// A Tier-3 formula handler must land in the backend whitelist: otherwise the
// host refuses to invoke it ("not in backend.exports"), the formula yields no
// value and the target column is left unset (QA 0922: customers
// SalesOrderItem.unit_price -> every POST /data/SalesOrderItem failed 422).
func TestFromV3_Tier3FormulaHandlerIsExported(t *testing.T) {
	m := &v3.Manifest{
		Models: []v3.Model{{
			Formulas: []v3.Formula{
				{Target: "unit_price", Tier: 3, Handler: "wasm:resolve_line_price"},
				{Target: "subtotal", Expr: "quantity * unit_price"},
			},
		}},
	}
	out := manifest.FromV3(m)
	if out.Backend == nil {
		t.Fatal("expected a wasm Backend derived from the Tier-3 formula handler")
	}
	for _, e := range out.Backend.Exports {
		if e == "resolve_line_price" {
			return
		}
	}
	t.Fatalf("Backend.Exports %v missing the Tier-3 formula export resolve_line_price", out.Backend.Exports)
}
