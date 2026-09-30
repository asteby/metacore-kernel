package dynamic

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/asteby/metacore-kernel/manifest"
	"github.com/asteby/metacore-kernel/modelbase"
	"github.com/asteby/metacore-kernel/query"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// ExtensionTable is one 1:1 extension of a model (manifest v3 Model.extends,
// CONTRACT-item-master.md §3.1) as the host resolves it for a request: the
// extension's model key (the field prefix "<Key>.<column>"), its physical table
// and its columns as lowered by the v3 conversion (search keys included).
type ExtensionTable struct {
	Key     string
	Table   string
	Columns []manifest.ColumnDef
}

// ExtensionResolver returns the extension tables of a model that are installed
// and enabled for the request's organization. Host-wired from the addon
// registry, like RelationResolver. nil / empty = the model has no extensions
// (flat models and hosts that have not wired it are unaffected).
type ExtensionResolver func(ctx context.Context, model string) []ExtensionTable

func (s *Service) resolveExtensions(ctx context.Context, model string) []ExtensionTable {
	if s.extensions == nil {
		return nil
	}
	var out []ExtensionTable
	for _, e := range s.extensions(ctx, model) {
		if e.Key == "" || !safeQualifiedTable(e.Table) {
			continue
		}
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

var safeIdentRe = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

// safeQualifiedTable accepts table or schema.table made of plain identifiers.
func safeQualifiedTable(t string) bool {
	parts := strings.Split(t, ".")
	if len(parts) == 0 || len(parts) > 2 {
		return false
	}
	for _, p := range parts {
		if !safeIdentRe.MatchString(p) {
			return false
		}
	}
	return true
}

// quotedTable renders schema.table with each part quoted.
func quotedTable(t string) string {
	parts := strings.Split(t, ".")
	for i, p := range parts {
		parts[i] = fmt.Sprintf("%q", p)
	}
	return strings.Join(parts, ".")
}

// extColumnWritable reports whether a caller may write the column: generated,
// search-key and sequence columns are server-side.
func extColumnWritable(c manifest.ColumnDef) bool {
	return c.Name != "" && c.Generated == "" && c.SearchKey == nil && c.Sequence == ""
}

// queryExtensions adapts the model's extensions to the list builder: filters
// `f_<Key>.<column>` and the normalized search keys.
func queryExtensions(exts []ExtensionTable) []query.ExtensionTable {
	out := make([]query.ExtensionTable, 0, len(exts))
	for _, e := range exts {
		qe := query.ExtensionTable{Key: e.Key, Table: e.Table, Columns: map[string]struct{}{}}
		for _, c := range e.Columns {
			if c.Name == "" {
				continue
			}
			qe.Columns[c.Name] = struct{}{}
			if c.SearchKey != nil {
				sk := c.SearchKey
				normalize := func(term string) string { return NormalizeSearchKey(sk, term) }
				if sk.Match == "exact" {
					normalize = strings.TrimSpace
				}
				qe.SearchKeys = append(qe.SearchKeys, query.ExtensionSearchKey{Column: c.Name, Normalize: normalize, Prefix: sk.Match == "normalized_prefix"})
			}
		}
		out = append(out, qe)
	}
	return out
}

// loadExtensionRows reads the extension rows of the given ids: id → column →
// value, per extension key.
func (s *Service) loadExtensionRows(ctx context.Context, db *gorm.DB, exts []ExtensionTable, ids []string) (map[string]map[string]map[string]any, error) {
	out := map[string]map[string]map[string]any{}
	if len(exts) == 0 || len(ids) == 0 {
		return out, nil
	}
	for _, e := range exts {
		cols := []string{`"id"`}
		for _, c := range e.Columns {
			if c.Name != "" {
				cols = append(cols, fmt.Sprintf("%q", c.Name))
			}
		}
		var rows []map[string]any
		stmt := fmt.Sprintf(`SELECT %s FROM %s WHERE "id" IN ?`, strings.Join(cols, ", "), quotedTable(e.Table))
		if err := db.WithContext(ctx).Raw(stmt, ids).Scan(&rows).Error; err != nil {
			return nil, fmt.Errorf("dynamic: read extension %s: %w", e.Key, err)
		}
		byID := map[string]map[string]any{}
		for _, r := range rows {
			id := fmt.Sprint(r["id"])
			delete(r, "id")
			byID[id] = r
		}
		out[e.Key] = byID
	}
	return out, nil
}

// mergeExtensions adds "<Key>.<column>" to every item. An item with no
// extension row gets the columns as nil, so a client sees the field exists.
func (s *Service) mergeExtensions(ctx context.Context, exts []ExtensionTable, items []map[string]any) error {
	if len(exts) == 0 || len(items) == 0 {
		return nil
	}
	ids := make([]string, 0, len(items))
	for _, it := range items {
		if id := fmt.Sprint(it["id"]); id != "" && id != "<nil>" {
			ids = append(ids, id)
		}
	}
	rows, err := s.loadExtensionRows(ctx, s.db, exts, ids)
	if err != nil {
		return err
	}
	for _, it := range items {
		id := fmt.Sprint(it["id"])
		for _, e := range exts {
			row := rows[e.Key][id]
			for _, c := range e.Columns {
				if c.Name == "" {
					continue
				}
				var v any
				if row != nil {
					v = row[c.Name]
				}
				it[e.Key+"."+c.Name] = v
			}
		}
	}
	return nil
}

// splitExtensionInput moves every "<Key>.<column>" key of input (and a nested
// "<Key>": {column: value} object) into its own map, so the owner write never
// sees them. Unknown extension keys are left in input untouched.
func splitExtensionInput(exts []ExtensionTable, input map[string]any) map[string]map[string]any {
	out := map[string]map[string]any{}
	for _, e := range exts {
		prefix := e.Key + "."
		for k, v := range input {
			if strings.HasPrefix(k, prefix) {
				if out[e.Key] == nil {
					out[e.Key] = map[string]any{}
				}
				out[e.Key][strings.TrimPrefix(k, prefix)] = v
				delete(input, k)
			}
		}
		if nested, ok := input[e.Key].(map[string]any); ok {
			if out[e.Key] == nil {
				out[e.Key] = map[string]any{}
			}
			for k, v := range nested {
				if _, set := out[e.Key][k]; !set {
					out[e.Key][k] = v
				}
			}
			delete(input, e.Key)
		}
	}
	return out
}

// validateExtensionInput runs the base column rules over each extension's
// submitted columns and returns the issues with "<Key>." prefixed field names.
// An extension the caller did not touch is not validated (a create without
// tire data is a create without a tire spec). On update, selfID is the owner
// id and before its extension rows.
func (s *Service) validateExtensionInput(ctx context.Context, user modelbase.AuthUser, exts []ExtensionTable, extIn map[string]map[string]any, selfID *uuid.UUID, before map[string]map[string]any) (*ValidationError, error) {
	ve := &ValidationError{}
	for _, e := range exts {
		sub := extIn[e.Key]
		if len(sub) == 0 {
			continue
		}
		known := map[string]struct{}{}
		for _, c := range e.Columns {
			known[c.Name] = struct{}{}
		}
		for k := range sub {
			if _, ok := known[k]; !ok {
				ve.add(e.Key+"."+k, "unknown_field", nil)
				delete(sub, k)
			}
		}
		for _, c := range e.Columns {
			if raw, sent := sub[c.Name]; sent && !extColumnWritable(c) {
				// A form posts every field back, the generated search key
				// included: echoing the persisted (or an empty) value changes
				// nothing, so it is dropped instead of failing the save (same
				// rule as a protected owner column). Only an attempt to change
				// it is rejected.
				if !protectedNoop(raw, c, before[e.Key], selfID != nil) {
					ve.add(e.Key+"."+c.Name, codeProtected, nil)
				}
				delete(sub, c.Name)
			}
		}
		cols := make([]manifest.ColumnDef, 0, len(e.Columns))
		for _, c := range e.Columns {
			if extColumnWritable(c) {
				cols = append(cols, c)
			}
		}
		// Required is judged against the row as it will be: a PATCH of one
		// column must not trip the required check of the others.
		sv, err := s.validateColumns(ctx, cols, e.Table, user, sub, selfID, before[e.Key])
		if err != nil {
			return nil, err
		}
		if sv != nil {
			for f, issues := range sv.Fields {
				for _, is := range issues {
					ve.add(e.Key+"."+f, is.Code, is.Params)
				}
			}
		}
	}
	return ve, nil
}

// mergeValidation folds the extension issues into the owner's validation
// result. err is the owner's validateWrite result.
func mergeValidation(err error, ext *ValidationError) error {
	if ext.Empty() {
		return err
	}
	if err == nil {
		return ext
	}
	if base, ok := err.(*ValidationError); ok {
		for f, issues := range ext.Fields {
			for _, is := range issues {
				base.add(f, is.Code, is.Params)
			}
		}
		return base
	}
	return err
}

// upsertExtensions writes each touched extension row for the owner id: one
// INSERT … ON CONFLICT (id) DO UPDATE per extension, only the columns the
// caller sent.
func (s *Service) upsertExtensions(ctx context.Context, db *gorm.DB, exts []ExtensionTable, extIn map[string]map[string]any, ownerID string, orgID uuid.UUID) error {
	for _, e := range exts {
		sub := extIn[e.Key]
		if len(sub) == 0 {
			continue
		}
		colDef := map[string]manifest.ColumnDef{}
		for _, c := range e.Columns {
			colDef[c.Name] = c
		}
		names := make([]string, 0, len(sub))
		for k := range sub {
			if c, ok := colDef[k]; ok && extColumnWritable(c) {
				names = append(names, k)
			}
		}
		if len(names) == 0 {
			continue
		}
		sort.Strings(names)
		cols := []string{`"id"`, `"organization_id"`}
		ph := []string{"?", "?"}
		args := []any{ownerID, orgID}
		sets := make([]string, 0, len(names)+1)
		for _, n := range names {
			cols = append(cols, fmt.Sprintf("%q", n))
			ph = append(ph, "?")
			args = append(args, coerceExtensionValue(colDef[n], sub[n]))
			sets = append(sets, fmt.Sprintf(`%q = EXCLUDED.%q`, n, n))
		}
		sets = append(sets, `"updated_at" = CURRENT_TIMESTAMP`)
		stmt := fmt.Sprintf(`INSERT INTO %s (%s) VALUES (%s) ON CONFLICT ("id") DO UPDATE SET %s`,
			quotedTable(e.Table), strings.Join(cols, ", "), strings.Join(ph, ", "), strings.Join(sets, ", "))
		if err := db.WithContext(ctx).Exec(stmt, args...).Error; err != nil {
			return fmt.Errorf("dynamic: write extension %s: %w", e.Key, err)
		}
	}
	return nil
}

// coerceExtensionValue turns the form's string values into the column's Go
// type ("" → NULL, "205" → 205 for an integer column). Values validation
// already accepted always coerce; anything else passes through for Postgres to
// judge.
func coerceExtensionValue(c manifest.ColumnDef, v any) any {
	str, isStr := v.(string)
	if !isStr {
		return v
	}
	str = strings.TrimSpace(str)
	if str == "" {
		return nil
	}
	t := strings.ToLower(c.Type)
	switch {
	case t == "integer" || t == "int" || t == "bigint" || t == "smallint":
		if n, err := strconv.ParseInt(str, 10, 64); err == nil {
			return n
		}
	case t == "numeric" || t == "decimal" || t == "float" || t == "double" || strings.HasPrefix(t, "numeric("):
		if f, err := strconv.ParseFloat(str, 64); err == nil {
			return f
		}
	case t == "boolean" || t == "bool":
		if b, err := strconv.ParseBool(str); err == nil {
			return b
		}
	}
	return str
}

// extensionBefore loads the owner's current extension rows, keyed by extension
// key (nil when the owner has none), for update validation and events.
func extensionBefore(ctx context.Context, s *Service, exts []ExtensionTable, id uuid.UUID) (map[string]map[string]any, error) {
	if len(exts) == 0 {
		return nil, nil
	}
	rows, err := s.loadExtensionRows(ctx, s.db, exts, []string{id.String()})
	if err != nil {
		return nil, err
	}
	out := map[string]map[string]any{}
	for k, byID := range rows {
		out[k] = byID[id.String()]
	}
	return out, nil
}

// ServeExtensions adds the model's extension columns ("<Key>.<column>") to rows
// a host read through its OWN list/show path (one not delegated to List/Get).
// A model without extensions is a no-op.
func (s *Service) ServeExtensions(ctx context.Context, model string, rows []map[string]any) error {
	return s.mergeExtensions(ctx, s.resolveExtensions(ctx, model), rows)
}

// HasExtensionInput reports whether input carries any "<Key>.<column>" (or
// nested "<Key>") key of the model's extensions — a host routing writes uses it
// to know the write needs WriteExtensions.
func (s *Service) HasExtensionInput(ctx context.Context, model string, input map[string]any) bool {
	for _, e := range s.resolveExtensions(ctx, model) {
		if _, ok := input[e.Key]; ok {
			return true
		}
		prefix := e.Key + "."
		for k := range input {
			if strings.HasPrefix(k, prefix) {
				return true
			}
		}
	}
	return false
}

// TakeExtensionInput removes the extension keys from input and validates them
// against the extension columns, for a host whose OWN write path persists the
// owner row (Create/Update do this themselves). ownerID is nil on create. The
// returned value is opaque: hand it to WriteExtensions once the owner row is
// written. A *ValidationError carries "<Key>.<column>" field names.
func (s *Service) TakeExtensionInput(ctx context.Context, model string, user modelbase.AuthUser, input map[string]any, ownerID *uuid.UUID) (ExtensionInput, error) {
	exts := s.resolveExtensions(ctx, model)
	in := ExtensionInput{exts: exts, values: splitExtensionInput(exts, input)}
	if len(in.values) == 0 {
		return in, nil
	}
	var before map[string]map[string]any
	if ownerID != nil {
		b, err := extensionBefore(ctx, s, exts, *ownerID)
		if err != nil {
			return in, err
		}
		before = b
	}
	ve, err := s.validateExtensionInput(ctx, user, exts, in.values, ownerID, before)
	if err != nil {
		return in, err
	}
	if !ve.Empty() {
		return in, ve
	}
	return in, nil
}

// ExtensionInput is the validated extension part of a write (TakeExtensionInput).
type ExtensionInput struct {
	exts   []ExtensionTable
	values map[string]map[string]any
}

// Empty reports whether the write touched no extension.
func (in ExtensionInput) Empty() bool { return len(in.values) == 0 }

// WriteExtensions upserts the extension rows of ownerID (PATCH: only the
// columns sent).
func (s *Service) WriteExtensions(ctx context.Context, in ExtensionInput, ownerID uuid.UUID, orgID uuid.UUID) error {
	if in.Empty() {
		return nil
	}
	return s.upsertExtensions(ctx, s.db, in.exts, in.values, ownerID.String(), orgID)
}

// extensionSearchConds is the options-picker twin of the list builder's
// extension search: `"id" IN (SELECT id FROM <ext> WHERE <key> = ?)` per search
// key, with the typed text normalized like the stored key.
func extensionSearchConds(exts []ExtensionTable, term string) ([]string, []any) {
	var conds []string
	var args []any
	for _, e := range exts {
		for _, c := range e.Columns {
			if c.SearchKey == nil || !safeIdentRe.MatchString(c.Name) {
				continue
			}
			v := strings.TrimSpace(term)
			if c.SearchKey.Match != "exact" {
				v = NormalizeSearchKey(c.SearchKey, term)
			}
			if v == "" {
				continue
			}
			if c.SearchKey.Match == "normalized_prefix" {
				conds = append(conds, fmt.Sprintf(`"id" IN (SELECT "id" FROM %s WHERE %q LIKE ?)`, quotedTable(e.Table), c.Name))
				args = append(args, v+"%")
				continue
			}
			conds = append(conds, fmt.Sprintf(`"id" IN (SELECT "id" FROM %s WHERE %q = ?)`, quotedTable(e.Table), c.Name))
			args = append(args, v)
		}
	}
	return conds, args
}
