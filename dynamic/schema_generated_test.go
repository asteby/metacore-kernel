package dynamic

import (
	"strings"
	"testing"

	"github.com/asteby/metacore-kernel/manifest"
)

// TestColumnDDL_Generated asserts the CREATE TABLE fragment for a STORED
// generated column: `GENERATED ALWAYS AS (<expr>) STORED`, no NOT NULL / DEFAULT.
func TestColumnDDL_Generated(t *testing.T) {
	c := manifest.ColumnDef{Name: "available", Type: "bigint", Generated: "quantity - reserved"}
	got, err := columnDDL(c)
	if err != nil {
		t.Fatalf("columnDDL: %v", err)
	}
	want := `"available" bigint GENERATED ALWAYS AS (("quantity" - "reserved")) STORED`
	if got != want {
		t.Fatalf("columnDDL mismatch:\nwant: %s\ngot:  %s", want, got)
	}
	if strings.Contains(got, "NOT NULL") || strings.Contains(got, "DEFAULT") {
		t.Fatalf("generated column must not carry NOT NULL/DEFAULT: %s", got)
	}
}

// TestColumnDDL_Plain asserts the non-generated path still emits NOT NULL/DEFAULT.
func TestColumnDDL_Plain(t *testing.T) {
	c := manifest.ColumnDef{Name: "quantity", Type: "bigint", Required: true, Default: 0}
	got, err := columnDDL(c)
	if err != nil {
		t.Fatalf("columnDDL: %v", err)
	}
	if !strings.Contains(got, "NOT NULL") || !strings.Contains(got, "DEFAULT 0") {
		t.Fatalf("expected NOT NULL DEFAULT 0, got: %s", got)
	}
}

// TestAddColumnDDL_Generated asserts the SyncSchema ALTER TABLE fragment.
func TestAddColumnDDL_Generated(t *testing.T) {
	c := manifest.ColumnDef{Name: "available", Type: "bigint", Generated: "quantity - reserved"}
	got, err := addColumnDDL("addon_stock", "stock_levels", c)
	if err != nil {
		t.Fatalf("addColumnDDL: %v", err)
	}
	want := `ALTER TABLE "addon_stock"."stock_levels" ADD COLUMN IF NOT EXISTS "available" bigint GENERATED ALWAYS AS (("quantity" - "reserved")) STORED`
	if got != want {
		t.Fatalf("addColumnDDL mismatch:\nwant: %s\ngot:  %s", want, got)
	}
}

// TestColumnDDL_GeneratedInjection: a malformed/injecting expression is rejected
// by RenderSQL (the strict computeexpr grammar) and surfaces as an error.
func TestColumnDDL_GeneratedInjection(t *testing.T) {
	c := manifest.ColumnDef{Name: "available", Type: "bigint", Generated: "quantity); DROP TABLE x; --"}
	if _, err := columnDDL(c); err == nil {
		t.Fatalf("expected error for injecting generated expr, got nil")
	}
}
