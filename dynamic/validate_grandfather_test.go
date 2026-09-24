package dynamic

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/asteby/metacore-kernel/manifest"
	"github.com/asteby/metacore-kernel/metadata"
	"github.com/asteby/metacore-kernel/modelbase"
)

// QA LIVE-19 (pitsline 2026-09-22): adding a declarative rule (an RFC regex, a
// price min:0) over a table that already holds non-conforming rows made those
// rows UNEDITABLE: the edit form posts every field back, so renaming a customer
// failed 422 on the legacy tax_id the operator never touched. A value re-sent
// unchanged is grandfathered; changing it, or creating, is still validated.
func TestValidate_UnchangedLegacyValueIsGrandfatheredOnUpdate(t *testing.T) {
	zero := 0.0
	db := setupValidationDB(t)
	modelbase.Register("val_items", func() modelbase.ModelDefiner { return &ValItem{} })
	svc := New(Config{
		DB:       db,
		Metadata: metadata.New(metadata.Config{CacheTTL: -1}),
		ValidationSchemaResolver: func(_ context.Context, model string) ([]manifest.ColumnDef, bool) {
			if model != "val_items" {
				return nil, false
			}
			return []manifest.ColumnDef{
				{Name: "name", Type: "string", Required: true},
				{Name: "sku", Type: "string", Validation: &manifest.ValidationRule{Regex: `^[A-Z]+$`}},
				{Name: "qty", Type: "int", Validation: &manifest.ValidationRule{Min: &zero}},
			}, true
		},
	})
	user := newUser(uuid.New())
	ctx := context.Background()

	created, err := svc.Create(ctx, "val_items", user, map[string]any{"name": "Legacy", "sku": "OK", "qty": 1})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	id := uuid.MustParse(created["id"].(string))
	// Legacy data written before the rules existed.
	db.Exec(`UPDATE val_items SET sku = ?, qty = ? WHERE id = ?`, "bad-1", -5, id.String())

	// The edit form re-sends the untouched legacy values: must pass.
	if _, err := svc.Update(ctx, "val_items", user, id, map[string]any{
		"name": "Renamed", "sku": "bad-1", "qty": "-5",
	}); err != nil {
		t.Fatalf("re-sending unchanged legacy values must not fail validation: %v", err)
	}

	// Changing the value to another invalid one is still rejected.
	_, err = svc.Update(ctx, "val_items", user, id, map[string]any{"sku": "bad-2", "qty": -6})
	if codes := fieldCodes(t, err, "sku"); !hasCode(codes, "regex") {
		t.Fatalf("changed sku must fail regex, got %v (%v)", codes, err)
	}
	if codes := fieldCodes(t, err, "qty"); !hasCode(codes, "min") {
		t.Fatalf("changed qty must fail min, got %v (%v)", codes, err)
	}

	// Create never grandfathers.
	_, err = svc.Create(ctx, "val_items", user, map[string]any{"name": "New", "sku": "bad-1"})
	if codes := fieldCodes(t, err, "sku"); !hasCode(codes, "regex") {
		t.Fatalf("create must fail regex, got %v (%v)", codes, err)
	}
}

func TestUnchangedFromPersisted(t *testing.T) {
	before := map[string]any{"price": "12.50", "tax_id": "abc-1 ", "n": float64(3), "nil": nil}
	cases := []struct {
		name string
		raw  any
		want bool
	}{
		{"price", 12.5, true},
		{"price", "12.5", true},
		{"price", 12.51, false},
		{"tax_id", "abc-1", true},
		{"tax_id", "ABC-1", false},
		{"n", 3, true},
		{"missing", "x", false},
		{"nil", nil, true},
		{"nil", "x", false},
	}
	for _, c := range cases {
		if got := unchangedFromPersisted(c.raw, before, c.name); got != c.want {
			t.Errorf("unchangedFromPersisted(%v, %s) = %v, want %v", c.raw, c.name, got, c.want)
		}
	}
	if unchangedFromPersisted("x", nil, "tax_id") {
		t.Error("nil before (create) is never unchanged")
	}
}
