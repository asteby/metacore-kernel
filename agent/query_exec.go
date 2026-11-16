package agent

import (
	"context"
	"fmt"
	"strings"
	"time"

	"gorm.io/gorm"
)

// QueryTreeOptions tune CRUDService.QueryTree. Every field is optional.
type QueryTreeOptions struct {
	// Catalog supplies the typed models (labels, relations, enums). Without
	// it the model is built from the CRUD catalog on each call.
	Catalog *CatalogIndex
	// Labels localizes labels when Catalog is nil.
	Labels CatalogLabeler
	// MaxRows caps the rows of the rendered table (default 20).
	MaxRows int
	// MaxBytes caps the rendered text (default DefaultCompactMaxBytes).
	MaxBytes int
	// NoRefNames skips the many2one name lookup ("Name [id]").
	NoRefNames bool
}

// AggregateResult is the outcome of a query tree with aggregates.
type AggregateResult struct {
	Model string           `json:"model"`
	Rows  []map[string]any `json:"rows"`
	Text  string           `json:"-"`
}

// LLMText implements tools.LLMTexter.
func (a *AggregateResult) LLMText() string {
	if a == nil {
		return ""
	}
	return a.Text
}

// catalogModel returns the typed model of a resolved key.
func (s *CRUDService) catalogModel(key string, opt QueryTreeOptions) (CatalogModel, bool) {
	if opt.Catalog != nil {
		if m, ok := opt.Catalog.Model(key); ok {
			return m, true
		}
	}
	meta, ok := s.cfg.Catalog.Lookup(key)
	if !ok {
		return CatalogModel{}, false
	}
	return BuildCatalogModel(s.cfg.Catalog, meta, opt.Labels), true
}

