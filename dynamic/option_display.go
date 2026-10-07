package dynamic

// option_display.go — server-side resolution of the declarative option
// display (manifest v3 models[].option_display + option_metrics[]).
//
// The options endpoint already fetched the page of source rows; the display is
// computed from those rows (title / subtitle / image / badges / field metrics:
// zero extra queries) plus ONE grouped aggregate query per contributed metric
// the display references (stock available = SUM(stock.available) GROUP BY
// product_id over the page's ids) — never one query per option. Every metric
// query is scoped to the caller's organization, honours the aggregated
// model's access policy and soft delete, and binds every value as a parameter.

import (
	"context"
	"fmt"
	"log"
	"math"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"

	v3 "github.com/asteby/metacore-kernel/manifest/v3"
	"github.com/asteby/metacore-kernel/modelbase"
)

// OptionDisplaySpec is what a host hands the kernel for one source model: its
// declared display and the metrics contributed to it by the addons ENABLED for
// the request's organization. In each metric, Model and Scope[].Through.Model
// are model names the Service's ModelResolver resolves (the host maps the
// addon's model key to its registered name — typically the table).
type OptionDisplaySpec struct {
	Display *v3.OptionDisplay
	Metrics []v3.OptionMetric
}

// OptionDisplayResolver returns the option display of a model (the options
// Source). ok=false or a nil Display = plain options. Host-wired like
// ExtensionResolver; it must filter Metrics to the org's enabled addons.
type OptionDisplayResolver func(ctx context.Context, model string) (OptionDisplaySpec, bool)

// OptionDisplayValue is the resolved, ready-to-paint display of one option,
// served as Option.Display.
type OptionDisplayValue struct {
	Title    string               `json:"title,omitempty"`
	Subtitle string               `json:"subtitle,omitempty"`
	Image    any                  `json:"image,omitempty"`
	Trailing []OptionTrailingItem `json:"trailing,omitempty"`
	Badges   []OptionBadgeItem    `json:"badges,omitempty"`
	// Tone is the most severe tone among the trailing items (row accent).
	Tone string `json:"tone,omitempty"`
	// Dimmed: a matched tone asked to attenuate the row (still selectable).
	Dimmed bool `json:"dimmed,omitempty"`
	// Blocked: a matched tone made the option not selectable (UI hint).
	Blocked bool `json:"blocked,omitempty"`
}

// OptionTrailingItem is one resolved right-aligned metric.
type OptionTrailingItem struct {
	Key      string `json:"key"`
	Label    string `json:"label,omitempty"`
	Value    any    `json:"value"`
	Format   string `json:"format,omitempty"`
	Currency string `json:"currency,omitempty"`
	Tone     string `json:"tone,omitempty"`
	// Text replaces the formatted value when a tone declares one ("Agotado").
	Text string `json:"text,omitempty"`
}

// OptionBadgeItem is one resolved badge.
type OptionBadgeItem struct {
	Text string `json:"text"`
	Tone string `json:"tone,omitempty"`
}

// maxOptionContext bounds OptionsQuery.Context.
const (
	maxOptionContextKeys  = 16
	maxOptionContextValue = 128
)

var optionContextKeyRe = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// OptionContextFromQuery reads the picker context of an options request:
// `?ctx.<key>=<value>` or `?ctx[<key>]=<value>` (e.g. ctx.warehouse_id=…).
// Invalid keys, empty or oversized values are dropped; at most 16 keys.
// Exported so a host with its own options handler parses it like the kernel.
func OptionContextFromQuery(c fiber.Ctx) map[string]string {
	var out map[string]string
	c.Request().URI().QueryArgs().VisitAll(func(k, v []byte) {
		key := string(k)
		switch {
		case strings.HasPrefix(key, "ctx."):
			key = strings.TrimPrefix(key, "ctx.")
		case strings.HasPrefix(key, "ctx[") && strings.HasSuffix(key, "]"):
			key = key[4 : len(key)-1]
		default:
			return
		}
		val := strings.TrimSpace(string(v))
		if !optionContextKeyRe.MatchString(key) || val == "" || len(val) > maxOptionContextValue {
			return
		}
		if out == nil {
			out = map[string]string{}
		}
		if len(out) >= maxOptionContextKeys {
			return
		}
		out[key] = val
	})
	return out
}

