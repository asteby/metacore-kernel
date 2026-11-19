package agent

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"github.com/asteby/metacore-kernel/genui"
	genuiv2 "github.com/asteby/metacore-kernel/genui/v2"
	"github.com/asteby/metacore-kernel/vector"
)

// ChatEmbed is a clickable record/view card or a live chart shown under Aby's reply.
type ChatEmbed struct {
	Type          string     `json:"type"` // record | view | count | chart | image | ui | text
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
	// Version is the contract of the document a "ui" embed carries: "genui/v2"
	// (document in UIV2) or empty/"genui/v1" (document in UI). A client reads
	// the field that Version names.
	Version string `json:"version,omitempty"`
	// UIV2 is a validated genui/v2 document (Type "ui", Version "genui/v2"): the
	// SAME blocks the pages and the dashboard render, from show_ui.
	UIV2 genuiv2.Doc `json:"ui_v2,omitempty"`
	// Fallback is the plain-text rendition of the embed for clients that cannot
	// draw it (voice, WhatsApp, old apps). DowngradeEmbeds turns an embed the
	// client cannot render into Type "text" carrying only this.
	Fallback string `json:"fallback,omitempty"`
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
	// uiOpts validate a genui/v2 "ui" that arrives from a tool OTHER than
	// show_ui (show_ui's own result was already validated by its profile).
	uiOpts genuiv2.Options
}

// NewEmbedBuilder returns a builder bound to the host's registry.
func NewEmbedBuilder(models ModelRegistry) *EmbedBuilder {
	return &EmbedBuilder{models: models, uiOpts: genuiv2.AgentProfile().Options()}
}

// SetUIOptions replaces how a genui/v2 "ui" from a foreign tool is validated
// (default: the agent profile with external links off). A host that widens the
// show_ui profile passes the same one here.
func (b *EmbedBuilder) SetUIOptions(o genuiv2.Options) *EmbedBuilder {
	b.uiOpts = o
	return b
}

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
		return b.embedsFromChartResult(result)
	case "generate_image":
		return embedsFromImageResult(result)
	default:
		return b.embedsFromUIResult(result)
	}
}

// embedsFromUIResult turns a tool result with a "ui" key into a "ui" embed.
// A genui/v2 document becomes an embed of Version "genui/v2" (UIV2); a genui/v1
// spec keeps its typed v1 path. Invalid documents are dropped (never rendered).
//
// show_ui's own result (*ShowUIResult) was validated by the tool with its
// profile, so only the contract is re-checked here (defence in depth, external
// links off). A "ui" from any other tool goes through the builder's options.
func (b *EmbedBuilder) embedsFromUIResult(result any) []ChatEmbed {
	var raw any
	opts := b.uiOpts
	fallback := ""
	switch r := result.(type) {
	case *ShowUIResult:
		if r == nil || r.UI == nil {
			return nil
		}
		raw, fallback, opts = r.UI, r.Fallback, genuiv2.Options{}
	default:
		m, ok := asMap(result)
		if !ok {
			return nil
		}
		raw = m["ui"]
		if raw == nil {
			return nil
		}
		fallback, _ = m["fallback"].(string)
	}
	if isV2Doc(raw) {
		doc, err := genuiv2.Parse(raw, opts)
		if err != nil {
			return nil
		}
		title, _ := doc["title"].(string)
		if fallback == "" {
			fallback = UIFallbackText(doc)
		}
		return []ChatEmbed{{Type: "ui", Version: genuiv2.Version, Title: title, UIV2: doc, Fallback: ClipRunes(fallback, maxUIFallback)}}
	}
	spec, err := genui.FromAny(raw)
	if err != nil {
		return nil
	}
	return []ChatEmbed{{Type: "ui", Title: spec.Title, UI: spec}}
}

func isV2Doc(raw any) bool {
	switch d := raw.(type) {
	case map[string]any:
		return d["version"] == genuiv2.Version
	}
	return false
}

// DowngradeEmbeds adapts embeds to what the client can render (caps comes from
// the client; nil = a v1-only client). A genui/v2 "ui" embed goes through
// genuiv2.Downgrade: a block the client lacks becomes its fallback, a v1-only
// client gets a valid genui/v1 spec in UI. If the document cannot be degraded
// the embed becomes Type "text" with only its Fallback (or is dropped when it
// has none), so the user always gets the conclusion. Other embeds pass through.
func DowngradeEmbeds(embeds []ChatEmbed, caps *genuiv2.Caps) []ChatEmbed {
	out := make([]ChatEmbed, 0, len(embeds))
	for _, e := range embeds {
		if e.Type != "ui" || e.UIV2 == nil {
			out = append(out, e)
			continue
		}
		res, err := genuiv2.Downgrade(e.UIV2, caps)
		if err == nil && len(res.Spec) > 0 {
			if res.Spec["version"] == genuiv2.VersionV1 {
				if spec, ferr := genui.FromAny(res.Spec); ferr == nil {
					e.UIV2, e.UI, e.Version = nil, spec, genuiv2.VersionV1
					out = append(out, e)
					continue
				}
			} else {
				e.UIV2 = res.Spec
				out = append(out, e)
				continue
			}
		}
		if e.Fallback != "" {
			out = append(out, ChatEmbed{Type: "text", Title: e.Title, Fallback: e.Fallback})
		}
	}
	return out
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

// embedsFromChartResult builds the chart embed of show_chart. When the tool was
// asked to also emit a genui/v2 document (ShowChartTool.EmitUI) and it is
// valid, that document wins: one renderer for everything.
func (b *EmbedBuilder) embedsFromChartResult(result any) []ChatEmbed {
	if m, ok := asMap(result); ok && m["ui"] != nil {
		if ui := b.embedsFromUIResult(result); len(ui) > 0 {
			return ui
		}
	}
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
		if e.Type == "ui" && e.UIV2 != nil {
			b, _ := json.Marshal(e.UIV2)
			key = "ui2|" + string(b)
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
