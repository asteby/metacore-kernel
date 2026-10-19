package audit

import (
	"context"
	"strings"
	"testing"
)

func TestRedactEntries_HidesGatedColumns(t *testing.T) {
	s := func(v string) *string { return &v }
	entries := []Entry{{
		Model:   "products",
		Before:  s(`{"id":"1","price":10,"cost":7}`),
		After:   s(`{"id":"1","price":12,"cost":8}`),
		Changes: s(`{"price":{"before":10,"after":12},"cost":{"before":7,"after":8}}`),
	}, {Model: "products", After: s(`not json`)}}
	var models []string
	RedactEntries(context.Background(), entries, func(_ context.Context, model string, row map[string]any) map[string]any {
		models = append(models, model)
		delete(row, "cost")
		return row
	})
	for _, p := range []*string{entries[0].Before, entries[0].After, entries[0].Changes} {
		if p == nil || strings.Contains(*p, "cost") || !strings.Contains(*p, "price") {
			t.Fatalf("redacted part: %v", p)
		}
	}
	if entries[1].After != nil {
		t.Fatal("an unparseable part must be dropped (fail closed)")
	}
	if len(models) != 3 || models[0] != "products" {
		t.Fatalf("redactor calls: %v", models)
	}
	// nil redactor leaves entries untouched.
	RedactEntries(context.Background(), entries, nil)
}