// applyOptionDisplay stamps Display on every option of a page. rows are the
// source rows the options were projected from (index-aligned with items).
func (s *Service) applyOptionDisplay(ctx context.Context, user modelbase.AuthUser, source string, rows reflect.Value, items []Option, qctx map[string]string) {
	if s.optionDisplays == nil || len(items) == 0 || !rows.IsValid() || rows.Len() != len(items) {
		return
	}
	spec, ok := s.optionDisplays(ctx, source)
	if !ok || spec.Display == nil {
		return
	}
	d := spec.Display

	// Metrics the display references, provided by an enabled addon.
	provided := map[string]v3.OptionMetric{}
	for _, m := range spec.Metrics {
		if _, dup := provided[m.Key]; !dup && m.Key != "" {
			provided[m.Key] = m
		}
	}
	needed := map[string]struct{}{}
	need := func(k string) {
		if _, ok := provided[k]; ok && k != "" {
			needed[k] = struct{}{}
		}
	}
	for _, t := range d.Trailing {
		need(t.Metric)
		for _, tn := range t.Tones {
			need(tn.When.RefMetric)
		}
	}
	for _, b := range d.Badges {
		if b.When != nil {
			need(b.When.RefMetric)
		}
	}

	rowAt := func(i int) reflect.Value {
		r := rows.Index(i)
		if r.Kind() == reflect.Ptr {
			r = r.Elem()
		}
		return r
	}
	ids := make([]string, 0, len(items))
	for i := range items {
		if id := optionRowID(rowAt(i)); id != "" {
			ids = append(ids, id)
		}
	}
	metricValues := map[string]map[string]float64{} // metric → row id → value
	for k := range needed {
		vals, err := s.queryOptionMetric(ctx, user, provided[k], ids, qctx)
		if err != nil {
			log.Printf("dynamic: option metric %q for %q skipped: %v", k, source, err)
			continue
		}
		if vals != nil {
			metricValues[k] = vals
		}
	}
	metricFor := func(key, id string) (any, bool) {
		vals, ok := metricValues[key]
		if !ok {
			return nil, false
		}
		if v, ok := vals[id]; ok {
			return v, true
		}
		// No aggregated rows for this option: an additive aggregate is 0 (a
		// product with no stock rows has nothing available), the others are
		// undefined.
		switch provided[key].Aggregate {
		case "sum", "count":
			return float64(0), true
		}
		return nil, true
	}

	for i := range items {
		row := rowAt(i)
		id := optionRowID(row)
		col := func(name string) any { return displayScalar(fieldValue(row, name)) }
		dv := &OptionDisplayValue{}
		if d.Title != "" {
			dv.Title = renderOptionTemplate(d.Title, col)
		}
		if dv.Title == "" {
			dv.Title = scalarString(displayScalar(items[i].Label))
		}
		var parts []string
		for _, p := range d.Subtitle {
			if txt := renderOptionTemplate(p, col); txt != "" {
				parts = append(parts, txt)
			}
		}
		dv.Subtitle = strings.Join(parts, " · ")
		if d.Image != "" {
			if img := col(d.Image); img != nil && img != "" {
				dv.Image = img
				if items[i].Image == nil || items[i].Image == "" {
					items[i].Image = img
				}
			}
		}
		if dv.Image == nil && items[i].Image != nil && items[i].Image != "" {
			dv.Image = items[i].Image
		}
		refValue := func(c v3.OptionDisplayCondition) (any, bool) {
			switch {
			case c.Ref != "":
				return col(c.Ref), true
			case c.RefMetric != "":
				return metricFor(c.RefMetric, id)
			default:
				return c.Value, true
			}
		}
		for _, t := range d.Trailing {
			var val any
			if t.Metric != "" {
				v, ok := metricFor(t.Metric, id)
				if !ok {
					continue // not provided (addon absent) or unavailable
				}
				val = v
			} else {
				val = col(t.Field)
			}
			item := OptionTrailingItem{Key: t.Key, Label: t.Label, Value: val, Format: t.Format}
			if t.CurrencyField != "" {
				item.Currency = scalarString(col(t.CurrencyField))
			}
			for _, tn := range t.Tones {
				rhs, ok := refValue(tn.When)
				if !ok || !evalOptionCondition(tn.When.Op, val, rhs) {
					continue
				}
				item.Tone, item.Text = tn.Tone, tn.Text
				dv.Dimmed = dv.Dimmed || tn.Dim
				dv.Blocked = dv.Blocked || tn.Block
				break
			}
			if val == nil && item.Text == "" {
				continue
			}
			if toneRank(item.Tone) > toneRank(dv.Tone) {
				dv.Tone = item.Tone
			}
			dv.Trailing = append(dv.Trailing, item)
		}
		for _, b := range d.Badges {
			var fv any
			if b.Field != "" {
				fv = col(b.Field)
			}
			if b.When != nil {
				tested := fv
				if b.When.Field != "" {
					tested = col(b.When.Field)
				}
				rhs, ok := refValue(*b.When)
				if !ok || !evalOptionCondition(b.When.Op, tested, rhs) {
					continue
				}
			} else if b.Field != "" && isEmptyDisplayValue(fv) {
				continue
			}
			text, tone := b.Text, b.Tone
			if b.Field != "" && len(b.Values) > 0 {
				if bv, ok := lookupBadgeValue(b.Values, scalarString(fv)); ok {
					if bv.Text != "" {
						text = bv.Text
					}
					if bv.Tone != "" {
						tone = bv.Tone
					}
				}
			}
			if text == "" {
				text = scalarString(fv)
			}
			if text == "" {
				continue
			}
			dv.Badges = append(dv.Badges, OptionBadgeItem{Text: text, Tone: tone})
		}
		items[i].Display = dv
	}
}

