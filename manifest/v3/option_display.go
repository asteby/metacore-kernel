package v3

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// OptionDisplay declares HOW a model's rows read as options of a picker: the
// title, a muted subtitle, a leading image, right-aligned metrics with their
// format and a declarative tone, and badges. The options endpoint resolves it
// server-side on every option it returns (listing and `?ids=` resolve mode) as
// a ready-to-paint `display` object, so every picker of the model — a form's
// dynamic_select, the document editor's product cell, a custom addon screen —
// shows the same rich row with zero per-screen code. Nil = the plain
// {label, image, description} option (the legacy behaviour).
//
// Values come from two places:
//
//   - `field`: a column of the row itself (price, sku, folio, total, status);
//   - `metric`: a value COMPUTED by another addon and contributed through its
//     top-level option_metrics[] (stock available = sum of inventory stock rows
//     of the product, optionally in the warehouse / branch of the document).
//     A trailing item whose metric no installed+enabled addon provides is
//     simply omitted — products declares the stock column once and works the
//     same with or without inventory installed.
type OptionDisplay struct {
	// Title of the option. A column name ("name") or a template with {column}
	// placeholders ("{folio} · {customer_name}"). Empty = the option's label.
	Title string `json:"title,omitempty"`
	// Subtitle parts joined with " · ". Each part is a column name ("sku") or a
	// template ("RFC {tax_id}"); a part whose placeholders all resolve empty is
	// dropped together with its separator. Empty = no subtitle.
	Subtitle []string `json:"subtitle,omitempty"`
	// Image names the column holding the option's image url (32px avatar).
	// Empty = the option's existing image (if any); the SDK falls back to
	// initials.
	Image string `json:"image,omitempty"`
	// Trailing are the right-aligned metrics, in order.
	Trailing []OptionTrailing `json:"trailing,omitempty"`
	// Badges are small chips next to the title (status, kind…).
	Badges []OptionBadge `json:"badges,omitempty"`
}

// OptionTrailing is one right-aligned metric of an option row.
type OptionTrailing struct {
	// Key identifies the metric in the served display (unique per display).
	Key string `json:"key"`
	// Label is a short caption (i18n key or literal), e.g. "Disp." / "Precio".
	Label string `json:"label,omitempty"`
	// Field names a column of the row. Exactly one of Field / Metric.
	Field string `json:"field,omitempty"`
	// Metric names a computed value contributed by an addon's option_metrics[]
	// for this model. Exactly one of Field / Metric. Unprovided = item omitted.
	Metric string `json:"metric,omitempty"`
	// Format: money | number | integer | percent | date | relative_date | text.
	// money uses CurrencyField when set, else the organization currency.
	Format string `json:"format,omitempty"`
	// CurrencyField names a column holding the ISO currency of a money value.
	CurrencyField string `json:"currency_field,omitempty"`
	// Tones are evaluated in order against the item's value; the FIRST match
	// sets the tone (and optionally replaces the text, dims or blocks the row).
	Tones []OptionTone `json:"tones,omitempty"`
	// When gates the item per row: shown only when the condition holds
	// (`field` names the column tested; without it the item's own value is
	// tested). E.g. no stock column for a service product. Nil = always.
	When *OptionDisplayCondition `json:"when,omitempty"`
}

// OptionTone is one conditional style rule of a trailing metric.
type OptionTone struct {
	When OptionDisplayCondition `json:"when"`
	// Tone: success | warning | danger | info | neutral.
	Tone string `json:"tone"`
	// Text replaces the formatted value (e.g. "Agotado"). i18n key or literal.
	Text string `json:"text,omitempty"`
	// Dim attenuates the whole option row (still selectable).
	Dim bool `json:"dim,omitempty"`
	// Block makes the option not selectable in the picker (UI hint).
	Block bool `json:"block,omitempty"`
}

