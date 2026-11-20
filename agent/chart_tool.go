package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/asteby/metacore-kernel/agent/contextmgr"
	genuiv2 "github.com/asteby/metacore-kernel/genui/v2"
)

const (
	maxChartRows   = 48
	maxChartSeries = 6
	maxPieSlices   = 8
	maxChartTitle  = 80
	maxChartNote   = 240
	maxChartKeyLen = 40
	maxChartLabel  = 48
)

// ChartSeries is one numeric series on a show_chart embed.
type ChartSeries struct {
	Key   string `json:"key"`
	Label string `json:"label,omitempty"`
}

// ChartSpec is the payload rendered as a live Recharts embed in Aby chat.
type ChartSpec struct {
	Kind   string           `json:"kind"`
	Title  string           `json:"title"`
	XKey   string           `json:"x_key"`
	XLabel string           `json:"x_label,omitempty"`
	Unit   string           `json:"unit,omitempty"`
	Series []ChartSeries    `json:"series"`
	Rows   []map[string]any `json:"rows"`
	Note   string           `json:"note,omitempty"`
}

// ShowChartTool stages an interactive, downloadable chart embed. Aby should
// call it after query/aggregate when a visual is clearer than a table.
//
// Deprecated: show_chart carries figures typed by the model, so the server
// cannot verify they come from an authorized query. Use ShowUITool (show_ui),
// where the chart is a genui/v2 block that reads a declared source (q: or
// widget) with the viewer's permissions, rendered by the same component as the
// pages and the dashboard. show_chart keeps working unchanged; EmitUI makes it
// also emit an equivalent genui/v2 document (inline source) so a client can
// migrate to the single renderer before hosts move to show_ui. Removal is
// tracked in the CHANGELOG.
type ShowChartTool struct {
	emitUI bool
}

// NewShowChartTool returns the legacy chart tool.
//
// Deprecated: use NewShowUITool.
func NewShowChartTool() *ShowChartTool { return &ShowChartTool{} }

// EmitUI makes the result also carry the chart as a genui/v2 document
// (ChartSpec.UIDoc, inline source). The embed builder then emits a "ui" embed
// of Version genui/v2 instead of the legacy "chart" embed whenever the kind
// has a v2 equivalent; any other kind keeps the legacy embed. Migration aid only.
func (t *ShowChartTool) EmitUI() *ShowChartTool { t.emitUI = true; return t }

// SatisfiesOutcomes implements tools.OutcomeSatisfier: both tools draw a chart,
// so whichever ran backs "aquí está el gráfico" (the claim guard checks each
// registered tool separately).
func (t *ShowChartTool) SatisfiesOutcomes() []string { return []string{"show_ui"} }

func (t *ShowChartTool) Name() string     { return "show_chart" }
func (t *ShowChartTool) Category() string { return "data" }
func (t *ShowChartTool) Description() string {
	return "Render an interactive chart embed in chat (downloadable PNG/CSV). Use after you have real numbers from query_data/aggregate_data. Pick the kind that fits: line/area for time, bar/stacked_bar/horizontal_bar for categories, pie/donut only for a few parts of a whole, scatter for two numerics, radar for 3+ axes, composed for bar+line. Do not use pie for everything. Call this when the user asks for a graph OR when a visual helps (comparisons, trends, mix). Never invent figures."
}

