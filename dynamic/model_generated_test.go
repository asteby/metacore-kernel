package dynamic

import (
	"strings"
	"testing"

	"github.com/asteby/metacore-kernel/manifest"
)

// A Postgres STORED generated column (e.g. inventory stock `available =
// quantity - reserved`) is maintained by the database on every write. The
// reflect model field for it MUST carry GORM's read-only permission ("->") so
// the column is excluded from INSERT/UPDATE — otherwise GORM sends the field's
// zero value in the column list and Postgres rejects the write with "cannot
// insert a non-DEFAULT value into column". It must still be readable (present in
// SELECT/scan) so it surfaces in list/detail/export.
func TestColumnToField_GeneratedIsReadOnly(t *testing.T) {
	field, err := columnToField(manifest.ColumnDef{
		Name:      "available",
		Type:      "numeric",
		Generated: "quantity - reserved",
	})
	if err != nil {
		t.Fatalf("columnToField(generated): %v", err)
	}
	gormTag := field.Tag.Get("gorm")
	if !strings.Contains(gormTag, "->") {
		t.Fatalf("generated column must be read-only; gorm tag = %q, want it to contain \"->\"", gormTag)
	}
	// It must never be declared NOT NULL / DEFAULT / index — Postgres rejects
	// those on a generated column and validation forbids them upstream.
	for _, forbidden := range []string{"not null", "default:", "index"} {
		if strings.Contains(gormTag, forbidden) {
			t.Fatalf("generated column gorm tag must not contain %q; got %q", forbidden, gormTag)
		}
	}
}

// A non-generated column keeps write permission (no "->"), so ordinary columns
// are still inserted/updated as before.
func TestColumnToField_OrdinaryIsWritable(t *testing.T) {
	field, err := columnToField(manifest.ColumnDef{Name: "quantity", Type: "numeric"})
	if err != nil {
		t.Fatalf("columnToField(ordinary): %v", err)
	}
	if strings.Contains(field.Tag.Get("gorm"), "->") {
		t.Fatalf("ordinary column must stay writable; got read-only gorm tag %q", field.Tag.Get("gorm"))
	}
}