// OptionDisplayCondition compares a value (the trailing item's value, or Field for a
// badge) against a literal Value, a column (Ref) or a metric (RefMetric).
type OptionDisplayCondition struct {
	// Field is the column tested — badges only (a tone tests its own value).
	Field string `json:"field,omitempty"`
	// Op: lt | lte | gt | gte | eq | neq | empty | not_empty.
	Op        string `json:"op"`
	Value     any    `json:"value,omitempty"`
	Ref       string `json:"ref,omitempty"`
	RefMetric string `json:"ref_metric,omitempty"`
}

// OptionBadge is a chip next to the option's title.
type OptionBadge struct {
	// Field names the column the badge reads (shown as text when Text is empty
	// and Values has no entry for it).
	Field string `json:"field,omitempty"`
	// When gates the badge; nil = shown whenever Field is non-empty (or always
	// when there is no Field and Text is set).
	When *OptionDisplayCondition `json:"when,omitempty"`
	Text string                  `json:"text,omitempty"`
	Tone string                  `json:"tone,omitempty"`
	// Values maps a Field value to its text/tone (a status chip).
	Values map[string]OptionBadgeValue `json:"values,omitempty"`
}

// OptionBadgeValue is the text and tone of one value of a badge's Field.
type OptionBadgeValue struct {
	Text string `json:"text,omitempty"`
	Tone string `json:"tone,omitempty"`
}

// OptionMetric CONTRIBUTES a computed value to the options of a model (own or
// another addon's): an aggregate over one of THIS addon's models related to the
// target by a foreign key — inventory publishes "stock_available" for
// products.Product as SUM(stock.available) GROUP BY product_id. The options
// endpoint resolves every metric a display references with ONE grouped query
// per page (never per option), scoped to the caller's organization, and the
// host only serves it while this addon is installed and enabled for the org.
type OptionMetric struct {
	// Key the target's option_display references in trailing[].metric.
	Key string `json:"key"`
	// Target is the model whose options receive the value, "<addon>.<Model>"
	// (e.g. "products.Product").
	Target string `json:"target"`
	// Model is the Key of one of THIS manifest's models[] aggregated.
	Model string `json:"model"`
	// ForeignKey is the column of Model holding the target row's id.
	ForeignKey string `json:"foreign_key"`
	// Aggregate: sum | count | min | max | avg.
	Aggregate string `json:"aggregate"`
	// Column aggregated (required unless aggregate is count).
	Column string `json:"column,omitempty"`
	// Where narrows the aggregated rows with equality predicates; a null value
	// means IS NULL. Column names must exist on Model.
	Where map[string]any `json:"where,omitempty"`
	// Scope narrows by the picker's CONTEXT (the document's warehouse, the
	// session's branch). A scope whose context key is absent from the request
	// does not apply (the aggregate spans the whole organization).
	Scope []OptionMetricScope `json:"scope,omitempty"`
}

// OptionMetricScope binds one context key of the options request to a column
// of the aggregated model, directly or THROUGH another model of this addon
// (stock.warehouse_id IN (SELECT id FROM warehouses WHERE branch_id = ctx)).
type OptionMetricScope struct {
	// Context key sent by the picker (?ctx.<key>=) or injected by the host.
	Context string `json:"context"`
	// Column of the aggregated model compared.
	Column string `json:"column"`
	// Through, when set, matches Column against the ids of Through.Model rows
	// whose Through.Column equals the context value.
	Through *OptionMetricThrough `json:"through,omitempty"`
}

// OptionMetricThrough is the indirection of a scope (see OptionMetricScope).
type OptionMetricThrough struct {
	Model  string `json:"model"`
	Column string `json:"column"`
}

