package wasm

import (
	"fmt"
	"strings"

	"gorm.io/gorm"
)

// Tenant scoping for data_query when the target table has no
// `organization_id` of its own.
//
// data_query used to append `organization_id = $1` unconditionally. That is
// right for a top-level table and fatal for a CHILD table (a document's line
// items), which carries no org column of its own — the read died with 42703.
//
// Dropping the predicate when the column is absent would "fix" the 42703 by
// removing the tenant boundary: the guest supplies the table and the equality
// filters, so an unscoped child table could be read across organizations. The
// FK path is what makes a child row belong to an org, so that is what gets
// checked: the row is admitted only if its parent row is in the caller's org.
//
//	EXISTS (SELECT 1 FROM <parent> p
//	         WHERE p.<parent_col> = <child>.<fk_col>
//	           AND p.organization_id = $1)
//
// One clause per qualifying foreign key, ANDed. A table with no qualifying FK
// is refused outright rather than read unscoped — see orgScopeClauses.

// orgScopeFK is one single-column foreign key from the child table to a parent
// that carries `organization_id`.
type orgScopeFK struct {
	Column       string // child column
	ParentTable  string // schema-qualified, already quoted
	ParentColumn string // referenced column on the parent
}

// errNoOrgScopePath reports that a table without `organization_id` also has no
// NOT NULL foreign key to an org-scoped parent, so the host cannot prove which
// organization its rows belong to.
type errNoOrgScopePath struct{ table string }

func (e *errNoOrgScopePath) Error() string {
	return fmt.Sprintf(
		"table %s has no organization_id and no NOT NULL foreign key to an org-scoped parent table, so the host cannot scope the read to one organization; query the parent table instead, or add organization_id to it",
		e.table)
}

// fkCandidatesSQL lists every single-column, NOT NULL foreign key on a table,
// with the parent it points at. Composite FKs are skipped: scoping through one
// column of a composite key would be wrong, and no model in the catalog uses
// one as its owning link.
//
// NULLABLE FKs are excluded deliberately. A nullable link cannot carry the
// tenant boundary — a row with the FK set to NULL would satisfy no EXISTS
// clause, so admitting nullable FKs would force a choice between dropping
// those rows and treating NULL as "unscoped", and the second option is exactly
// the cross-org read this guards against.
const fkCandidatesSQL = `
SELECT att.attname       AS fk_column,
       ns2.nspname       AS parent_schema,
       cl2.relname       AS parent_table,
       att2.attname      AS parent_column
FROM pg_constraint c
JOIN pg_class     cl  ON cl.oid  = c.conrelid
JOIN pg_class     cl2 ON cl2.oid = c.confrelid
JOIN pg_namespace ns2 ON ns2.oid = cl2.relnamespace
JOIN pg_attribute att  ON att.attrelid  = cl.oid  AND att.attnum  = c.conkey[1]
JOIN pg_attribute att2 ON att2.attrelid = cl2.oid AND att2.attnum = c.confkey[1]
WHERE c.contype = 'f'
  AND cl.oid = $1::regclass
  AND array_length(c.conkey, 1) = 1
  AND att.attnotnull
ORDER BY att.attname`

// orgScopeClauses returns the WHERE fragments that scope a read of `tbl` to
// `$1` (the caller's organization).
//
//   - The table has `organization_id` → the plain predicate, unchanged from
//     before this file existed. This is the path every top-level table takes.
//   - Otherwise → one EXISTS clause per qualifying FK, ANDed, so a row is
//     visible only when ALL of its owning parents are in the caller's org.
//     ANDing rather than ORing is the conservative reading: a row whose
//     parents disagree about their organization is corrupt, and excluding it
//     is the safe failure.
//   - No qualifying FK → errNoOrgScopePath. Fail closed: a table the host
//     cannot tie to an organization is not readable through this import.
//
// The clauses bind the caller's organization to `ph` (a placeholder like
// "$1"), which is the ONLY parameter they introduce — callers keep full
// control of their own numbering.
func orgScopeClauses(work *gorm.DB, tbl string, cols map[string]bool, ph string) ([]string, error) {
	spec, err := resolveOrgScope(work, tbl, cols)
	if err != nil {
		return nil, err
	}
	return spec.clauses(ph), nil
}