// chartClaims are the assertions that a chart exists NOW, by language
// ("aquí está el gráfico", "here is the chart", "segue o gráfico"). Offers
// ("puedo mostrarte…") and refusals ("no pude generar…") must not match. No \b
// after a group that can end in an accent: Go's \b is ASCII. A host adds
// another language with RegisterOutcomeClaims("show_chart", "fr", ...).
var chartClaims = map[string][]*regexp.Regexp{
	"es": {regexp.MustCompile(`(?i)\b(?:aqu[ií]\s+(?:est[aá]n?|tienes|va)|te\s+(?:muestro|dejo|comparto|presento)|ya\s+(?:est[aá]|tienes)|he\s+(?:generado|creado|preparado|armado)|acabo\s+de\s+(?:generar|crear|preparar|armar|generado|creado|preparado|armado)|gener[eé]|cre[eé]|prepar[eé])[^.\n]{0,40}\b(?:gr[aá]fic[oa]s?|chart)\b`)},
	"en": {regexp.MustCompile(`(?i)\b(?:here(?:'s|\s+is|\s+are)|i(?:'ve|\s+have)\s+(?:created|generated|built|prepared)|i\s+(?:created|generated|built|prepared)|below\s+is)\s+(?:the|a|your|an)\s+(?:\w+\s+){0,2}(?:chart|graph)s?\b`)},
	"pt": {regexp.MustCompile(`(?i)\b(?:aqui\s+(?:est[aá]n?|vai|v[aã]o)|segue(?:m)?|(?:te|lhe)\s+(?:mostro|deixo|apresento)|j[aá]\s+(?:est[aá]|gerei|criei|preparei)|gerei|criei|preparei|montei|acabei\s+de\s+(?:gerar|criar|preparar|montar))[^.\n]{0,40}\b(?:gr[aá]ficos?)\b`)},
}

// OutcomeClaims makes show_chart a tools.OutcomeClaimer: the orchestrator
// checks the reply against these patterns and the turn's successful calls.
func (t *ShowChartTool) OutcomeClaims() map[string][]*regexp.Regexp { return chartClaims }

// PromptHint is the rule that travels with the tool (tools.PromptHinter): the
// numbers come from a data tool, and a chart is only "shown" if this tool ran.
func (t *ShowChartTool) PromptHint() string {
	return "úsala cuando el usuario pida una gráfica o cuando 3+ cifras comparables se entiendan mejor así; " +
		"primero obtén los números con una herramienta de datos y pásalos exactos en rows (nunca inventes valores). " +
		"Si dices que muestras un gráfico, debes llamar show_chart en ese mismo turno; si no puedes, dilo y entrega una tabla Markdown. " +
		"No pegues el JSON del gráfico."
}

func (t *ShowChartTool) Parameters() map[string]any {
	return map[string]any{
		"title": map[string]any{
			"type": "string", "description": "Short chart title", "required": true,
		},
		"kind": map[string]any{
			"type":        "string",
			"description": "bar | stacked_bar | horizontal_bar | line | area | pie | donut | scatter | radar | composed",
			"required":    true,
		},
		"x_key": map[string]any{
			"type": "string", "description": "Row field used as category / X (e.g. month, warehouse)", "required": true,
		},
		"x_label": map[string]any{
			"type": "string", "description": "Axis label for X",
		},
		"unit": map[string]any{
			"type": "string", "description": "Unit for Y (the organization's currency code, pieces, %)",
		},
		"series": map[string]any{
			"type": "array", "description": "Numeric series: [{key, label}]", "required": true,
		},
		"rows": map[string]any{
			"type": "array", "description": "Data rows: objects with x_key and each series key as numbers", "required": true,
		},
		"note": map[string]any{
			"type": "string", "description": "Optional one-line insight under the chart",
		},
	}
}

func (t *ShowChartTool) Execute(ctx context.Context, params map[string]any, agentCtx *contextmgr.ConversationContext) (any, error) {
	spec, err := parseChartSpec(params)
	if err != nil {
		return nil, err
	}
	out := map[string]any{
		"ok":     true,
		"kind":   spec.Kind,
		"title":  spec.Title,
		"points": len(spec.Rows),
		"chart":  spec,
	}
	if t.emitUI {
		if doc, err := spec.UIDoc(); err == nil {
			out["ui"] = doc
			out["fallback"] = ClipRunes(spec.Title+" · "+spec.Note, maxUIFallback)
		}
	}
	return out, nil
}

// chartV2Kind maps a legacy chart kind to the kind of the genui/v2 chart block
// ("" = no equivalent: stacked/horizontal bars, scatter, radar and composed
// stay on the legacy renderer instead of being drawn as something else).
var chartV2Kind = map[string]string{"bar": "bar", "line": "line", "area": "area", "pie": "pie", "donut": "donut"}

