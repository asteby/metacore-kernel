package wasm

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/asteby/metacore-kernel/dynamic"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// ExtensionFn resolves the 1:1 extension tables of a logical table (manifest
// Model.extends). model is the guest ModelKey and may be empty on data_query,
// which only names a table. Return nil when the table has no extensions.
// The physical table must be a snake_case identifier or schema.table; anything
// else is ignored so a guest column name can never choose the SQL target.
type ExtensionFn func(logicalTable, model string) []dynamic.ExtensionTable

// WithExtensions wires extension-column routing into data_mutate, data_batch
// and data_query. A guest field "<Ext>.<column>" (or a nested object under
// the extension key) is written to that extension's table, keyed by the owner
// row id, and data_query can filter on it and returns it under the same
// prefix the REST dynamic service uses. When unset, a dotted column name is
// invalid_request and never reaches SQL.
func (h *Host) WithExtensions(fn ExtensionFn) *Host {
	h.extensions = fn
	return h
}

func extensionsFor(inv *invocation, table, model string) []dynamic.ExtensionTable {
	if inv == nil || inv.extensions == nil {
		return nil
	}
	raw := inv.extensions(table, model)
	out := make([]dynamic.ExtensionTable, 0, len(raw))
	for _, e := range raw {
		if e.Key == "" || strings.Contains(e.Key, ".") || !safePhysicalTable(e.Table) {
			continue
		}
		out = append(out, e)
	}
	return out
}

func safePhysicalTable(t string) bool {
	parts := strings.Split(t, ".")
	if len(parts) < 1 || len(parts) > 2 {
		return false
	}
	for _, p := range parts {
		if !dataMutateIdentRe.MatchString(p) {
			return false
		}
	}
	return true
}

// explodeExtensionObjects turns a nested {"TireSpec": {"width": 205}} value
// into the flat "TireSpec.width" keys the rest of the write path uses. A
// key that is not a wired extension is left alone for the column check.
func explodeExtensionObjects(exts []dynamic.ExtensionTable, raw map[string]json.RawMessage) error {
	if len(exts) == 0 || len(raw) == 0 {
		return nil
	}
	for _, e := range exts {
		rv, ok := raw[e.Key]
		if !ok {
			continue
		}
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(rv, &obj); err != nil || obj == nil {
			return fmt.Errorf("%s: extension value must be an object of columns", e.Key)
		}
		delete(raw, e.Key)
		for col, v := range obj {
			composed := e.Key + "." + col
			if _, dup := raw[composed]; dup {
				return fmt.Errorf("column %q set both as a field and inside %s", composed, e.Key)
			}
			raw[composed] = v
		}
	}
	return nil
}

// extColumn reports the extension and bare column of a "<Key>.<column>" name
// when that column is declared. The second return is the bare column.
func extColumn(exts []dynamic.ExtensionTable, prefixed string) (dynamic.ExtensionTable, string, bool) {
	prefix, name, ok := strings.Cut(prefixed, ".")
	if !ok || prefix == "" || strings.Contains(name, ".") || !dataMutateIdentRe.MatchString(name) {
		return dynamic.ExtensionTable{}, "", false
	}
	for _, e := range exts {
		if e.Key != prefix {
			continue
		}
		for _, c := range e.Columns {
			if c.Name == name {
				return e, name, true
			}
		}
		return dynamic.ExtensionTable{}, "", false
	}
	return dynamic.ExtensionTable{}, "", false
}

func extColumnWritable(exts []dynamic.ExtensionTable, prefixed string) bool {
	e, name, ok := extColumn(exts, prefixed)
	if !ok {
		return false
	}
	for _, c := range e.Columns {
		if c.Name != name {
			continue
		}
		return c.Generated == "" && c.SearchKey == nil && c.Sequence == ""
	}
	return false
}

// partitionExtensionCols moves "<Key>.<column>" entries out of the owner
// maps. The caller has already rejected unknown prefixes.
func partitionExtensionCols(exts []dynamic.ExtensionTable, data, inc map[string]any) (map[string]map[string]any, map[string]map[string]any) {
	return takeExt(exts, data), takeExt(exts, inc)
}

func takeExt(exts []dynamic.ExtensionTable, cols map[string]any) map[string]map[string]any {
	if len(exts) == 0 || len(cols) == 0 {
		return nil
	}
	var out map[string]map[string]any
	for k, v := range cols {
		e, name, ok := extColumn(exts, k)
		if !ok {
			continue
		}
		if out == nil {
			out = map[string]map[string]any{}
		}
		if out[e.Key] == nil {
			out[e.Key] = map[string]any{}
		}
		out[e.Key][name] = v
		delete(cols, k)
	}
	return out
}

func mergeExtensionFields(row map[string]any, data, inc map[string]map[string]any) {
	if row == nil {
		return
	}
	for key, cols := range data {
		for col, v := range cols {
			row[key+"."+col] = v
		}
	}
	for key, cols := range inc {
		for col, v := range cols {
			if _, set := row[key+"."+col]; set {
				continue
			}
			row[key+"."+col] = v
		}
	}
}

