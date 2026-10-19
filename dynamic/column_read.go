package dynamic

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/asteby/metacore-kernel/modelbase"
	"github.com/asteby/metacore-kernel/permission"
	"github.com/asteby/metacore-kernel/query"
)

// Column read permissions (manifest v3 columns[].read_permission →
// modelbase.ColumnDef.ReadPermission).
//
// A column that declares a ReadPermission is readable only by callers holding
// that permission. Every read surface of the dynamic runtime resolves ONE
// ReadMask per (model, caller) through Service.ColumnReadMask and runs its
// output and its client-controlled query shape through it:
//
//   - rows (List, Get, Create/Update/Restore responses, singleton, source
//     lines, export) are served without the denied columns, including the
//     columns of related models embedded by ?with= preloads;
//   - a filter, sort, group_by or aggregate over a denied column is rejected
//     with *ColumnReadDeniedError (403, code "column_read_denied"), never
//     silently ignored, so a client cannot probe the hidden value;
//   - free-text search, relation search and the options/search pickers never
//     match or project a denied column;
//   - facets over a denied column are rejected.
//
// Events published on the in-process bus keep the full row: subscribers are
// server-side (addons, audit, dispatch). A host that forwards events to a
// client (websocket, outbound webhook) runs them through RedactEvent first.

// ColumnReadChecker decides whether user holds perm, the permission a column
// declares in ReadPermission. Wire it (Config.ColumnReadChecker) when the host
// resolves permissions outside Config.Permissions; nil uses
// Config.Permissions.Check with perm as the capability.
type ColumnReadChecker func(ctx context.Context, user modelbase.AuthUser, perm string) bool

// ColumnReadDeniedError reports a query that tried to use a column the caller
// may not read (filter, sort, group_by, aggregate, facet, options filter). It
// wraps ErrForbidden so it answers 403; the handler adds code
// "column_read_denied", the model and the column.
type ColumnReadDeniedError struct {
	Model  string
	Column string
	// Use is what the query tried to do with the column: filter, sort,
	// group_by, aggregate, facet.
	Use string
}

func (e *ColumnReadDeniedError) Error() string {
	return fmt.Sprintf("permission denied: column %q of %s is not readable (%s)", e.Column, e.Model, e.Use)
}

// Unwrap ties the error to ErrForbidden for errors.Is-based 403 mapping.
func (e *ColumnReadDeniedError) Unwrap() error { return ErrForbidden }

// ReadMask is the set of columns of one model a caller may not read. The zero
// value and nil deny nothing, so callers never need a nil check.
type ReadMask struct {
	model  string
	denied map[string]struct{}
	// related resolves the mask of a related model (preloads, relation
	// filters). nil = related models are not masked.
	related func(model string) *ReadMask
}

// Empty reports whether the mask hides nothing.
func (m *ReadMask) Empty() bool { return m == nil || len(m.denied) == 0 }