// QueryTree runs a validated read: rows (*QueryResult) or, with aggregates
// or group_by, an *AggregateResult. It is permission-checked and org-scoped
// exactly like Query, refuses columns the caller may not read, and renders a
// compact text for the model (Text) alongside the structured value.
func (s *CRUDService) QueryTree(ctx context.Context, identity *AgentIdentity, t QueryTree, opt QueryTreeOptions) (any, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	raw := strings.TrimSpace(t.Model)
	if raw == "" {
		return nil, fmt.Errorf("model es requerido (usa catalog_search para encontrarlo)")
	}
	model := s.resolve(raw)
	if err := guardResolvedModel(raw, model); err != nil {
		return nil, err
	}
	table := s.tableName(model)
	if table == "" {
		return nil, s.unknownModelError(raw, opt)
	}
	if err := s.CheckPermission(identity, model, "index"); err != nil {
		return nil, err
	}
	cm, ok := s.catalogModel(model, opt)
	if !ok {
		return nil, s.unknownModelError(raw, opt)
	}
	t, err := ValidateQueryTree(cm, t)
	if err != nil {
		return nil, err
	}
	mask := s.readMaskFor(identity, model)
	for _, c := range t.Where {
		if err := mask.check(model, c.Field, "filter"); err != nil {
			return nil, err
		}
	}
	for _, f := range append(append([]string(nil), t.Fields...), t.GroupBy...) {
		if err := mask.check(model, f, "select"); err != nil {
			return nil, err
		}
	}
	for _, a := range t.Aggregates {
		if err := mask.check(model, a.Field, "aggregate"); err != nil {
			return nil, err
		}
	}
	for _, term := range strings.Split(t.Order, ",") {
		if f := strings.Fields(term); len(f) > 0 {
			if err := mask.check(model, f[0], "order"); err != nil {
				return nil, err
			}
		}
	}
	cm = cm.withoutFields(mask)

	base := func() *gorm.DB {
		q := s.cfg.DB.WithContext(ctx).Table(table).
			Where("organization_id = ?", identity.OrganizationID).
			Where("deleted_at IS NULL")
		for _, c := range t.Where {
			q = applyCondition(q, cm, c)
		}
		if t.Search != "" {
			cols := cm.SearchIn
			if len(cols) == 0 {
				cols = s.searchColumns(model)
			}
			var conds []string
			var args []any
			for _, col := range mask.allowed(cols) {
				if isValidField(col) {
					conds = append(conds, fmt.Sprintf("LOWER(%s) LIKE ?", col))
					args = append(args, "%"+strings.ToLower(t.Search)+"%")
				}
			}
			if len(conds) > 0 {
				q = q.Where("("+strings.Join(conds, " OR ")+")", args...)
			}
		}
		return q
	}

	maxBytes := opt.MaxBytes
	if maxBytes <= 0 {
		maxBytes = DefaultCompactMaxBytes
	}

	if len(t.Aggregates) > 0 {
		sel := append([]string(nil), t.GroupBy...)
		for _, a := range t.Aggregates {
			sel = append(sel, aggregateSQL(a)+" AS "+a.As)
		}
		q := base().Select(strings.Join(sel, ", "))
		if len(t.GroupBy) > 0 {
			q = q.Group(strings.Join(t.GroupBy, ", "))
			order := t.Order
			if order == "" {
				order = t.Aggregates[0].As + " desc"
			}
			q = q.Order(order).Limit(t.Limit).Offset(t.Offset)
		}
		var rows []map[string]any
		if err := q.Find(&rows).Error; err != nil {
			return nil, fmt.Errorf("aggregate failed: %w", err)
		}
		derefRows(rows)
		var refs map[string]map[string]string
		if !opt.NoRefNames {
			refs = s.refNames(ctx, identity, cm, t.GroupBy, rows, opt)
		}
		return &AggregateResult{Model: model, Rows: rows, Text: RenderAggregateTable(cm, t, rows, refs, maxBytes)}, nil
	}

	q := base()
	if len(t.Fields) > 0 {
		q = q.Select(strings.Join(appendUnique(append([]string{"id"}, t.Fields...), "id"), ", "))
	}
	if t.Order != "" {
		q = q.Order(t.Order)
	} else {
		q = q.Order("created_at DESC")
	}
	var rows []map[string]any
	if err := q.Limit(t.Limit).Offset(t.Offset).Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("query failed: %w", err)
	}
	derefRows(rows)
	for i := range rows {
		stripSensitive(rows[i])
		mask.redact(rows[i])
	}
	total := t.Offset + len(rows)
	if len(rows) == t.Limit {
		var n int64
		if err := base().Count(&n).Error; err == nil {
			total = int(n)
		}
	}
	var refs map[string]map[string]string
	if !opt.NoRefNames {
		cols := t.Fields
		if len(cols) == 0 {
			for _, f := range cm.Fields {
				if f.Rel == "m2o" {
					cols = append(cols, f.Name)
				}
			}
		}
		refs = s.refNames(ctx, identity, cm, cols, rows, opt)
	}
	var text string
	if len(rows) == 1 && t.Offset == 0 && total == 1 && len(t.Fields) == 0 {
		text = RenderRecordKV(cm, rows[0], refs, maxBytes)
	} else {
		text = RenderRowsTable(cm, rows, TableOptions{Columns: t.Fields, MaxRows: opt.MaxRows, MaxBytes: maxBytes, Total: total, Offset: t.Offset, RefNames: refs})
	}
	return &QueryResult{Model: model, Count: total, Records: rows, Text: text}, nil
}

// unknownModelError suggests the closest models from the catalog.
func (s *CRUDService) unknownModelError(raw string, opt QueryTreeOptions) error {
	msg := fmt.Sprintf("modelo %q no existe", raw)
	if opt.Catalog != nil {
		var names []string
		for _, h := range opt.Catalog.Search(context.Background(), raw, 3, nil) {
			names = append(names, fmt.Sprintf("`%s` (%s)", h.Model.Name, h.Model.Label))
		}
		if len(names) > 0 {
			msg += "; ¿quisiste decir " + strings.Join(names, " o ") + "?"
			return fmt.Errorf("%s", msg)
		}
	}
	return fmt.Errorf("%s; usa catalog_search para encontrar el modelo", msg)
}

func aggregateSQL(a QueryAggregate) string {
	switch a.Fn {
	case "count":
		if a.Field == "" {
			return "COUNT(*)"
		}
		return "COUNT(" + a.Field + ")"
	case "count_distinct":
		return "COUNT(DISTINCT " + a.Field + ")"
	}
	return strings.ToUpper(a.Fn) + "(" + a.Field + ")"
}