// queryOptionMetric runs ONE grouped aggregate for a page of option ids. A nil
// map with a nil error means the metric is unavailable for this request (access
// denied, a context value that cannot match the column): the item is omitted
// rather than showing a number computed over the wrong scope.
func (s *Service) queryOptionMetric(ctx context.Context, user modelbase.AuthUser, m v3.OptionMetric, ids []string, qctx map[string]string) (map[string]float64, error) {
	if len(ids) == 0 {
		return map[string]float64{}, nil
	}
	inst, ok := s.lookupModel(ctx, m.Model)
	if !ok {
		return nil, fmt.Errorf("model %q not found", m.Model)
	}
	if err := s.checkAccess(ctx, user, m.Model, inst, modelbase.AccessList); err != nil {
		return nil, nil
	}
	table, err := s.tableNameFor(ctx, m.Model, inst)
	if err != nil {
		return nil, err
	}
	if !safeColumn.MatchString(m.ForeignKey) {
		return nil, fmt.Errorf("unsafe foreign_key %q", m.ForeignKey)
	}
	var aggExpr string
	switch strings.ToLower(m.Aggregate) {
	case "count":
		if m.Column == "" {
			aggExpr = "COUNT(*)"
		} else if safeColumn.MatchString(m.Column) {
			aggExpr = fmt.Sprintf("COUNT(%s)", m.Column)
		}
	case "sum", "min", "max", "avg":
		if safeColumn.MatchString(m.Column) {
			aggExpr = fmt.Sprintf("%s(%s)", strings.ToUpper(m.Aggregate), m.Column)
		}
	}
	if aggExpr == "" {
		return nil, fmt.Errorf("invalid aggregate %q(%q)", m.Aggregate, m.Column)
	}

	if columnIsUUID(inst, m.ForeignKey) {
		valid := make([]string, 0, len(ids))
		for _, id := range ids {
			if u, err := uuid.Parse(id); err == nil {
				valid = append(valid, u.String())
			}
		}
		ids = valid
		if len(ids) == 0 {
			return map[string]float64{}, nil
		}
	}

	db := s.db.WithContext(ctx).Table(table)
	db, err = s.scopeOrDeny(db, inst, user)
	if err != nil {
		return nil, err
	}
	db = scopeSoftDelete(db, inst)
	db = db.Where(fmt.Sprintf("%s IN ?", m.ForeignKey), ids)
	for _, k := range sortedKeys(m.Where) {
		if !safeColumn.MatchString(k) {
			return nil, fmt.Errorf("unsafe where column %q", k)
		}
		if v := m.Where[k]; v == nil {
			db = db.Where(fmt.Sprintf("%s IS NULL", k))
		} else {
			db = db.Where(fmt.Sprintf("%s = ?", k), v)
		}
	}
	for _, sc := range m.Scope {
		val := strings.TrimSpace(qctx[sc.Context])
		if val == "" {
			continue
		}
		if !safeColumn.MatchString(sc.Column) {
			return nil, fmt.Errorf("unsafe scope column %q", sc.Column)
		}
		if sc.Through == nil {
			if columnIsUUID(inst, sc.Column) {
				u, err := uuid.Parse(val)
				if err != nil {
					return nil, nil
				}
				val = u.String()
			}
			db = db.Where(fmt.Sprintf("%s = ?", sc.Column), val)
			continue
		}
		tinst, ok := s.lookupModel(ctx, sc.Through.Model)
		if !ok {
			return nil, fmt.Errorf("through model %q not found", sc.Through.Model)
		}
		if !safeColumn.MatchString(sc.Through.Column) {
			return nil, fmt.Errorf("unsafe through column %q", sc.Through.Column)
		}
		if columnIsUUID(tinst, sc.Through.Column) {
			u, err := uuid.Parse(val)
			if err != nil {
				return nil, nil
			}
			val = u.String()
		}
		ttable, err := s.tableNameFor(ctx, sc.Through.Model, tinst)
		if err != nil {
			return nil, err
		}
		sub := s.db.WithContext(ctx).Table(ttable).Select("id")
		sub, err = s.scopeOrDeny(sub, tinst, user)
		if err != nil {
			return nil, err
		}
		sub = scopeSoftDelete(sub, tinst).Where(fmt.Sprintf("%s = ?", sc.Through.Column), val)
		db = db.Where(fmt.Sprintf("%s IN (?)", sc.Column), sub)
	}

	var got []struct {
		K string
		V *float64
	}
	err = db.Select(fmt.Sprintf("CAST(%s AS TEXT) AS k, %s AS v", m.ForeignKey, aggExpr)).
		Group(m.ForeignKey).Scan(&got).Error
	if err != nil {
		return nil, err
	}
	out := make(map[string]float64, len(got))
	for _, g := range got {
		if g.V == nil {
			continue
		}
		out[strings.ToLower(g.K)] = *g.V
	}
	return out, nil
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// optionRowID is the row's id as the canonical lower-case string the metric
// query groups by.
func optionRowID(row reflect.Value) string {
	v := fieldValue(row, "id")
	if v == nil {
		return ""
	}
	return strings.ToLower(scalarString(displayScalar(v)))
}

// displayScalar unwraps a struct field value into a JSON scalar: pointers are
// dereferenced, named basic kinds unwrapped, time as RFC 3339, uuid/decimal via
// String(). Non-scalars (maps, slices) yield nil.
func displayScalar(v any) any {
	if v == nil {
		return nil
	}
	switch t := v.(type) {
	case time.Time:
		if t.IsZero() {
			return nil
		}
		return t.Format(time.RFC3339)
	case *time.Time:
		if t == nil || t.IsZero() {
			return nil
		}
		return t.Format(time.RFC3339)
	}
	rv := reflect.ValueOf(v)
	for rv.Kind() == reflect.Ptr {
		if rv.IsNil() {
			return nil
		}
		rv = rv.Elem()
		if t, ok := rv.Interface().(time.Time); ok {
			return displayScalar(t)
		}
	}
	switch rv.Kind() {
	case reflect.String:
		return rv.String()
	case reflect.Bool:
		return rv.Bool()
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return rv.Int()
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return rv.Uint()
	case reflect.Float32, reflect.Float64:
		return rv.Float()
	}
	if st, ok := rv.Interface().(fmt.Stringer); ok {
		return st.String()
	}
	return nil
}

func scalarString(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case float32:
		return strconv.FormatFloat(float64(t), 'f', -1, 32)
	}
	return fmt.Sprint(v)
}