// UIDoc translates the spec into a genui/v2 document with ONE chart block over
// an inline source (the rows the model typed). It exists for the migration
// from show_chart to show_ui; it is not the path for new charts, because an
// inline source cannot be authorized against the user's permissions. It
// returns an error for a kind with no v2 equivalent, and the document is
// validated against the bare contract before it is returned.
func (s *ChartSpec) UIDoc() (genuiv2.Doc, error) {
	kind := chartV2Kind[s.Kind]
	if kind == "" {
		return nil, fmt.Errorf("chart kind %q has no genui/v2 equivalent", s.Kind)
	}
	series := make([]any, 0, len(s.Series))
	for _, se := range s.Series {
		series = append(series, map[string]any{"key": se.Key, "label": se.Label})
	}
	rows := make([]any, 0, len(s.Rows))
	for _, r := range s.Rows {
		row := make(map[string]any, len(r))
		for k, v := range r {
			row[k] = v
		}
		rows = append(rows, row)
	}
	summary := s.Title
	if s.Note != "" {
		summary = s.Title + ". " + s.Note
	}
	block := map[string]any{
		"type": "chart", "kind": kind, "title": s.Title, "x": s.XKey,
		"series": series, "source": "data.chart", "summary": summary,
	}
	if s.Note != "" {
		block["caption"] = s.Note
	}
	doc := map[string]any{
		"version": genuiv2.Version,
		"title":   s.Title,
		"data":    map[string]any{"chart": map[string]any{"inline": rows}},
		"blocks":  []any{block},
	}
	return genuiv2.Parse(doc, genuiv2.Options{})
}

func parseChartSpec(params map[string]any) (*ChartSpec, error) {
	title := ClipRunes(strParam(params, "title"), maxChartTitle)
	if title == "" {
		title = "Gráfico"
	}
	xKey := sanitizeChartKey(strParam(params, "x_key"))
	if xKey == "" {
		xKey = "label"
	}
	spec := &ChartSpec{
		Kind:   normalizeChartKind(strParam(params, "kind")),
		Title:  title,
		XKey:   xKey,
		XLabel: ClipRunes(strParam(params, "x_label"), maxChartLabel),
		Unit:   ClipRunes(strParam(params, "unit"), 16),
		Note:   ClipRunes(strParam(params, "note"), maxChartNote),
	}

	spec.Series = parseChartSeries(params["series"], spec.XKey)
	spec.Rows = parseChartRows(params["rows"], spec.XKey, spec.Series)
	if len(spec.Rows) == 0 {
		return nil, fmt.Errorf("rows: se necesitan datos numéricos reales (query_data / aggregate_data primero)")
	}
	if len(spec.Series) == 0 {
		spec.Series = inferChartSeries(spec.Rows, spec.XKey)
	}
	if len(spec.Series) == 0 {
		return nil, fmt.Errorf("series: indica al menos una serie numérica (key + label)")
	}
	if len(spec.Series) > maxChartSeries {
		spec.Series = spec.Series[:maxChartSeries]
	}

	// Pie of 12 slices is noise — force a bar so Aby stays useful.
	if (spec.Kind == "pie" || spec.Kind == "donut") && len(spec.Rows) > maxPieSlices {
		spec.Kind = "bar"
	}
	if spec.Kind == "scatter" && len(spec.Series) < 1 {
		return nil, fmt.Errorf("scatter necesita una serie numérica (Y); X puede ser numérico en x_key")
	}
	return spec, nil
}

func normalizeChartKind(raw string) string {
	k := strings.ToLower(strings.TrimSpace(raw))
	k = strings.ReplaceAll(k, "-", "_")
	switch k {
	case "bar", "column", "columns":
		return "bar"
	case "stacked_bar", "stacked", "stack":
		return "stacked_bar"
	case "horizontal_bar", "bar_h", "hbar", "barh":
		return "horizontal_bar"
	case "line", "lines", "timeseries", "time":
		return "line"
	case "area":
		return "area"
	case "pie", "torta", "pastel":
		return "pie"
	case "donut", "doughnut", "anillo":
		return "donut"
	case "scatter", "xy", "puntos":
		return "scatter"
	case "radar", "spider", "polar":
		return "radar"
	case "composed", "combo", "bar_line":
		return "composed"
	default:
		return "bar"
	}
}

