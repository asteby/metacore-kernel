package agent

import (
	"fmt"
	"strings"
)

// columnMask is the set of columns of one model the identity may not read
// (ModelMeta.ReadPermissions). nil denies nothing.
type columnMask map[string]struct{}

// readMaskFor resolves the mask of a (resolved) model key.
func (s *CRUDService) readMaskFor(identity *AgentIdentity, model string) columnMask {
	m, ok := s.cfg.Catalog.Lookup(model)
	if !ok {
		return nil
	}
	return s.readMask(identity, m)
}

// readMask is the single permission decision of the agent column gate: a
// column declared in ReadPermissions is hidden unless the human the copilot
// acts for holds its permission. Without a ColumnPermissions checker every
// declared column is hidden (fail closed); DevAllowAll reads everything.
func (s *CRUDService) readMask(identity *AgentIdentity, m ModelMeta) columnMask {
	if len(m.ReadPermissions) == 0 || s.cfg.DevAllowAll {
		return nil
	}
	held := map[string]bool{}
	var out columnMask
	for col, perm := range m.ReadPermissions {
		perm = strings.TrimSpace(perm)
		if col == "" || perm == "" {
			continue
		}
		ok, seen := held[perm]
		if !seen {
			if s.cfg.ColumnPermissions != nil && identity != nil {
				has, err := s.cfg.ColumnPermissions.HasPermission(identity.OrganizationID, identity.UserID, perm)
				ok = err == nil && has
			}
			held[perm] = ok
		}
		if !ok {
			if out == nil {
				out = columnMask{}
			}
			out[strings.ToLower(col)] = struct{}{}
		}
	}
	return out
}

func (m columnMask) denied(col string) bool {
	if len(m) == 0 {
		return false
	}
	// SQL folds unquoted identifiers: "COST" and "Cost" reach the cost column.
	_, ok := m[strings.ToLower(strings.TrimSpace(col))]
	return ok
}

// check refuses a filter / order / aggregate / group over a hidden column.
func (m columnMask) check(model, col, use string) error {
	if m.denied(col) {
		return fmt.Errorf("permission denied: column %q of %s is not readable (%s)", col, model, use)
	}
	return nil
}

// allowed returns cols without the hidden ones.
func (m columnMask) allowed(cols []string) []string {
	if len(m) == 0 {
		return cols
	}
	out := make([]string, 0, len(cols))
	for _, c := range cols {
		if !m.denied(c) {
			out = append(out, c)
		}
	}
	return out
}

// redact deletes the hidden columns from a record in place.
func (m columnMask) redact(row map[string]any) {
	if len(m) == 0 {
		return
	}
	for k := range row {
		if m.denied(k) {
			delete(row, k)
		}
	}
}

// schemaColumnKeys are the describe_model entries that list columns.
var schemaColumnKeys = map[string]bool{"columns": true, "fields": true, "search_in": true, "filters": true}

// pruneSchemaValue drops hidden columns from a column list of the opaque
// describe_model schema: a []string, or a list of objects naming the column
// under "key", "name" or "field". Anything else is returned unchanged.
func (m columnMask) pruneSchemaValue(key string, v any) any {
	if len(m) == 0 || !schemaColumnKeys[key] {
		return v
	}
	switch t := v.(type) {
	case []string:
		return m.allowed(t)
	case []any:
		out := make([]any, 0, len(t))
		for _, it := range t {
			if m.deniedEntry(it) {
				continue
			}
			out = append(out, it)
		}
		return out
	case []map[string]any:
		out := make([]map[string]any, 0, len(t))
		for _, it := range t {
			if m.deniedEntry(it) {
				continue
			}
			out = append(out, it)
		}
		return out
	}
	return v
}

func (m columnMask) deniedEntry(it any) bool {
	switch e := it.(type) {
	case string:
		return m.denied(e)
	case map[string]any:
		for _, k := range []string{"key", "name", "field"} {
			if s, ok := e[k].(string); ok && m.denied(s) {
				return true
			}
		}
	}
	return false
}