// orgScopedForeignKeys returns the NOT NULL single-column foreign keys of tbl
// whose parent table carries `organization_id`. A FK to a parent that is
// itself org-less (a lookup/catalog table, or another child) proves nothing
// about the tenant and is dropped — this walk is deliberately ONE level deep:
// recursing would make the visible row set depend on the shape of the schema
// several joins away, which is hard to reason about and easy to get wrong.
func orgScopedForeignKeys(work *gorm.DB, tbl string) ([]orgScopeFK, error) {
	rows, err := work.Raw(fkCandidatesSQL, tbl).Rows()
	if err != nil {
		return nil, err
	}
	type candidate struct{ fkCol, schema, table, parentCol string }
	var candidates []candidate
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.fkCol, &c.schema, &c.table, &c.parentCol); err != nil {
			_ = rows.Close()
			return nil, err
		}
		candidates = append(candidates, c)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	_ = rows.Close()

	var out []orgScopeFK
	for _, c := range candidates {
		parent := quoteIdent(c.schema) + "." + quoteIdent(c.table)
		parentCols, err := tableColumns(work, parent)
		if err != nil {
			// A parent we cannot introspect cannot be used to prove tenancy;
			// skip it rather than failing the whole read, and let the
			// no-qualifying-FK path refuse if nothing else qualifies.
			continue
		}
		if !parentCols["organization_id"] {
			continue
		}
		out = append(out, orgScopeFK{
			Column:       c.fkCol,
			ParentTable:  parent,
			ParentColumn: c.parentCol,
		})
	}
	return out, nil
}

// tableColumns returns the column set of tbl via a zero-row probe, the same
// deterministic, driver-agnostic technique tableHasColumn uses (and which it
// now delegates to).
func tableColumns(work *gorm.DB, tbl string) (map[string]bool, error) {
	rows, err := work.Raw(fmt.Sprintf("SELECT * FROM %s LIMIT 0", tbl)).Rows()
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	out := make(map[string]bool, len(cols))
	for _, c := range cols {
		out[strings.ToLower(c)] = true
	}
	return out, rows.Err()
}

// orgScopeSpec is a table's resolved tenant-scoping strategy: either the plain
// column or a set of parent links. Resolving it costs one FK introspection (and
// one probe per candidate parent), so the mutation paths — which need the same
// predicate at two different placeholder positions, e.g. the pre-read snapshot
// and the UPDATE itself — resolve it ONCE and render it as many times as
// needed. Rendering is pure string work.
type orgScopeSpec struct {
	hasOrgColumn bool
	fks          []orgScopeFK
	table        string
}

// resolveOrgScope determines how `tbl` is scoped to an organization. It returns
// errNoOrgScopePath when the table has neither an organization_id nor a usable
// parent link — the fail-closed case.
func resolveOrgScope(work *gorm.DB, tbl string, cols map[string]bool) (*orgScopeSpec, error) {
	if cols["organization_id"] {
		return &orgScopeSpec{hasOrgColumn: true, table: tbl}, nil
	}
	fks, err := orgScopedForeignKeys(work, tbl)
	if err != nil {
		return nil, err
	}
	if len(fks) == 0 {
		return nil, &errNoOrgScopePath{table: tbl}
	}
	return &orgScopeSpec{fks: fks, table: tbl}, nil
}

// clauses renders the scope as WHERE fragments binding the organization to ph.
func (s *orgScopeSpec) clauses(ph string) []string {
	if s.hasOrgColumn {
		return []string{"organization_id = " + ph}
	}
	out := make([]string, 0, len(s.fks))
	for i, fk := range s.fks {
		alias := fmt.Sprintf("__org_scope_%d", i)
		out = append(out, fmt.Sprintf(
			"EXISTS (SELECT 1 FROM %s %s WHERE %s.%s = %s.%s AND %s.organization_id = %s)",
			fk.ParentTable, alias,
			alias, quoteIdent(fk.ParentColumn),
			s.table, quoteIdent(fk.Column),
			alias, ph))
	}
	return out
}

// predicate renders the scope as ONE ANDed WHERE fragment, for the mutation
// paths that splice a single condition into a larger statement.
func (s *orgScopeSpec) predicate(ph string) string {
	return strings.Join(s.clauses(ph), " AND ")
}
