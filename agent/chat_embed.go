package agent

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"github.com/asteby/metacore-kernel/genui"
	"github.com/asteby/metacore-kernel/vector"
)

// ChatEmbed is a clickable record/view card or a live chart shown under Aby's reply.
type ChatEmbed struct {
	Type          string     `json:"type"` // record | view | count | chart | image
	Model         string     `json:"model"`
	ModelTitle    string     `json:"model_title,omitempty"`
	ID            string     `json:"id,omitempty"`
	Title         string     `json:"title,omitempty"`
	Subtitle      string     `json:"subtitle,omitempty"`
	Search        string     `json:"search,omitempty"`
	FilterSummary string     `json:"filter_summary,omitempty"`
	Chart         *ChartSpec `json:"chart,omitempty"`
	// ImageDataURL carries a generate_image result inline ("data:image/png;base64,…") — no
	// upload/storage step, same "return the payload, frontend renders it" shape as Chart.
	ImageDataURL string `json:"image_data_url,omitempty"`
	// Count is the number of matching rows when Type is "count": a query
	// that returned no rows to show (usually 0), rendered as a card with the
	// model's label instead of "Count: 0 / Model: products".
	Count int `json:"count,omitempty"`
	// UI is a validated generative-UI spec (Type "ui"): cards, KPIs, tables,
	// links, timelines, charts or a confirmation form drawn from the SDK's
	// whitelist. Any tool whose result carries a "ui" key produces one.
	UI *genui.Spec `json:"ui,omitempty"`
}

const maxRecordEmbeds = 2

// ModelRegistry is how the runtime talks about the host's data models without
// importing them: resolving what the LLM called a model to a canonical key and
// producing localized, user-facing labels. Implemented by the host.
type ModelRegistry interface {
	// ResolveModelKey maps an LLM-provided name/alias to the canonical key
	// ("" when unknown).
	ResolveModelKey(name string) string
	// ModelLabel is the localized plural label of a model ("datos" fallback is
	// the implementation's choice).
	ModelLabel(modelKey string) string
	// FieldLabel is the localized label of a column; raw is returned when none.
	FieldLabel(model, key, raw string) string
	// EnumValueLabel localizes an enum/state value; value is returned when none.
	EnumValueLabel(model, field, value string) string
}

// QueryResult is the shape of a successful data-query tool call (a record
// page of one model); EmbedBuilder renders it as record/count cards.
type QueryResult struct {
	Model   string           `json:"model"`
	Count   int              `json:"count"`
	Records []map[string]any `json:"records"`
	// Text, when set, is what the model reads instead of the JSON of the
	// records (a compact Markdown table, see RenderRowsTable).
	Text string `json:"-"`
}

// LLMText implements tools.LLMTexter.
func (q *QueryResult) LLMText() string {
	if q == nil {
		return ""
	}
	return q.Text
}

// EmbedBuilder converts tool results into chat embeds (record cards, counts,
// charts, images) using the host's ModelRegistry for labels.
type EmbedBuilder struct {
	models ModelRegistry
}

// NewEmbedBuilder returns a builder bound to the host's registry.
func NewEmbedBuilder(models ModelRegistry) *EmbedBuilder { return &EmbedBuilder{models: models} }

// FromToolResult builds chat embeds from successful data tools.
func (b *EmbedBuilder) FromToolResult(toolName string, result any, params map[string]any) []ChatEmbed {
	if result == nil {
		return nil
	}
	switch toolName {
	case "query_data":
		return b.embedsFromQueryResult(result, params)
	case "vector_search":
		return b.embedsFromVectorResult(result)
	case "show_chart":
		return embedsFromChartResult(result)
	case "generate_image":
		return embedsFromImageResult(result)
	default:
		return embedsFromUIResult(result)
	}
}

// embedsFromUIResult turns a tool result with a "ui" key into a "ui" embed.
// Invalid specs are dropped (never rendered).
func embedsFromUIResult(result any) []ChatEmbed {
	m, ok := asMap(result)
	if !ok {
		return nil
	}
	raw, ok := m["ui"]
	if !ok || raw == nil {
		return nil
	}
	spec, err := genui.FromAny(raw)
	if err != nil {
		return nil
	}
	return []ChatEmbed{{Type: "ui", Title: spec.Title, UI: spec}}
}

