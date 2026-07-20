package wasm

import (
	"fmt"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// Tenant scoping for data_mutate CREATE when the target table has no
// `organization_id`.
//
// The create path used to put `organization_id` in the INSERT column list
// unconditionally, so inserting into a child table (a document's line items)
// died with 42703 — the write-side twin of the read bug fixed for data_query.
//
// The fix is NOT symmetric with the read side, and the asymmetry is the whole
// point. On a read, dropping the tenant predicate EXPOSES other organizations'
// rows. On a write, dropping the column instead lets the guest ATTACH a row to
// another organization's tree: nothing about `{"purchase_order_id": "<ajeno>"}`
// is malformed, and once written the line is a real child of a document the
// caller was never allowed to touch. So omitting the column is necessary but
// not sufficient — the parent link has to be verified.
//
// verifyChildCreateTenancy therefore demands, for a table with no
// organization_id, that the new row present at least one FK to a parent that
// IS in the caller's organization, and that every FK it presents to an
// org-scoped parent table resolves inside that organization.

// errChildCreateUnverifiable reports that a create on an org-less table could
// not be tied to the caller's organization.
type errChildCreateUnverifiable struct {
	table  string
	reason string
}

func (e *errChildCreateUnverifiable) Error() string {
	return fmt.Sprintf("cannot create in %s: %s", e.table, e.reason)
}

// verifyChildCreateTenancy validates the parent links of a row about to be
// inserted into a table that carries no `organization_id`.
//
// Rules, in order:
//
//   - No FK to an org-scoped parent exists at all → refuse. The host has no
//     way to tie the row to an organization, and an unscoped INSERT would
//     create a row nobody can safely read back.
//   - A qualifying FK is present in `data` but its value names a parent row
//     that is missing, or lives in another organization → refuse. This is the
//     cross-tenant write attempt.
//   - No qualifying FK is supplied in `data` → refuse. A row with every parent
//     link left to a column default is not attributable either.
//
// Only the org-less path calls this. A table WITH organization_id keeps the
// exact behaviour it had before (the column pins the row's tenancy), so this
// change cannot regress any create that works today.
func verifyChildCreateTenancy(work *gorm.DB, tbl string, data map[string]any, orgID uuid.UUID) error {
	fks, err := orgScopedForeignKeys(work, tbl)
	if err != nil {
		return err
	}
	if len(fks) == 0 {
		return &errChildCreateUnverifiable{
			table:  tbl,
			reason: "the table has no organization_id and no NOT NULL foreign key to an org-scoped parent, so the host cannot determine which organization the new row would belong to",
		}
	}

	checked := 0
	for _, fk := range fks {
		v, supplied := data[fk.Column]
		if !supplied || v == nil {
			continue
		}
		ok, err := parentRowInOrg(work, fk, v, orgID)
		if err != nil {
			return err
		}
		if !ok {
			return &errChildCreateUnverifiable{
				table: tbl,
				reason: fmt.Sprintf(
					"%s references a row in %s that does not exist in this organization",
					fk.Column, fk.ParentTable),
			}
		}
		checked++
	}
	if checked == 0 {
		names := make([]string, 0, len(fks))
		for _, fk := range fks {
			names = append(names, fk.Column)
		}
		return &errChildCreateUnverifiable{
			table: tbl,
			reason: fmt.Sprintf(
				"the row supplies none of the parent links the host can verify (%v), so it cannot be attributed to an organization",
				names),
		}
	}
	return nil
}

// parentRowInOrg reports whether the referenced parent row exists inside orgID.
// A missing row and a row owned by another organization are the same answer on
// purpose: the guest learns only "not yours", never whether the id exists
// elsewhere.
func parentRowInOrg(work *gorm.DB, fk orgScopeFK, value any, orgID uuid.UUID) (bool, error) {
	stmt := fmt.Sprintf(
		"SELECT 1 FROM %s WHERE %s = $1 AND organization_id = $2 LIMIT 1",
		fk.ParentTable, quoteIdent(fk.ParentColumn))
	row, err := queryOneRow(work, stmt, value, orgID)
	if err != nil {
		return false, err
	}
	return row != nil, nil
}