// Closed vocabularies of the option display primitive.
var (
	OptionDisplayFormats = map[string]struct{}{
		"money": {}, "number": {}, "integer": {}, "percent": {}, "date": {}, "relative_date": {}, "text": {},
	}
	OptionDisplayTones = map[string]struct{}{
		"success": {}, "warning": {}, "danger": {}, "info": {}, "neutral": {},
	}
	OptionConditionOps = map[string]struct{}{
		"lt": {}, "lte": {}, "gt": {}, "gte": {}, "eq": {}, "neq": {}, "empty": {}, "not_empty": {},
	}
	OptionMetricAggregates = map[string]struct{}{
		"sum": {}, "count": {}, "min": {}, "max": {}, "avg": {},
	}
)

var (
	optionIdentRe       = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
	optionPlaceholderRe = regexp.MustCompile(`\{([^{}]*)\}`)
	optionTargetRe      = regexp.MustCompile(`^[a-z][a-z0-9_-]*\.[A-Z][A-Za-z0-9]+$`)
)

// OptionTemplateColumns returns the columns a title/subtitle part references:
// the part itself when it is a bare identifier, else its {placeholders}.
func OptionTemplateColumns(part string) []string {
	p := strings.TrimSpace(part)
	if optionIdentRe.MatchString(p) {
		return []string{p}
	}
	var out []string
	for _, m := range optionPlaceholderRe.FindAllStringSubmatch(p, -1) {
		out = append(out, strings.TrimSpace(m[1]))
	}
	return out
}

// validateOptionDisplays checks every model's option_display against the
// model's own columns and the closed vocabularies.
func validateOptionDisplays(m *Manifest, colsByModel map[string]map[string]struct{}) []string {
	var errs []string
	for _, mod := range m.Models {
		if mod.OptionDisplay == nil {
			continue
		}
		cols := colsByModel[mod.Key]
		w := fmt.Sprintf("models[%s].option_display", mod.Key)
		errs = append(errs, validateOptionDisplay(w, mod.OptionDisplay, cols)...)
	}
	return errs
}

