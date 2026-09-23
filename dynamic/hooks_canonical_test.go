package dynamic

import (
	"context"
	"testing"

	"github.com/asteby/metacore-kernel/manifest"
)

func quoteCanon(m string) string {
	switch m {
	case "quote_items", "QuoteItem":
		return "QuoteItem"
	case "quotes", "Quote":
		return "Quote"
	}
	return ""
}

// QA 0922: a model is addressed by its ModelKey AND its table alias. Hooks
// keyed by the literal name fired for one and not the other (a quote line
// posted to /data/quote_items skipped every formula and rollup). With a
// canonicalizer, both names share ONE set of hooks.
func TestHookRegistry_CanonicalizerSharesHooksAcrossAliases(t *testing.T) {
	r := NewHookRegistry()
	r.SetModelCanonicalizer(quoteCanon)

	fired := 0
	r.RegisterBeforeCreate("QuoteItem", func(context.Context, HookContext, map[string]any) error { fired++; return nil })
	for _, name := range []string{"QuoteItem", "quote_items"} {
		if err := r.runBeforeCreate(context.Background(), HookContext{Model: name}, map[string]any{}); err != nil {
			t.Fatal(err)
		}
	}
	if fired != 2 {
		t.Fatalf("hook registered under QuoteItem must fire for both names, fired=%d", fired)
	}

	// Registered under the alias -> fires for the canonical name too, once.
	fired = 0
	r2 := NewHookRegistry()
	r2.SetModelCanonicalizer(quoteCanon)
	r2.RegisterAfterCreate("quote_items", func(context.Context, HookContext, any) error { fired++; return nil })
	_ = r2.runAfterCreate(context.Background(), HookContext{Model: "QuoteItem"}, nil)
	if fired != 1 {
		t.Fatalf("fired=%d", fired)
	}
}

func TestHookRegistry_CanonicalizerRollupIndex(t *testing.T) {
	r := NewHookRegistry()
	r.SetModelCanonicalizer(quoteCanon)
	RegisterComputeHooks(r, manifest.Manifest{Key: "q", ModelDefinitions: []manifest.ModelDefinition{
		{ModelKey: "Quote", TableName: "quotes", Relations: []manifest.RelationDef{{
			Name: "items", Kind: "one_to_many", Through: "QuoteItem", ForeignKey: "quote_id",
			Rollups: []manifest.Rollup{{Target: "total", Fn: "sum", From: "line_total"}},
		}}},
		{ModelKey: "QuoteItem", TableName: "quote_items", Formulas: []manifest.Formula{{Target: "line_total", Expr: "quantity * unit_price"}}},
	}})
	if !r.HasRollupsForChild("quote_items") || !r.HasRollupsForChild("QuoteItem") {
		t.Fatal("rollups over QuoteItem must be found by either name")
	}
	in := map[string]any{"quantity": 4.0, "unit_price": 2000.0}
	if err := r.runBeforeCreate(context.Background(), HookContext{Model: "quote_items"}, in); err != nil {
		t.Fatal(err)
	}
	if in["line_total"] != 8000.0 {
		t.Fatalf("formula via alias: line_total=%v", in["line_total"])
	}
}

// Without a canonicalizer nothing changes (names stay literal).
func TestHookRegistry_NoCanonicalizerIsLiteral(t *testing.T) {
	r := NewHookRegistry()
	fired := 0
	r.RegisterBeforeCreate("QuoteItem", func(context.Context, HookContext, map[string]any) error { fired++; return nil })
	_ = r.runBeforeCreate(context.Background(), HookContext{Model: "quote_items"}, map[string]any{})
	if fired != 0 {
		t.Fatal("without a canonicalizer an alias must not match")
	}
}