func embedsFromImageResult(result any) []ChatEmbed {
	m, ok := asMap(result)
	if !ok {
		return nil
	}
	b64, _ := m["image_base64"].(string)
	if b64 == "" {
		return nil
	}
	contentType, _ := m["content_type"].(string)
	if contentType == "" {
		contentType = "image/png"
	}
	prompt, _ := m["prompt"].(string)
	return []ChatEmbed{{
		Type:         "image",
		Title:        ClipRunes(prompt, 80),
		ImageDataURL: "data:" + contentType + ";base64," + b64,
	}}
}

func (b *EmbedBuilder) embedsFromQueryResult(result any, params map[string]any) []ChatEmbed {
	qr, ok := result.(*QueryResult)
	if !ok {
		// JSON round-trip / map form
		m, ok := asMap(result)
		if !ok {
			return nil
		}
		model, _ := m["model"].(string)
		recs, _ := m["records"].([]any)
		rows := make([]map[string]any, 0, len(recs))
		for _, r := range recs {
			if row, ok := asMap(r); ok {
				rows = append(rows, row)
			}
		}
		total := len(rows)
		switch t := m["count"].(type) {
		case float64:
			if int(t) > total {
				total = int(t)
			}
		case int:
			if t > total {
				total = t
			}
		}
		if len(rows) == 0 {
			return b.countEmbed(model, params, total)
		}
		return b.buildRecordEmbeds(model, rows, params, total)
	}
	total := qr.Count
	if total < len(qr.Records) {
		total = len(qr.Records)
	}
	if len(qr.Records) == 0 {
		return b.countEmbed(qr.Model, params, total)
	}
	return b.buildRecordEmbeds(qr.Model, qr.Records, params, total)
}

// countEmbed answers a query with no rows to show: one "count" card with the
// model's localized label, opening the (filtered) table.
func (b *EmbedBuilder) countEmbed(model string, params map[string]any, total int) []ChatEmbed {
	model = b.models.ResolveModelKey(model)
	if model == "" {
		return nil
	}
	title := b.models.ModelLabel(model)
	searchQS, summary := b.viewSearchFromParams(params, model)
	sub := title
	if summary != "" {
		sub = title + " · " + summary
	}
	return []ChatEmbed{{
		Type:          "count",
		Model:         model,
		ModelTitle:    title,
		Title:         title,
		Subtitle:      sub,
		Search:        searchQS,
		FilterSummary: summary,
		Count:         total,
	}}
}

func embedsFromChartResult(result any) []ChatEmbed {
	spec := chartSpecFromResult(result)
	if spec == nil || len(spec.Rows) == 0 {
		return nil
	}
	sub := spec.Kind
	if spec.Unit != "" {
		sub = spec.Kind + " · " + spec.Unit
	}
	return []ChatEmbed{{
		Type:     "chart",
		Title:    spec.Title,
		Subtitle: sub,
		Chart:    spec,
	}}
}

func chartSpecFromResult(result any) *ChartSpec {
	switch v := result.(type) {
	case *ChartSpec:
		return v
	case ChartSpec:
		cp := v
		return &cp
	}
	m, ok := asMap(result)
	if !ok {
		return nil
	}
	if raw, ok := m["chart"]; ok {
		if spec := chartSpecFromResult(raw); spec != nil {
			return spec
		}
	}
	spec, err := parseChartSpec(m)
	if err != nil {
		return nil
	}
	return spec
}

func (b *EmbedBuilder) embedsFromVectorResult(result any) []ChatEmbed {
	m, ok := asMap(result)
	if !ok {
		return nil
	}
	model, _ := m["model"].(string)
	rows := vectorRows(m["results"])
	return b.buildRecordEmbeds(model, rows, nil, len(rows))
}