func validateOptionDisplay(w string, d *OptionDisplay, cols map[string]struct{}) []string {
	var errs []string
	hasCol := func(c string) bool { _, ok := cols[c]; return ok }
	checkTemplate := func(field, part string) {
		if strings.TrimSpace(part) == "" {
			errs = append(errs, fmt.Sprintf("%s.%s is empty", w, field))
			return
		}
		refs := OptionTemplateColumns(part)
		if len(refs) == 0 {
			errs = append(errs, fmt.Sprintf("%s.%s %q names no column (use a column name or {column} placeholders)", w, field, part))
		}
		for _, c := range refs {
			if !hasCol(c) {
				errs = append(errs, fmt.Sprintf("%s.%s references %q, which is not a column of the model", w, field, c))
			}
		}
	}
	if d.Title != "" {
		checkTemplate("title", d.Title)
	}
	for i, p := range d.Subtitle {
		checkTemplate(fmt.Sprintf("subtitle[%d]", i), p)
	}
	if d.Image != "" && !hasCol(d.Image) {
		errs = append(errs, fmt.Sprintf("%s.image %q is not a column of the model", w, d.Image))
	}
	checkCond := func(cw string, c OptionDisplayCondition, needField bool) {
		if _, ok := OptionConditionOps[c.Op]; !ok {
			errs = append(errs, fmt.Sprintf("%s.op %q is not one of lt|lte|gt|gte|eq|neq|empty|not_empty", cw, c.Op))
		}
		if needField && c.Field != "" && !hasCol(c.Field) {
			errs = append(errs, fmt.Sprintf("%s.field %q is not a column of the model", cw, c.Field))
		}
		if !needField && c.Field != "" {
			errs = append(errs, fmt.Sprintf("%s.field is only valid on a badge or trailing `when` (a tone tests its own value)", cw))
		}
		if c.Ref != "" && !hasCol(c.Ref) {
			errs = append(errs, fmt.Sprintf("%s.ref %q is not a column of the model", cw, c.Ref))
		}
		if c.RefMetric != "" && !optionIdentRe.MatchString(c.RefMetric) {
			errs = append(errs, fmt.Sprintf("%s.ref_metric %q must match ^[a-z][a-z0-9_]*$", cw, c.RefMetric))
		}
		set := 0
		if c.Value != nil {
			set++
		}
		if c.Ref != "" {
			set++
		}
		if c.RefMetric != "" {
			set++
		}
		switch c.Op {
		case "empty", "not_empty":
			if set > 0 {
				errs = append(errs, fmt.Sprintf("%s: op %q takes no value/ref/ref_metric", cw, c.Op))
			}
		default:
			if set != 1 {
				errs = append(errs, fmt.Sprintf("%s: exactly one of value / ref / ref_metric is required for op %q", cw, c.Op))
			}
		}
	}
	checkTone := func(tw, tone string, required bool) {
		if tone == "" {
			if required {
				errs = append(errs, tw+" is empty")
			}
			return
		}
		if _, ok := OptionDisplayTones[tone]; !ok {
			errs = append(errs, fmt.Sprintf("%s %q is not one of success|warning|danger|info|neutral", tw, tone))
		}
	}
	seen := map[string]struct{}{}
	for i, t := range d.Trailing {
		tw := fmt.Sprintf("%s.trailing[%d]", w, i)
		if !optionIdentRe.MatchString(t.Key) {
			errs = append(errs, fmt.Sprintf("%s.key %q must match ^[a-z][a-z0-9_]*$", tw, t.Key))
		} else if _, dup := seen[t.Key]; dup {
			errs = append(errs, fmt.Sprintf("%s.key %q is duplicated", tw, t.Key))
		}
		seen[t.Key] = struct{}{}
		switch {
		case t.Field == "" && t.Metric == "":
			errs = append(errs, tw+": one of field / metric is required")
		case t.Field != "" && t.Metric != "":
			errs = append(errs, tw+": field and metric are mutually exclusive")
		case t.Field != "" && !hasCol(t.Field):
			errs = append(errs, fmt.Sprintf("%s.field %q is not a column of the model", tw, t.Field))
		case t.Metric != "" && !optionIdentRe.MatchString(t.Metric):
			errs = append(errs, fmt.Sprintf("%s.metric %q must match ^[a-z][a-z0-9_]*$", tw, t.Metric))
		}
		if t.Format != "" {
			if _, ok := OptionDisplayFormats[t.Format]; !ok {
				errs = append(errs, fmt.Sprintf("%s.format %q is not one of money|number|integer|percent|date|relative_date|text", tw, t.Format))
			}
		}
		if t.CurrencyField != "" {
			if t.Format != "money" {
				errs = append(errs, tw+".currency_field is only valid with format money")
			}
			if !hasCol(t.CurrencyField) {
				errs = append(errs, fmt.Sprintf("%s.currency_field %q is not a column of the model", tw, t.CurrencyField))
			}
		}
		if t.When != nil {
			checkCond(tw+".when", *t.When, true)
		}
		for j, tn := range t.Tones {
			nw := fmt.Sprintf("%s.tones[%d]", tw, j)
			checkCond(nw+".when", tn.When, false)
			checkTone(nw+".tone", tn.Tone, true)
		}
	}
	for i, b := range d.Badges {
		bw := fmt.Sprintf("%s.badges[%d]", w, i)
		if b.Field != "" && !hasCol(b.Field) {
			errs = append(errs, fmt.Sprintf("%s.field %q is not a column of the model", bw, b.Field))
		}
		if b.Field == "" && b.Text == "" {
			errs = append(errs, bw+": one of field / text is required")
		}
		if b.When != nil {
			checkCond(bw+".when", *b.When, true)
		}
		checkTone(bw+".tone", b.Tone, false)
		keys := make([]string, 0, len(b.Values))
		for k := range b.Values {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		if len(keys) > 0 && b.Field == "" {
			errs = append(errs, bw+".values requires field")
		}
		for _, k := range keys {
			checkTone(fmt.Sprintf("%s.values[%q].tone", bw, k), b.Values[k].Tone, false)
		}
	}
	return errs
}

// validateOptionMetrics checks the contributed option metrics: unique keys,
// an addon-qualified target (an own target must exist), and every column the
// aggregate / where / scope names exists on this addon's models.
func validateOptionMetrics(m *Manifest, colsByModel map[string]map[string]struct{}) []string {
	if len(m.OptionMetrics) == 0 {
		return nil
	}
	var errs []string
	seen := map[string]struct{}{}
	for i, om := range m.OptionMetrics {
		w := fmt.Sprintf("option_metrics[%d]", i)
		if !optionIdentRe.MatchString(om.Key) {
			errs = append(errs, fmt.Sprintf("%s.key %q must match ^[a-z][a-z0-9_]*$", w, om.Key))
		}
		if !optionTargetRe.MatchString(om.Target) {
			errs = append(errs, fmt.Sprintf("%s.target %q must be \"<addon>.<Model>\"", w, om.Target))
		} else {
			addon, model, _ := strings.Cut(om.Target, ".")
			if addon == m.Metadata.Key {
				if _, ok := colsByModel[model]; !ok {
					errs = append(errs, fmt.Sprintf("%s.target %q is not a model of this addon", w, om.Target))
				}
			}
			dk := om.Target + "#" + om.Key
			if _, dup := seen[dk]; dup {
				errs = append(errs, fmt.Sprintf("%s.key %q is duplicated for target %q", w, om.Key, om.Target))
			}
			seen[dk] = struct{}{}
		}
		if _, ok := OptionMetricAggregates[om.Aggregate]; !ok {
			errs = append(errs, fmt.Sprintf("%s.aggregate %q is not one of sum|count|min|max|avg", w, om.Aggregate))
		}
		cols, ok := colsByModel[om.Model]
		if !ok {
			errs = append(errs, fmt.Sprintf("%s.model %q is not a model of this addon", w, om.Model))
			continue
		}
		requireCol := func(field, name string, c map[string]struct{}, model string) {
			if name == "" {
				errs = append(errs, fmt.Sprintf("%s.%s is empty", w, field))
				return
			}
			if _, ok := c[name]; !ok {
				errs = append(errs, fmt.Sprintf("%s.%s %q is not a column of model %q", w, field, name, model))
			}
		}
		requireCol("foreign_key", om.ForeignKey, cols, om.Model)
		if om.Aggregate != "count" || om.Column != "" {
			requireCol("column", om.Column, cols, om.Model)
		}
		whereCols := make([]string, 0, len(om.Where))
		for c := range om.Where {
			whereCols = append(whereCols, c)
		}
		sort.Strings(whereCols)
		for _, c := range whereCols {
			if _, ok := cols[c]; !ok {
				errs = append(errs, fmt.Sprintf("%s.where names %q, which is not a column of model %q", w, c, om.Model))
			}
		}
		for j, sc := range om.Scope {
			sw := fmt.Sprintf("%s.scope[%d]", w, j)
			if !optionIdentRe.MatchString(sc.Context) {
				errs = append(errs, fmt.Sprintf("%s.context %q must match ^[a-z][a-z0-9_]*$", sw, sc.Context))
			}
			requireCol(fmt.Sprintf("scope[%d].column", j), sc.Column, cols, om.Model)
			if sc.Through != nil {
				tcols, ok := colsByModel[sc.Through.Model]
				if !ok {
					errs = append(errs, fmt.Sprintf("%s.through.model %q is not a model of this addon", sw, sc.Through.Model))
					continue
				}
				requireCol(fmt.Sprintf("scope[%d].through.column", j), sc.Through.Column, tcols, sc.Through.Model)
			}
		}
	}
	return errs
}