var optionPlaceholder = regexp.MustCompile(`\{([a-z][a-z0-9_]*)\}`)

// renderOptionTemplate renders a title/subtitle part: a bare column name is
// that column's value; otherwise {column} placeholders are substituted and the
// part is dropped ("") when every placeholder resolved empty.
func renderOptionTemplate(part string, col func(string) any) string {
	p := strings.TrimSpace(part)
	if optionContextKeyRe.MatchString(p) {
		return strings.TrimSpace(scalarString(col(p)))
	}
	filled := false
	out := optionPlaceholder.ReplaceAllStringFunc(p, func(m string) string {
		v := strings.TrimSpace(scalarString(col(m[1 : len(m)-1])))
		if v != "" {
			filled = true
		}
		return v
	})
	if !filled {
		return ""
	}
	return strings.TrimSpace(out)
}

func isEmptyDisplayValue(v any) bool {
	if v == nil {
		return true
	}
	if s, ok := v.(string); ok {
		return strings.TrimSpace(s) == ""
	}
	return false
}

func displayNumber(v any) (float64, bool) {
	switch t := v.(type) {
	case float64:
		return t, !math.IsNaN(t)
	case float32:
		return float64(t), true
	case int64:
		return float64(t), true
	case int:
		return float64(t), true
	case int32:
		return float64(t), true
	case uint64:
		return float64(t), true
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(t), 64)
		return f, err == nil
	}
	return 0, false
}

