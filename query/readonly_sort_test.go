package query

import (
	"testing"

	"github.com/asteby/metacore-kernel/modelbase"
)

// A read-only (view-backed) model does not whitelist created_at/updated_at and
// sorts by its id when nothing else applies.
func TestReadOnlyModelDefaultSort(t *testing.T) {
	meta := &modelbase.TableMetadata{ReadOnly: true, Columns: []modelbase.ColumnDef{{Key: "id"}, {Key: "name"}}}
	b := New(meta)
	if _, ok := b.allowed["created_at"]; ok {
		t.Fatal("created_at must not be whitelisted on a read-only model")
	}
	if col, order := b.defaultSort(); col != "id" || order != "asc" {
		t.Fatalf("default sort = %q %q", col, order)
	}
	plain := New(&modelbase.TableMetadata{Columns: []modelbase.ColumnDef{{Key: "name"}}})
	if col, _ := plain.defaultSort(); col != "created_at" {
		t.Fatalf("ordinary model keeps created_at, got %q", col)
	}
}