func parseChartSeries(raw any, xKey string) []ChartSeries {
	list, ok := raw.([]any)
	if !ok {
		if typed, ok := raw.([]ChartSeries); ok {
			out := make([]ChartSeries, 0, len(typed))
			for _, s := range typed {
				key := sanitizeChartKey(s.Key)
				if key == "" || key == xKey {
					continue
				}
				label := strings.TrimSpace(s.Label)
				if label == "" {
					label = key
				}
				out = append(out, ChartSeries{Key: key, Label: ClipRunes(label, maxChartLabel)})
			}
			return out
		}
		return nil
	}
	out := make([]ChartSeries, 0, len(list))
	seen := map[string]struct{}{}
	for _, item := range list {
		m, ok := asMap(item)
		if !ok {
			continue
		}
		key := sanitizeChartKey(strParam(m, "key"))
		if key == "" {
			key = sanitizeChartKey(strParam(m, "name"))
		}
		if key == "" || key == xKey {
			continue
		}
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		label := strings.TrimSpace(strParam(m, "label"))
		if label == "" {
			label = key
		}
		out = append(out, ChartSeries{Key: key, Label: ClipRunes(label, maxChartLabel)})
		if len(out) >= maxChartSeries {
			break
		}
	}
	return out
}

func parseChartRows(raw any, xKey string, series []ChartSeries) []map[string]any {
	list, ok := raw.([]any)
	if !ok {
		if typed, ok := raw.([]map[string]any); ok {
			out := make([]map[string]any, 0, len(typed))
			for _, row := range typed {
				if cleaned := cleanChartRow(row, xKey, series); cleaned != nil {
					out = append(out, cleaned)
				}
			}
			return capChartRows(out)
		}
		return nil
	}
	out := make([]map[string]any, 0, len(list))
	for _, item := range list {
		m, ok := asMap(item)
		if !ok {
			continue
		}
		if cleaned := cleanChartRow(m, xKey, series); cleaned != nil {
			out = append(out, cleaned)
		}
	}
	return capChartRows(out)
}

func capChartRows(rows []map[string]any) []map[string]any {
	if len(rows) > maxChartRows {
		return rows[:maxChartRows]
	}
	return rows
}

func cleanChartRow(row map[string]any, xKey string, series []ChartSeries) map[string]any {
	if row == nil {
		return nil
	}
	label := scalarEmbedLabel(row[xKey])
	if label == "" {
		label = scalarEmbedLabel(row["label"])
	}
	if label == "" {
		label = scalarEmbedLabel(row["name"])
	}
	if label == "" {
		return nil
	}
	out := map[string]any{xKey: ClipRunes(label, maxChartLabel)}
	if len(series) == 0 {
		for k, v := range row {
			key := sanitizeChartKey(k)
			if key == "" || key == xKey {
				continue
			}
			if n, ok := asChartNumber(v); ok {
				out[key] = n
			}
		}
		if len(out) < 2 {
			return nil
		}
		return out
	}
	hasNum := false
	for _, s := range series {
		n, ok := asChartNumber(row[s.Key])
		if !ok {
			n = 0
		} else {
			hasNum = true
		}
		out[s.Key] = n
	}
	if !hasNum {
		return nil
	}
	return out
}

func inferChartSeries(rows []map[string]any, xKey string) []ChartSeries {
	if len(rows) == 0 {
		return nil
	}
	var out []ChartSeries
	seen := map[string]struct{}{xKey: {}}
	for k, v := range rows[0] {
		key := sanitizeChartKey(k)
		if key == "" {
			continue
		}
		if _, skip := seen[key]; skip {
			continue
		}
		if _, ok := asChartNumber(v); !ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, ChartSeries{Key: key, Label: key})
		if len(out) >= maxChartSeries {
			break
		}
	}
	return out
}

func asChartNumber(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case int32:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	case string:
		s := strings.TrimSpace(strings.ReplaceAll(n, ",", ""))
		if s == "" {
			return 0, false
		}
		f, err := strconv.ParseFloat(s, 64)
		return f, err == nil
	default:
		return 0, false
	}
}

func sanitizeChartKey(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-' {
			b.WriteRune(r)
		}
	}
	out := b.String()
	if len(out) > maxChartKeyLen {
		out = out[:maxChartKeyLen]
	}
	return out
}

func ClipRunes(s string, n int) string {
	s = strings.TrimSpace(s)
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