// Columns returns the denied column keys, sorted.
func (m *ReadMask) Columns() []string {
	if m.Empty() {
		return nil
	}
	out := make([]string, 0, len(m.denied))
	for c := range m.denied {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

// Denied reports whether col may not be read. col may be a plain column, an
// extension column ("<Ext>.<column>") or a jsonb path ("<column>.<key>"): the
// full key and its first segment are both checked.
func (m *ReadMask) Denied(col string) bool {
	if m.Empty() || col == "" {
		return false
	}
	col = strings.TrimSpace(col)
	if _, ok := m.denied[col]; ok {
		return true
	}
	if i := strings.IndexByte(col, '.'); i > 0 {
		if _, ok := m.denied[col[:i]]; ok {
			return true
		}
	}
	return false
}

// Check returns a *ColumnReadDeniedError when col is denied, nil otherwise.
func (m *ReadMask) Check(col, use string) error {
	if m.Denied(col) {
		return &ColumnReadDeniedError{Model: m.model, Column: col, Use: use}
	}
	return nil
}

// Allowed returns cols without the denied ones (same order).
func (m *ReadMask) Allowed(cols []string) []string {
	if m.Empty() {
		return cols
	}
	out := make([]string, 0, len(cols))
	for _, c := range cols {
		if !m.Denied(c) {
			out = append(out, c)
		}
	}
	return out
}

// CheckParams rejects a list query that filters, sorts, groups or aggregates
// by a denied column — of the model itself, of one of its extensions, or of
// a related model reached by a relation filter.
func (m *ReadMask) CheckParams(p query.Params) error {
	if m == nil {
		return nil
	}
	for col := range p.Filters {
		if err := m.Check(col, "filter"); err != nil {
			return err
		}
	}
	if p.SortBy != "" {
		if err := m.Check(strings.TrimPrefix(p.SortBy, "-"), "sort"); err != nil {
			return err
		}
	}
	for _, g := range p.GroupBy {
		if err := m.Check(g, "group_by"); err != nil {
			return err
		}
	}
	for _, a := range p.Aggregations {
		if err := m.Check(a.Field, "aggregate"); err != nil {
			return err
		}
	}
	for _, rf := range p.RelationFilters {
		// An extension column ("<Ext>.<column>") is part of this model's mask.
		if err := m.Check(rf.Relation+"."+rf.Field, "filter"); err != nil {
			return err
		}
		if rel := m.relatedMask(rf.Relation); rel != nil {
			if err := rel.Check(rf.Field, "filter"); err != nil {
				return err
			}
		}
	}
	return nil
}

func (m *ReadMask) relatedMask(name string) *ReadMask {
	if m == nil || m.related == nil || name == "" {
		return nil
	}
	return m.related(name)
}

// RedactRow returns row without the denied columns. Related records embedded
// under a relation key (?with= preloads) are redacted with the related
// model's mask. The input is never mutated: a row shared with an event
// payload or a hook keeps every column. When nothing has to be removed the
// same map is returned.
func (m *ReadMask) RedactRow(row map[string]any) map[string]any {
	out, _ := m.redact(row)
	return out
}

func (m *ReadMask) redact(row map[string]any) (map[string]any, bool) {
	if row == nil || m == nil || (len(m.denied) == 0 && m.related == nil) {
		return row, false
	}
	var out map[string]any
	ensure := func() {
		if out == nil {
			out = make(map[string]any, len(row))
			for k, v := range row {
				out[k] = v
			}
		}
	}
	for k, v := range row {
		if m.Denied(k) {
			ensure()
			delete(out, k)
			continue
		}
		rel := m.relatedMask(k)
		if rel == nil {
			continue
		}
		if nv, changed := rel.redactNested(v); changed {
			ensure()
			out[k] = nv
		}
	}
	if out == nil {
		return row, false
	}
	return out, true
}

// redactNested redacts an embedded related record or list of records.
func (m *ReadMask) redactNested(v any) (any, bool) {
	switch t := v.(type) {
	case map[string]any:
		return m.redact(t)
	case []map[string]any:
		var out []map[string]any
		for i, it := range t {
			if r, changed := m.redact(it); changed {
				if out == nil {
					out = append([]map[string]any(nil), t...)
				}
				out[i] = r
			}
		}
		if out == nil {
			return v, false
		}
		return out, true
	case []any:
		var out []any
		for i, it := range t {
			mm, ok := it.(map[string]any)
			if !ok {
				continue
			}
			if r, changed := m.redact(mm); changed {
				if out == nil {
					out = append([]any(nil), t...)
				}
				out[i] = r
			}
		}
		if out == nil {
			return v, false
		}
		return out, true
	}
	return v, false
}

// RedactRows redacts every row (see RedactRow) into a new slice.
func (m *ReadMask) RedactRows(rows []map[string]any) []map[string]any {
	if m == nil || (len(m.denied) == 0 && m.related == nil) {
		return rows
	}
	out := make([]map[string]any, len(rows))
	for i, r := range rows {
		out[i] = m.RedactRow(r)
	}
	return out
}

// TableMeta returns a copy of meta without the denied columns: dropped from
// Columns, SearchColumns, SearchRefs and DefaultSortBy. The query builder is
// bound to this copy, so even a path that forgot CheckParams cannot filter,
// sort or search by a denied column; hosts serve it as the caller's metadata
// so the client does not render a column it will never receive.
func (m *ReadMask) TableMeta(meta *modelbase.TableMetadata) *modelbase.TableMetadata {
	if meta == nil || m.Empty() {
		return meta
	}
	cp := *meta
	cp.Columns = make([]modelbase.ColumnDef, 0, len(meta.Columns))
	for _, c := range meta.Columns {
		if !m.Denied(c.Key) {
			cp.Columns = append(cp.Columns, c)
		}
	}
	cp.SearchColumns = m.Allowed(meta.SearchColumns)
	cp.SearchRefs = m.Allowed(meta.SearchRefs)
	if m.Denied(cp.DefaultSortBy) {
		cp.DefaultSortBy = ""
	}
	return &cp
}

// ColumnReadMask resolves the columns of model that user may not read. A
// system caller (NewSystemCaller) reads everything. With no permission service
// the host's RequirePermissions choice decides, like checkPerm: every gated
// column hidden when it fails closed, none otherwise. A nil user (public
// options/search) never holds a column permission.
func (s *Service) ColumnReadMask(ctx context.Context, model string, user modelbase.AuthUser) *ReadMask {
	meta, err := s.meta.GetTable(ctx, model)
	if err != nil {
		meta = nil
	}
	return s.readMask(ctx, model, meta, user, 0)
}

// maxMaskDepth bounds the related-model chain resolved for nested preloads.
const maxMaskDepth = 2

func (s *Service) readMask(ctx context.Context, model string, meta *modelbase.TableMetadata, user modelbase.AuthUser, depth int) *ReadMask {
	m := &ReadMask{model: model}
	if _, ok := user.(systemPrincipal); ok {
		return m
	}
	gated := map[string]string{} // column → permission
	if meta != nil {
		for _, c := range meta.Columns {
			if p := strings.TrimSpace(c.ReadPermission); p != "" && c.Key != "" {
				gated[c.Key] = p
			}
		}
	}
	for _, e := range s.resolveExtensions(ctx, model) {
		for _, c := range e.Columns {
			if p := strings.TrimSpace(c.ReadPermission); p != "" && c.Name != "" {
				gated[e.Key+"."+c.Name] = p
			}
		}
	}
	if len(gated) > 0 {
		held := map[string]bool{}
		for col, p := range gated {
			ok, seen := held[p]
			if !seen {
				ok = s.holdsColumnPermission(ctx, user, p)
				held[p] = ok
			}
			if !ok {
				if m.denied == nil {
					m.denied = map[string]struct{}{}
				}
				m.denied[col] = struct{}{}
			}
		}
	}
	if depth < maxMaskDepth {
		m.related = s.relatedMasks(ctx, model, user, depth)
	}
	return m
}

// holdsColumnPermission is the single permission decision of the column gate.
func (s *Service) holdsColumnPermission(ctx context.Context, user modelbase.AuthUser, perm string) bool {
	if s.columnReadChecker != nil {
		return user != nil && s.columnReadChecker(ctx, user, perm)
	}
	if s.perms == nil {
		return !s.requirePerms
	}
	if user == nil {
		return false
	}
	return s.perms.Check(ctx, user, permission.Capability(perm)) == nil
}

// relatedMasks returns a lazy resolver of the masks of model's related models,
// keyed by the relation name an embedded record or a relation filter uses
// (modelbase.RelationDef.Name, also tried in snake_case). Masks are resolved
// once per call and memoized.
func (s *Service) relatedMasks(ctx context.Context, model string, user modelbase.AuthUser, depth int) func(string) *ReadMask {
	inst, ok := s.lookupModel(ctx, model)
	if !ok {
		return nil
	}
	hr, ok := inst.(modelbase.HasRelations)
	if !ok {
		return nil
	}
	targets := map[string]string{}
	for _, r := range hr.DefineRelations() {
		if r.Name == "" || r.Through == "" {
			continue
		}
		targets[r.Name] = r.Through
		targets[toSnakeASCII(r.Name)] = r.Through
	}
	if len(targets) == 0 {
		return nil
	}
	memo := map[string]*ReadMask{}
	return func(name string) *ReadMask {
		through, ok := targets[name]
		if !ok {
			return nil
		}
		if rm, ok := memo[through]; ok {
			return rm
		}
		var rm *ReadMask
		if refModel, _, refMeta := s.resolveRefModel(ctx, through); refMeta != nil {
			rm = s.readMask(ctx, refModel, refMeta, user, depth+1)
		}
		memo[through] = rm
		return rm
	}
}

// RedactEvent returns a copy of ev whose Before/After carry only the columns
// user may read, for a host that forwards canonical events to a client
// (websocket push, outbound webhook). A nil user — a delivery with no
// principal, such as an org-level webhook — receives no gated column at all.
// The original event is never mutated.
func (s *Service) RedactEvent(ctx context.Context, ev *CanonicalEvent, user modelbase.AuthUser) *CanonicalEvent {
	if ev == nil {
		return nil
	}
	m := s.ColumnReadMask(ctx, ev.Model, user)
	cp := *ev
	cp.Before = m.RedactRow(ev.Before)
	cp.After = m.RedactRow(ev.After)
	return &cp
}

// RedactRecord returns row as user may read it — the helper for host
// endpoints that serve records of a dynamic model outside this package.
func (s *Service) RedactRecord(ctx context.Context, model string, user modelbase.AuthUser, row map[string]any) map[string]any {
	return s.ColumnReadMask(ctx, model, user).RedactRow(row)
}

// extensions returns exts without the denied extension columns, so the list
// builder can neither filter by them nor match their search keys.
func (m *ReadMask) extensions(exts []ExtensionTable) []ExtensionTable {
	if m.Empty() || len(exts) == 0 {
		return exts
	}
	out := make([]ExtensionTable, 0, len(exts))
	for _, e := range exts {
		cp := e
		cp.Columns = cp.Columns[:0:0]
		for _, c := range e.Columns {
			if !m.Denied(e.Key + "." + c.Name) {
				cp.Columns = append(cp.Columns, c)
			}
		}
		out = append(out, cp)
	}
	return out
}

// deniedPath reports whether a dotted search path ("customer.cost") reaches a
// denied column: on this model, or through a declared relation on the
// related model.
func (m *ReadMask) deniedPath(path string) bool {
	if m.Denied(path) {
		return true
	}
	i := strings.IndexByte(path, '.')
	if i <= 0 {
		return false
	}
	if rel := m.relatedMask(path[:i]); rel != nil {
		return rel.deniedPath(path[i+1:])
	}
	return false
}

// maskSearchConfig returns a copy of cfg that never matches, orders by or
// projects a denied column. A denied label falls back to the value column.
func maskSearchConfig(m *ReadMask, cfg *SearchConfig) *SearchConfig {
	if m.Empty() && (m == nil || m.related == nil) {
		return cfg
	}
	cp := *cfg
	cp.SearchIn = nil
	for _, f := range cfg.SearchIn {
		if !m.deniedPath(f) {
			cp.SearchIn = append(cp.SearchIn, f)
		}
	}
	label := cp.Label
	if label == "" {
		label = "name"
	}
	if m.Denied(label) {
		cp.Label = cp.Value
		if cp.Label == "" {
			cp.Label = "id"
		}
	}
	if m.Denied(cp.Description) {
		cp.Description = ""
	}
	if m.Denied(cp.Image) {
		cp.Image = ""
	}
	if m.Denied(cp.Icon) {
		cp.Icon = ""
	}
	if m.Denied(cp.OrderBy) {
		cp.OrderBy = ""
	}
	cp.ExtraFields = m.Allowed(cfg.ExtraFields)
	return &cp
}

// maskOptionsConfig is maskSearchConfig for a dynamic options field: a denied
// label falls back to the value column, a denied description/image/order/extra
// column is dropped. FilterBy is checked by the caller (it is refused, not
// dropped, when the client sends a filter value).
func maskOptionsConfig(m *ReadMask, cfg FieldOptionsConfig) FieldOptionsConfig {
	if m.Empty() {
		return cfg
	}
	label := cfg.Label
	if label == "" {
		label = "name"
	}
	if m.Denied(label) {
		cfg.Label = cfg.Value
		if cfg.Label == "" {
			cfg.Label = "id"
		}
	}
	if m.Denied(cfg.Description) {
		cfg.Description = ""
	}
	if m.Denied(cfg.Image) {
		cfg.Image = ""
	}
	if m.Denied(cfg.OrderBy) {
		cfg.OrderBy = ""
	}
	cfg.ExtraColumns = m.Allowed(cfg.ExtraColumns)
	return cfg
}