// applyCondition adds one validated condition. Field names were checked
// against the model metadata (and isValidField) by ValidateQueryTree; values
// are always bound parameters. A bare date compared with a date-time field
// covers the whole day.
func applyCondition(q *gorm.DB, m CatalogModel, c QueryCondition) *gorm.DB {
	col := c.Field
	if !isValidField(col) {
		return q.Where("1 = 0")
	}
	f, _ := m.field(col)
	isDT := f.Type == "dt" || col == "created_at" || col == "updated_at"
	switch c.Op {
	case "is null":
		return q.Where(col + " IS NULL")
	case "is not null":
		return q.Where(col + " IS NOT NULL")
	case "in":
		return q.Where(col+" IN ?", c.Value)
	case "not in":
		return q.Where(col+" NOT IN ?", c.Value)
	case "like":
		return q.Where(col+" LIKE ?", c.Value)
	case "ilike":
		return q.Where("LOWER("+col+") LIKE ?", strings.ToLower(fmt.Sprint(c.Value)))
	case "between":
		list, _ := c.Value.([]any)
		if len(list) != 2 {
			return q.Where("1 = 0")
		}
		lo, hi := list[0], list[1]
		if isDT {
			if d, ok := bareDate(hi); ok {
				return q.Where(col+" >= ? AND "+col+" < ?", lo, d.AddDate(0, 0, 1).Format("2006-01-02"))
			}
		}
		return q.Where(col+" BETWEEN ? AND ?", lo, hi)
	}
	if isDT {
		if d, ok := bareDate(c.Value); ok {
			day, next := d.Format("2006-01-02"), d.AddDate(0, 0, 1).Format("2006-01-02")
			switch c.Op {
			case "=":
				return q.Where(col+" >= ? AND "+col+" < ?", day, next)
			case "!=":
				return q.Where("("+col+" < ? OR "+col+" >= ?)", day, next)
			case "<=":
				return q.Where(col+" < ?", next)
			case ">":
				return q.Where(col+" >= ?", next)
			}
		}
	}
	sqlOp := c.Op
	if sqlOp == "!=" {
		sqlOp = "<>"
	}
	return q.Where(col+" "+sqlOp+" ?", c.Value)
}

func bareDate(v any) (time.Time, bool) {
	s, ok := v.(string)
	if !ok || len(s) != 10 {
		return time.Time{}, false
	}
	d, err := time.Parse("2006-01-02", s)
	return d, err == nil
}

// refLabelColumns are tried, in order, as the display name of a referenced
// record.
var refLabelColumns = []string{"name", "title", "display_name", "full_name", "label", "folio", "code", "number", "reference", "sku", "email"}

// refNames resolves many2one ids in cols to display names, per column, with
// one query per referenced model. Models the caller may not read, columns it
// may not see and tables that are not org-scoped are skipped (the cell then
// shows the bare id).
func (s *CRUDService) refNames(ctx context.Context, identity *AgentIdentity, m CatalogModel, cols []string, rows []map[string]any, opt QueryTreeOptions) map[string]map[string]string {
	if len(rows) == 0 || s.cfg.DB == nil {
		return nil
	}
	out := map[string]map[string]string{}
	for _, col := range cols {
		f, ok := m.field(col)
		if !ok || f.Rel != "m2o" || f.Ref == "" {
			continue
		}
		ids := map[string]bool{}
		var list []string
		for _, r := range rows {
			id := scalarText(r[col])
			if id != "" && !ids[id] {
				ids[id] = true
				list = append(list, id)
			}
		}
		if len(list) == 0 {
			continue
		}
		ref := s.resolve(f.Ref)
		table := s.tableName(ref)
		if table == "" || s.CheckPermission(identity, ref, "index") != nil {
			continue
		}
		rm, ok := s.catalogModel(ref, opt)
		if !ok {
			continue
		}
		mask := s.readMaskFor(identity, ref)
		label := ""
		for _, c := range refLabelColumns {
			if _, has := rm.field(c); has && !mask.denied(c) {
				label = c
				break
			}
		}
		if label == "" {
			continue
		}
		var found []map[string]any
		err := s.cfg.DB.WithContext(ctx).Table(table).Select("id, "+label).
			Where("id IN ?", list).
			Where("organization_id = ?", identity.OrganizationID).
			Find(&found).Error
		if err != nil {
			continue
		}
		names := map[string]string{}
		for _, r := range found {
			if n := strings.TrimSpace(scalarText(r[label])); n != "" {
				names[scalarText(r["id"])] = n
			}
		}
		if len(names) > 0 {
			out[col] = names
		}
	}
	return out
}