// evalOptionCondition compares lhs against rhs: numerically when both are
// numbers (or numeric strings), else as trimmed case-insensitive strings
// (lexical order for lt/gt — correct for ISO dates).
func evalOptionCondition(op string, lhs, rhs any) bool {
	switch op {
	case "empty":
		return isEmptyDisplayValue(lhs)
	case "not_empty":
		return !isEmptyDisplayValue(lhs)
	}
	if lhs == nil || rhs == nil {
		return false
	}
	var cmp int
	if a, ok := displayNumber(lhs); ok {
		if b, ok := displayNumber(rhs); ok {
			switch {
			case a < b:
				cmp = -1
			case a > b:
				cmp = 1
			}
			return applyCmp(op, cmp)
		}
	}
	a := strings.ToLower(strings.TrimSpace(scalarString(lhs)))
	b := strings.ToLower(strings.TrimSpace(scalarString(rhs)))
	cmp = strings.Compare(a, b)
	return applyCmp(op, cmp)
}

func applyCmp(op string, cmp int) bool {
	switch op {
	case "lt":
		return cmp < 0
	case "lte":
		return cmp <= 0
	case "gt":
		return cmp > 0
	case "gte":
		return cmp >= 0
	case "eq":
		return cmp == 0
	case "neq":
		return cmp != 0
	}
	return false
}

func toneRank(t string) int {
	switch t {
	case "danger":
		return 5
	case "warning":
		return 4
	case "success":
		return 3
	case "info":
		return 2
	case "neutral":
		return 1
	}
	return 0
}

func lookupBadgeValue(values map[string]v3.OptionBadgeValue, key string) (v3.OptionBadgeValue, bool) {
	if v, ok := values[key]; ok {
		return v, true
	}
	for k, v := range values {
		if strings.EqualFold(strings.TrimSpace(k), strings.TrimSpace(key)) {
			return v, true
		}
	}
	return v3.OptionBadgeValue{}, false
}
