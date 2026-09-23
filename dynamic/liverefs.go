package dynamic

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/asteby/metacore-kernel/manifest"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// ErrDeletedRef marks a create whose ref (FK) column points at a row that
// still exists but is soft-deleted (deleted_at set). Selling a deleted product
// is the motivating case (QA 7Leguas VEN-N08): the sale line went through,
// stock moved and a warranty opened for an item no longer in the catalog.
var ErrDeletedRef = errors.New("reference to a deleted record")

var softDeleteCols sync.Map // table → bool (has deleted_at)

// tableHasDeletedAt reports (cached per table) whether a table carries the
// soft-delete column. Unknown / unsafe names report false.
func tableHasDeletedAt(db *gorm.DB, table string) bool {
	if v, ok := softDeleteCols.Load(table); ok {
		return v.(bool)
	}
	has := db.Migrator().HasColumn(table, "deleted_at")
	softDeleteCols.Store(table, has)
	return has
}

var orgScopedTables sync.Map // table → bool (has organization_id)

func tableHasOrgID(db *gorm.DB, table string) bool {
	if v, ok := orgScopedTables.Load(table); ok {
		return v.(bool)
	}
	has := db.Migrator().HasColumn(table, "organization_id")
	orgScopedTables.Store(table, has)
	return has
}

// CheckNoDeletedRefs rejects a CREATE row whose ref columns that opt in with
// reject_deleted_ref point at soft-deleted rows of the ref target (scoped to orgID when the target is
// tenant-scoped). Only DELETED targets fail: a ref to an id the table does not
// hold at all is left to the caller's own rules (the wasm tier writes
// cross-addon ids the kernel cannot always resolve). Hosts wire it into
// wasm.Host.WithCreateCheck so data_mutate / data_batch creates get the same
// protection Service.Create gives through validateWrite. The error wraps
// ErrDeletedRef and names every offending column.
func CheckNoDeletedRefs(ctx context.Context, tx *gorm.DB, orgID uuid.UUID, cols []manifest.ColumnDef, row map[string]any) error {
	var bad []string
	for _, col := range cols {
		if col.Ref == "" || !col.RejectDeletedRef {
			continue
		}
		raw, present := row[col.Name]
		if !present || raw == nil {
			continue
		}
		table, ok := refTable(col.Ref)
		if !ok || !tableHasDeletedAt(tx, table) {
			continue
		}
		ids, multi := refIDs(raw)
		if !multi {
			if id := valueToString(raw); id != "" {
				ids = []string{id}
			}
		}
		if len(ids) == 0 {
			continue
		}
		q := tx.WithContext(ctx).Table(table).Where("id IN ?", ids).Where("deleted_at IS NOT NULL")
		if orgID != uuid.Nil && tableHasOrgID(tx, table) {
			q = q.Where("organization_id = ?", orgID)
		}
		var n int64
		if err := q.Count(&n).Error; err != nil {
			return fmt.Errorf("dynamic: deleted-ref check %s.%s: %w", table, col.Name, err)
		}
		if n > 0 {
			bad = append(bad, col.Name+" → "+table)
		}
	}
	if len(bad) > 0 {
		return fmt.Errorf("%w: %s", ErrDeletedRef, strings.Join(bad, ", "))
	}
	return nil
}
