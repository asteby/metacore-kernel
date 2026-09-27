package query

import (
	"fmt"
	"sort"

	"gorm.io/gorm"
)

// ExtensionTable is a 1:1 extension of the listed model (manifest v3
// Model.extends): a table whose `id` is the owner row's id. Filters address its
// columns as `f_<Key>.<column>`; its search keys join the free-text search.
type ExtensionTable struct {
	// Key is the extension model key used as the filter prefix ("TireSpec").
	Key string
	// Table is the physical table, plain or schema-qualified.
	Table string
	// Columns are the filterable columns (anything else is ignored).
	Columns map[string]struct{}
	// SearchKeys are normalized search-key columns: the typed term goes through
	// Normalize and is compared for equality against the stored key.
	SearchKeys []ExtensionSearchKey
}

// ExtensionSearchKey is one search-key column of an extension table.
type ExtensionSearchKey struct {
	Column    string
	Normalize func(term string) string
}

// WithExtensions registers the model's extension tables. Unsafe names are
// dropped here so the SQL emitters never see them.
func (b *Builder) WithExtensions(exts []ExtensionTable) *Builder {
	b.extensions = map[string]ExtensionTable{}
	b.extOrder = nil
	for _, e := range exts {
		if !isSafeIdent(e.Key) || !isSafeTable(e.Table) {
			continue
		}
		b.extensions[e.Key] = e
		b.extOrder = append(b.extOrder, e.Key)
	}
	sort.Strings(b.extOrder)
	return b
}

// applyExtensionFilters turns every `f_<Key>.<column>` relation filter that
// names an extension into `owner.id IN (SELECT id FROM <ext> WHERE <cond>)`.
// A filter on an unknown or undeclared column is ignored, like a garbage
// column filter on the owner.
func (b *Builder) applyExtensionFilters(db *gorm.DB, params Params) *gorm.DB {
	if len(b.extensions) == 0 || len(params.RelationFilters) == 0 {
		return db
	}
	for _, rf := range params.RelationFilters {
		ext, ok := b.extensions[rf.Relation]
		if !ok {
			continue
		}
		if _, ok := ext.Columns[rf.Field]; !ok || !isSafeIdent(rf.Field) {
			continue
		}
		sub := db.Session(&gorm.Session{NewDB: true}).Table(ext.Table + " __ex").Select("__ex.id")
		sub = applyOneFilter(sub, "__ex."+rf.Field, Filter{Op: rf.Op, Value: rf.Value})
		db = db.Where(fmt.Sprintf("%s IN (?)", b.qualifyOwner("id")), sub)
	}
	return db
}

// extensionSearchConds returns the OR-able search conditions contributed by
// extension search keys: `owner.id IN (SELECT id FROM <ext> WHERE key = ?)`.
func (b *Builder) extensionSearchConds(term string) ([]string, []any) {
	var conds []string
	var args []any
	for _, k := range b.extOrder {
		ext := b.extensions[k]
		for _, sk := range ext.SearchKeys {
			if sk.Normalize == nil || !isSafeIdent(sk.Column) {
				continue
			}
			v := sk.Normalize(term)
			if v == "" {
				continue
			}
			conds = append(conds, fmt.Sprintf("%s IN (SELECT __ex.id FROM %s __ex WHERE __ex.%s = ?)",
				b.qualifyOwner("id"), ext.Table, sk.Column))
			args = append(args, v)
		}
	}
	return conds, args
}

// hasExtensionSearch reports whether any extension contributes a search key.
func (b *Builder) hasExtensionSearch() bool {
	for _, e := range b.extensions {
		if len(e.SearchKeys) > 0 {
			return true
		}
	}
	return false
}