func vectorRows(raw any) []map[string]any {
	switch list := raw.(type) {
	case []vector.SearchResult:
		rows := make([]map[string]any, 0, len(list))
		for _, v := range list {
			row := v.Data
			if row == nil {
				row = map[string]any{}
			}
			if v.ID != "" {
				row["id"] = v.ID
			}
			rows = append(rows, row)
		}
		return rows
	case []any:
		rows := make([]map[string]any, 0, len(list))
		for _, item := range list {
			switch v := item.(type) {
			case vector.SearchResult:
				row := v.Data
				if row == nil {
					row = map[string]any{}
				}
				if v.ID != "" {
					row["id"] = v.ID
				}
				rows = append(rows, row)
			case map[string]any:
				data, _ := v["data"].(map[string]any)
				if data == nil {
					data = map[string]any{}
				}
				if id, ok := v["id"]; ok && id != nil {
					data["id"] = id
				}
				rows = append(rows, data)
			}
		}
		return rows
	default:
		return nil
	}
}

func (b *EmbedBuilder) buildRecordEmbeds(model string, records []map[string]any, params map[string]any, totalHint int) []ChatEmbed {
	model = b.models.ResolveModelKey(model)
	if model == "" || len(records) == 0 {
		return nil
	}
	title := b.models.ModelLabel(model)
	count := totalHint
	if count < len(records) {
		count = len(records)
	}

	searchQS, summary := b.viewSearchFromParams(params, model)

	// Large / filtered lists: one smart view card with count — no spam of record tiles.
	if len(records) > maxRecordEmbeds || count > maxRecordEmbeds {
		viewTitle := fmt.Sprintf("%s (%d)", title, count)
		return []ChatEmbed{{
			Type:          "view",
			Model:         model,
			ModelTitle:    title,
			Title:         viewTitle,
			Search:        searchQS,
			FilterSummary: summary,
			Subtitle:      summary,
		}}
	}

	out := make([]ChatEmbed, 0, len(records)+1)
	seen := map[string]struct{}{}

	for _, row := range records {
		if len(out) >= maxRecordEmbeds {
			break
		}
		id := fmt.Sprint(row["id"])
		if id == "" || id == "<nil>" {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		recTitle := embedRecordTitle(row, title)
		sub := b.embedRecordSubtitle(row, model, title)
		if sub == recTitle || sub == title {
			sub = title
		}
		out = append(out, ChatEmbed{
			Type:       "record",
			Model:      model,
			ModelTitle: title,
			ID:         id,
			Title:      recTitle,
			Subtitle:   sub,
		})
	}
	if len(out) == 0 {
		return nil
	}

	// Two rows: also offer filtered table when filters/search exist.
	if len(records) > 1 && searchQS != "" {
		viewTitle := fmt.Sprintf("%s (%d)", title, count)
		out = append(out, ChatEmbed{
			Type:          "view",
			Model:         model,
			ModelTitle:    title,
			Title:         viewTitle,
			Search:        searchQS,
			FilterSummary: summary,
		})
	}
	return out
}

func (b *EmbedBuilder) viewSearchFromParams(params map[string]any, model string) (searchQS, summary string) {
	if params == nil {
		return "", ""
	}
	q := url.Values{}
	var bits []string
	if s := strings.TrimSpace(fmt.Sprint(params["search"])); s != "" && s != "<nil>" {
		q.Set("search", s)
		bits = append(bits, "“"+s+"”")
	}
	if filters, ok := params["filters"].(map[string]any); ok {
		for k, v := range filters {
			if k == "" || v == nil {
				continue
			}
			val := strings.TrimSpace(fmt.Sprint(v))
			if val == "" || val == "<nil>" {
				continue
			}
			q.Set("f_"+k, "eq:"+val)
			if len(bits) < 4 {
				bits = append(bits, b.models.FieldLabel(model, k, k)+": "+b.models.EnumValueLabel(model, k, val))
			}
		}
	}
	if len(q) == 0 {
		return "", ""
	}
	return "?" + q.Encode(), strings.Join(bits, " · ")
}

func embedRecordTitle(row map[string]any, fallback string) string {
	keys := []string{
		"name", "title", "number", "code", "folio", "sku", "email", "label",
		"reference", "product_name", "warehouse_name", "variant_name",
	}
	for _, k := range keys {
		if s := scalarEmbedLabel(row[k]); s != "" {
			return s
		}
	}
	for _, k := range []string{"product_id", "product", "warehouse_id", "warehouse", "customer_id", "customer", "variant_id", "variant"} {
		if s := scalarEmbedLabel(row[k]); s != "" {
			return s
		}
	}
	if id := fmt.Sprint(row["id"]); id != "" && id != "<nil>" {
		if len(id) > 8 {
			return id[:8]
		}
		return id
	}
	return fallback
}

func (b *EmbedBuilder) embedRecordSubtitle(row map[string]any, model, modelTitle string) string {
	var bits []string
	product := scalarEmbedLabel(row["product_id"])
	if product == "" {
		product = scalarEmbedLabel(row["product"])
	}
	if product == "" {
		product = scalarEmbedLabel(row["product_name"])
	}
	warehouse := scalarEmbedLabel(row["warehouse_id"])
	if warehouse == "" {
		warehouse = scalarEmbedLabel(row["warehouse"])
	}
	if warehouse == "" {
		warehouse = scalarEmbedLabel(row["warehouse_name"])
	}
	variant := scalarEmbedLabel(row["variant_id"])
	if variant == "" {
		variant = scalarEmbedLabel(row["variant"])
	}
	if variant == "" {
		variant = scalarEmbedLabel(row["variant_name"])
	}
	qty := scalarEmbedLabel(row["quantity"])
	if qty == "" {
		qty = scalarEmbedLabel(row["available"])
	}
	if qty == "" {
		qty = scalarEmbedLabel(row["qty"])
	}
	if variant != "" && variant != product {
		bits = append(bits, variant)
	}
	if warehouse != "" {
		bits = append(bits, warehouse)
	}
	if qty != "" {
		bits = append(bits, "Cant. "+qty)
	}
	for _, k := range []string{"status", "state", "stage", "total", "amount"} {
		if s := scalarEmbedLabel(row[k]); s != "" {
			if k == "status" || k == "state" || k == "stage" {
				s = b.models.EnumValueLabel(model, k, s)
			}
			bits = append(bits, s)
		}
	}
	if len(bits) == 0 {
		return modelTitle
	}
	return modelTitle + " · " + strings.Join(bits, " · ")
}

func scalarEmbedLabel(v any) string {
	if v == nil {
		return ""
	}
	switch t := v.(type) {
	case string:
		return strings.TrimSpace(t)
	case float64, float32, int, int64, int32:
		s := strings.TrimSpace(fmt.Sprint(t))
		if s == "<nil>" {
			return ""
		}
		return s
	case map[string]any:
		for _, k := range []string{"label", "name", "title", "value", "display_name"} {
			if s := scalarEmbedLabel(t[k]); s != "" {
				return s
			}
		}
	}
	s := strings.TrimSpace(fmt.Sprint(v))
	if s == "" || s == "<nil>" || s == "map[]" {
		return ""
	}
	// Skip bare UUIDs as primary titles when longer nested labels exist elsewhere
	return s
}

func asMap(v any) (map[string]any, bool) {
	switch m := v.(type) {
	case map[string]any:
		return m, true
	default:
		return nil, false
	}
}

func DedupeEmbeds(embeds []ChatEmbed) []ChatEmbed {
	if len(embeds) == 0 {
		return embeds
	}
	seen := map[string]struct{}{}
	out := make([]ChatEmbed, 0, len(embeds))
	for _, e := range embeds {
		key := e.Type + "|" + e.Model + "|" + e.ID + "|" + e.Search
		if e.Type == "chart" {
			key = "chart|" + e.Title + "|" + ""
			if e.Chart != nil {
				key = fmt.Sprintf("chart|%s|%s|%d", e.Chart.Kind, e.Title, len(e.Chart.Rows))
			}
		}
		if e.Type == "ui" && e.UI != nil {
			b, _ := json.Marshal(e.UI)
			key = "ui|" + string(b)
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, e)
	}
	return out
}