// writeExtensions upserts each touched extension row. id is the owner row id
// (the extension's primary key). Absolute sets use EXCLUDED; increments add
// to the stored column so two guests cannot lose an update.
func writeExtensions(work *gorm.DB, exts []dynamic.ExtensionTable, data, inc map[string]map[string]any, ownerID string, orgID uuid.UUID) error {
	if len(data) == 0 && len(inc) == 0 {
		return nil
	}
	for _, e := range exts {
		sets := data[e.Key]
		deltas := inc[e.Key]
		if len(sets) == 0 && len(deltas) == 0 {
			continue
		}
		tbl, err := quoteQualifiedTable(e.Table)
		if err != nil {
			return err
		}
		names := make([]string, 0, len(sets)+len(deltas))
		seen := map[string]struct{}{}
		for n := range sets {
			seen[n] = struct{}{}
			names = append(names, n)
		}
		for n := range deltas {
			if _, ok := seen[n]; ok {
				continue
			}
			names = append(names, n)
		}
		sort.Strings(names)
		cols := []string{quoteIdent("id"), quoteIdent("organization_id")}
		args := []any{ownerID, orgID}
		for _, n := range names {
			cols = append(cols, quoteIdent(n))
			if v, ok := sets[n]; ok {
				args = append(args, v)
			} else {
				args = append(args, deltas[n])
			}
		}
		ph := make([]string, len(cols))
		for i := range cols {
			ph[i] = fmt.Sprintf("$%d", i+1)
		}
		bare := quoteIdent(tableBare(e.Table))
		assigns := make([]string, 0, len(names)+1)
		for _, n := range names {
			q := quoteIdent(n)
			if _, ok := deltas[n]; ok {
				assigns = append(assigns, fmt.Sprintf("%s = %s.%s + EXCLUDED.%s", q, bare, q, q))
				continue
			}
			assigns = append(assigns, fmt.Sprintf("%s = EXCLUDED.%s", q, q))
		}
		assigns = append(assigns, fmt.Sprintf("%s = CURRENT_TIMESTAMP", quoteIdent("updated_at")))
		stmt := fmt.Sprintf(`INSERT INTO %s (%s) VALUES (%s) ON CONFLICT ("id") DO UPDATE SET %s`,
			tbl, strings.Join(cols, ", "), strings.Join(ph, ", "), strings.Join(assigns, ", "))
		if err := work.Exec(stmt, args...).Error; err != nil {
			return fmt.Errorf("write extension %s: %w", e.Key, err)
		}
	}
	return nil
}

func tableBare(physical string) string {
	parts := strings.Split(physical, ".")
	return parts[len(parts)-1]
}

// mergeQueriedExtensions loads each extension row for the owner ids and
// copies declared columns onto the owner row as "<Key>.<column>", the same
// shape the REST dynamic service returns.
func mergeQueriedExtensions(work *gorm.DB, exts []dynamic.ExtensionTable, rows []map[string]any) error {
	if len(exts) == 0 || len(rows) == 0 {
		return nil
	}
	ids := make([]any, 0, len(rows))
	index := map[string][]int{}
	for i, row := range rows {
		id := fmt.Sprint(row["id"])
		if id == "" || id == "<nil>" {
			continue
		}
		if _, seen := index[id]; !seen {
			ids = append(ids, row["id"])
		}
		index[id] = append(index[id], i)
	}
	if len(ids) == 0 {
		return nil
	}
	for _, e := range exts {
		tbl, err := quoteQualifiedTable(e.Table)
		if err != nil {
			return err
		}
		ph := make([]string, len(ids))
		for i := range ids {
			ph[i] = fmt.Sprintf("$%d", i+1)
		}
		stmt := fmt.Sprintf("SELECT * FROM %s WHERE %s IN (%s)", tbl, quoteIdent("id"), strings.Join(ph, ", "))
		erows, err := work.Raw(stmt, ids...).Rows()
		if err != nil {
			return fmt.Errorf("read extension %s: %w", e.Key, err)
		}
		cols, err := erows.Columns()
		if err != nil {
			_ = erows.Close()
			return err
		}
		declared := map[string]struct{}{}
		for _, c := range e.Columns {
			if c.Name != "" {
				declared[c.Name] = struct{}{}
			}
		}
		for erows.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := erows.Scan(ptrs...); err != nil {
				_ = erows.Close()
				return err
			}
			rec := make(map[string]any, len(cols))
			for i, c := range cols {
				rec[c] = jsonifyDBVal(vals[i])
			}
			id := fmt.Sprint(rec["id"])
			for _, i := range index[id] {
				for name := range declared {
					rows[i][e.Key+"."+name] = rec[name]
				}
			}
		}
		err = erows.Err()
		_ = erows.Close()
		if err != nil {
			return err
		}
	}
	return nil
}
