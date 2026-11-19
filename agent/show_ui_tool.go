package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/asteby/metacore-kernel/agent/contextmgr"
	genuiv2 "github.com/asteby/metacore-kernel/genui/v2"
)

const (
	maxUIFallback = 500
	// maxUIRetries is how many corrected attempts the HOST gives the model after
	// a rejection (the tool is stateless; the orchestrator counts). It is part
	// of the rejection the model reads.
	maxUIRetries = 2
)

// ShowUITool is the one way the agent shows something richer than text: a
// genui/v2 document. The model does not type figures: it declares SOURCES
// (data.<n> = {q} | {widget} | small {inline}) and composes the SAME blocks the
// pages and the dashboard use (chart, metric, table, map...), or references
// named blocks ({type:"ref"}). The kernel validates the document against the
// agent profile and REJECTS (never fixes) anything outside it, with a
// structured error the model can correct; the host authorizes every source with
// the identity of the conversation, expands references, audits, and only then
// does the user see it.
//
// Pipeline: expand refs (host catalog) -> Parse with the agent profile ->
// inline declaration check -> authorize sources (host) -> audit (host) ->
// result with the "ui" key (a "ui" embed of version genui/v2).
type ShowUITool struct {
	profile *genuiv2.Profile
	host    UIHost
}

// ShowUIOption tunes NewShowUITool.
type ShowUIOption func(*ShowUITool)

// WithUIProfile replaces the validation profile (default genuiv2.AgentProfile()).
// The profile is copied; external links stay off whatever the profile says.
func WithUIProfile(p *genuiv2.Profile) ShowUIOption {
	return func(t *ShowUITool) {
		if p != nil {
			t.profile = p.Clone()
		}
	}
}

// NewShowUITool returns the tool bound to the host's ports.
func NewShowUITool(host UIHost, opts ...ShowUIOption) *ShowUITool {
	t := &ShowUITool{profile: genuiv2.AgentProfile(), host: host}
	for _, o := range opts {
		o(t)
	}
	return t
}

func (t *ShowUITool) Name() string     { return "show_ui" }
func (t *ShowUITool) Category() string { return "data" }

func (t *ShowUITool) Description() string {
	return "Show the user a visual answer: a genui/v2 document made of the SAME blocks the app's pages and dashboard use (chart, metric, stats, table, map, timeline, markdown, section/grid/tabs, kpi_card). " +
		"You do NOT type figures: declare where data comes from in doc.data (q: \"t:<model>?...\" reads a table, \"r:<model>/<id>\" a record, {widget:\"<addon>.dashboard.<key>\"} an existing widget) and point blocks at it with source:\"data.<name>\". " +
		"Before composing from scratch call list_blocks and list_widgets and reuse what exists: {type:\"ref\",ref:\"<addon>.<key>\"} or data.<name>={widget:...}. " +
		"inline data is only for a few rows you declare yourself (list their names in inline_sources). " +
		"Allowed blocks: " + strings.Join(t.profile.AllowedBlocks, ", ") + ". No ext/slot/settings/forms, no external links, no invoke/submit actions. " +
		"The document is validated and REJECTED, not fixed: if the answer is an error with code/path, correct exactly that and call again (at most 2 retries)."
}

// PromptHint implements tools.PromptHinter.
func (t *ShowUITool) PromptHint() string {
	return "para gráficas, tablas, métricas, mapas o líneas de tiempo usa show_ui con fuentes (data.<n>: q/widget), nunca con cifras tecleadas; " +
		"antes mira list_blocks y list_widgets y reutiliza lo que ya existe con {type:\"ref\"}; " +
		"si show_ui devuelve un error con code y path, corrige solo eso y reintenta (máx. 2); " +
		"si dices que muestras un gráfico o una vista, debes llamar show_ui en ese mismo turno; añade fallback con una frase de conclusión. " +
		"No pegues el JSON del documento."
}

// OutcomeClaims implements tools.OutcomeClaimer: "aquí está el gráfico" is a
// claim that show_ui (or show_chart, see SatisfiesOutcomes) ran.
func (t *ShowUITool) OutcomeClaims() map[string][]*regexp.Regexp { return chartClaims }

// SatisfiesOutcomes implements tools.OutcomeSatisfier: a successful show_ui
// backs the claims of the deprecated show_chart.
func (t *ShowUITool) SatisfiesOutcomes() []string { return []string{"show_chart"} }

func (t *ShowUITool) Parameters() map[string]any {
	return map[string]any{
		"doc": map[string]any{
			"type":        "object",
			"description": "genui/v2 document: {version:\"genui/v2\", title?, data?:{<name>:{q|widget|inline}}, blocks:[...]}. Blocks read data with source:\"data.<name>\". Schema: see the genui/v2 contract.",
			"required":    true,
		},
		"fallback": map[string]any{
			"type":        "string",
			"description": "One or two sentences with the conclusion, for clients that cannot draw the document (text, voice). Required in practice: no figures you did not read from a tool.",
		},
		"inline_sources": map[string]any{
			"type":        "array",
			"description": "Names of the data entries you wrote as inline (small data you declare yourself). An inline source not listed here is rejected.",
		},
	}
}

// ShowUIResult is the successful result of show_ui. The "ui" key is what the
// embed builder turns into a "ui" embed (version genui/v2).
type ShowUIResult struct {
	OK      bool        `json:"ok"`
	Version string      `json:"version"`
	UI      genuiv2.Doc `json:"ui"`
	DocHash string      `json:"doc_hash"`
	// Fallback is the text for clients without the document renderer.
	Fallback string `json:"fallback,omitempty"`
	// Provenance says where the data comes from, so a renderer can mark what
	// the model declared itself.
	Provenance UIProvenance `json:"provenance"`
	Blocks     int          `json:"blocks"`
}

// UIProvenance separates authorized sources from data the model typed.
type UIProvenance struct {
	Sources []UISource `json:"sources,omitempty"`
	// ModelInline lists the data entries whose rows were declared by the model.
	ModelInline []string `json:"model_inline,omitempty"`
}

// LLMText implements tools.LLMTexter: the model does not need its own document
// echoed back, only that it was shown.
func (r *ShowUIResult) LLMText() string {
	if r == nil {
		return ""
	}
	return fmt.Sprintf("Interfaz mostrada al usuario (%d bloques, %s). No repitas su contenido: añade, a lo sumo, una frase de conclusión.", r.Blocks, r.DocHash[:12])
}

// UIRejection is the structured error show_ui returns when a document is not
// accepted. The model reads Error() (JSON); a host reads it with errors.As to
// count attempts (MaxRetries) and to degrade to text when they run out.
type UIRejection struct {
	Code       string `json:"code"`
	Path       string `json:"path,omitempty"`
	Message    string `json:"message"`
	Hint       string `json:"hint,omitempty"`
	Retryable  bool   `json:"retryable"`
	MaxRetries int    `json:"max_retries"`
}

func (e *UIRejection) Error() string {
	b, _ := json.Marshal(struct {
		Error *UIRejection `json:"show_ui_rejected"`
	}{e})
	return string(b)
}

// Codes of the rejections the tool itself produces (the contract and profile
// layers keep their own genui.* codes).
const (
	CodeUINoDocument      = "ui.no_document"
	CodeUIRefsUnavailable = "ui.refs_unavailable"
	CodeUIRefUnresolved   = "ui.ref_unresolved"
	CodeUIInlineUndecl    = "ui.inline_undeclared"
	CodeUISourcesNoHost   = "ui.sources_unavailable"
	CodeUISourceDenied    = "ui.source_denied"
	CodeUIAuditFailed     = "ui.audit_failed"
)

func reject(code, path, msg, hint string, retryable bool) *UIRejection {
	r := &UIRejection{Code: code, Path: path, Message: msg, Hint: hint, Retryable: retryable}
	if retryable {
		r.MaxRetries = maxUIRetries
	}
	return r
}

func uiHint(code string) string {
	switch {
	case strings.HasPrefix(code, "genui.profile.literal_data"):
		return "no tecleas cifras: declara la fuente en doc.data (q o widget) y usa source:\"data.<nombre>\" en el bloque"
	case strings.HasPrefix(code, "genui.profile.block_not_allowed"):
		return "usa solo los bloques permitidos de la descripción de show_ui"
	case strings.HasPrefix(code, "genui.profile.action_not_allowed"), strings.HasPrefix(code, "genui.profile.emit_not_allowed"):
		return "solo navegación interna, estado (open/close/set/refresh) y emit de ids permitidos; las escrituras van por propuesta"
	case strings.HasPrefix(code, "genui.profile.source_not_allowed"):
		return "lee solo con q:\"t:<modelo>...\" o q:\"r:<modelo>/<id>\", {widget} de list_widgets o inline pequeño"
	case strings.HasPrefix(code, "genui.profile.limit"):
		return "reduce el documento: menos bloques, fuentes, gráficas, acciones o filas inline"
	case code == "genui.external_link_disabled", code == "genui.unsafe_link":
		return "solo rutas internas que empiezan por /"
	case strings.HasPrefix(code, "genui."):
		return "corrige el campo indicado en path según el contrato genui/v2"
	}
	return ""
}

func asRejection(err error) *UIRejection {
	var ge *genuiv2.Error
	if errors.As(err, &ge) {
		return reject(ge.Code, ge.Path, ge.Msg, uiHint(ge.Code), true)
	}
	return reject(genuiv2.CodeInvalidDocument, "", err.Error(), uiHint(genuiv2.CodeInvalidDocument), true)
}

// Execute runs the pipeline. A rejection is returned as an error (*UIRejection)
// so the turn does not count it as a shown result.
func (t *ShowUITool) Execute(ctx context.Context, params map[string]any, agentCtx *contextmgr.ConversationContext) (any, error) {
	id := UIIdentityFrom(agentCtx)

	rej := func(r *UIRejection) (any, error) {
		t.auditRejection(ctx, id, r)
		return nil, r
	}

	doc, err := docParam(params["doc"])
	if err != nil {
		return rej(reject(CodeUINoDocument, "doc", err.Error(), "pasa doc como un objeto genui/v2", true))
	}

	// 1. References: only if the profile accepts them and the host has a catalog.
	if refs := countRefs(doc); refs > 0 {
		if !t.profile.AllowsBlock("ref") {
			return rej(reject(genuiv2.CodeProfileBlock, "$.blocks", "los bloques ref no estan permitidos", uiHint(genuiv2.CodeProfileBlock), true))
		}
		if t.host.Catalog == nil {
			return rej(reject(CodeUIRefsUnavailable, "$.blocks", "este host no expone catalogo de bloques", "no uses ref: compon el documento con fuentes", true))
		}
		exp, err := t.host.Catalog.ExpandRefs(ctx, id, doc)
		if err != nil {
			return rej(reject(CodeUIRefUnresolved, "$.blocks", err.Error(), "usa solo refs que devuelve list_blocks (<addon>.<key>)", true))
		}
		doc = exp
	}

	// 2. Contract + agent profile (rejects, never fixes).
	parsed, err := genuiv2.Parse(doc, t.profile.Options())
	if err != nil {
		return rej(asRejection(err))
	}

	// 3. Inline data must be declared by the model.
	declared := declaredNames(params["inline_sources"])
	sources, inline := splitSources(parsed)
	for _, name := range inline {
		if !contains(declared, name) {
			return rej(reject(CodeUIInlineUndecl, "$.data."+name+".inline",
				"la fuente "+name+" lleva datos inline que no declaraste",
				"o la lees de una fuente q/widget, o lista \""+name+"\" en inline_sources si son pocos datos tuyos", true))
		}
	}

	// 4. Identity: every non-inline source, with the conversation's identity.
	if len(sources) > 0 {
		if t.host.Sources == nil {
			return rej(reject(CodeUISourcesNoHost, "$.data", "este host no autoriza fuentes", "responde con texto o con datos inline declarados", false))
		}
		if err := t.host.Sources.AuthorizeSources(ctx, id, sources); err != nil {
			var denied *SourceDeniedError
			if errors.As(err, &denied) {
				return rej(reject(CodeUISourceDenied, "$.data."+firstOr(denied.Sources, ""), denied.Error(),
					"quita esa fuente o usa otra de list_widgets; no insistas con la misma", true))
			}
			return rej(reject(CodeUISourcesNoHost, "$.data", "no se pudieron autorizar las fuentes", "", false))
		}
	}

	// 5. Audit: an accepted document that cannot be recorded is not shown.
	hash := docHash(parsed)
	entry := UIAuditEntry{
		Accepted: true, DocHash: hash, Doc: parsed, Sources: sources, InlineSources: inline,
		Blocks: countBlockTypes(parsed), Profile: t.profile.Name,
	}
	if t.host.Audit != nil {
		if err := t.host.Audit.RecordUI(ctx, id, entry); err != nil {
			return nil, reject(CodeUIAuditFailed, "", "no se pudo registrar la interfaz", "responde con texto", false)
		}
	}

	total := 0
	for _, n := range entry.Blocks {
		total += n
	}
	fb := ClipRunes(strParam(params, "fallback"), maxUIFallback)
	if fb == "" {
		fb = UIFallbackText(parsed)
	}
	return &ShowUIResult{
		OK: true, Version: genuiv2.Version, UI: parsed, DocHash: hash, Fallback: fb, Blocks: total,
		Provenance: UIProvenance{Sources: sources, ModelInline: inline},
	}, nil
}

func (t *ShowUITool) auditRejection(ctx context.Context, id UIIdentity, r *UIRejection) {
	if t.host.Audit != nil {
		_ = t.host.Audit.RecordUI(ctx, id, UIAuditEntry{Accepted: false, Code: r.Code, Path: r.Path, Profile: t.profile.Name})
	}
}

// docParam accepts the document as an object or as the JSON text of one (models
// often stringify nested objects).
func docParam(v any) (map[string]any, error) {
	switch d := v.(type) {
	case map[string]any:
		return d, nil
	case string:
		var m map[string]any
		dec := json.NewDecoder(strings.NewReader(d))
		dec.UseNumber()
		if err := dec.Decode(&m); err != nil || m == nil {
			return nil, errors.New("doc no es un objeto JSON valido")
		}
		return m, nil
	}
	return nil, errors.New("falta doc (objeto genui/v2)")
}

func declaredNames(v any) []string {
	var out []string
	switch l := v.(type) {
	case []any:
		for _, x := range l {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
	case []string:
		out = append(out, l...)
	}
	return out
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

func firstOr(l []string, d string) string {
	if len(l) > 0 {
		return l[0]
	}
	return d
}

// docBlocks calls fn for every block of a document: the root list, the tabs of
// a tabs block, nested blocks and overlays. Data keys are never entered.
func docBlocks(doc map[string]any, fn func(b map[string]any)) {
	var walk func(list any)
	walk = func(list any) {
		items, _ := list.([]any)
		for _, x := range items {
			b, ok := x.(map[string]any)
			if !ok {
				continue
			}
			fn(b)
			walk(b["blocks"])
			if tabs, ok := b["tabs"].([]any); ok {
				for _, t := range tabs {
					if tm, ok := t.(map[string]any); ok {
						walk(tm["blocks"])
					}
				}
			}
		}
	}
	walk(doc["blocks"])
	if ov, ok := doc["overlays"].(map[string]any); ok {
		for _, o := range ov {
			if om, ok := o.(map[string]any); ok {
				walk(om["blocks"])
			}
		}
	}
}

func countRefs(doc map[string]any) int {
	n := 0
	docBlocks(doc, func(b map[string]any) {
		if b["type"] == "ref" {
			n++
		}
	})
	return n
}

func countBlockTypes(doc map[string]any) map[string]int {
	out := map[string]int{}
	docBlocks(doc, func(b map[string]any) { out[fmt.Sprint(b["type"])]++ })
	return out
}

// splitSources lists the sources to authorize (sorted by name) and the names of
// inline ones.
func splitSources(doc map[string]any) (sources []UISource, inline []string) {
	data, _ := doc["data"].(map[string]any)
	names := make([]string, 0, len(data))
	for n := range data {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		src, _ := data[n].(map[string]any)
		for _, kind := range []string{"q", "widget", "plan"} {
			if v, ok := src[kind].(string); ok {
				sources = append(sources, UISource{Name: n, Kind: kind, Value: v})
			}
		}
		if _, ok := src["inline"]; ok {
			inline = append(inline, n)
		}
	}
	return sources, inline
}

// docHash is the sha256 of the canonical JSON (encoding/json sorts map keys).
func docHash(doc map[string]any) string {
	b, _ := json.Marshal(doc)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// UIFallbackText derives a plain-text summary of a document (titles, labels,
// summaries and text blocks in order) for clients that cannot draw it. It
// reads only what the document says; it never invents figures.
func UIFallbackText(doc map[string]any) string {
	var parts []string
	if s, _ := doc["title"].(string); s != "" {
		parts = append(parts, s)
	}
	docBlocks(doc, func(b map[string]any) {
		for _, k := range []string{"title", "label", "summary", "text", "markdown"} {
			if s, ok := b[k].(string); ok && strings.TrimSpace(s) != "" {
				parts = append(parts, strings.TrimSpace(s))
				break
			}
		}
	})
	return ClipRunes(strings.Join(parts, " · "), maxUIFallback)
}

// ---- list_blocks / list_widgets: read-only catalog tools ----

// ListBlocksTool lists the named blocks the user can reference from show_ui.
type ListBlocksTool struct{ catalog UICatalog }

// NewListBlocksTool returns list_blocks bound to the host catalog.
func NewListBlocksTool(c UICatalog) *ListBlocksTool { return &ListBlocksTool{catalog: c} }

func (t *ListBlocksTool) Name() string     { return "list_blocks" }
func (t *ListBlocksTool) Category() string { return "data" }
func (t *ListBlocksTool) Description() string {
	return "List the reusable named blocks the user can see (charts, maps, tables already defined by the app's addons). Call it before show_ui and reference what fits with {type:\"ref\",ref:\"<ref>\"} instead of rebuilding it."
}
func (t *ListBlocksTool) Parameters() map[string]any { return map[string]any{} }
func (t *ListBlocksTool) ParallelSafe() bool         { return true }

func (t *ListBlocksTool) Execute(ctx context.Context, _ map[string]any, c *contextmgr.ConversationContext) (any, error) {
	if t.catalog == nil {
		return map[string]any{"blocks": []BlockInfo{}, "count": 0}, nil
	}
	list, err := t.catalog.ListBlocks(ctx, UIIdentityFrom(c))
	if err != nil {
		return nil, fmt.Errorf("list_blocks: %w", err)
	}
	if list == nil {
		list = []BlockInfo{}
	}
	return map[string]any{"blocks": list, "count": len(list)}, nil
}

// ListWidgetsTool lists the dashboard widgets the user can read from show_ui.
type ListWidgetsTool struct{ catalog UICatalog }

// NewListWidgetsTool returns list_widgets bound to the host catalog.
func NewListWidgetsTool(c UICatalog) *ListWidgetsTool { return &ListWidgetsTool{catalog: c} }

func (t *ListWidgetsTool) Name() string     { return "list_widgets" }
func (t *ListWidgetsTool) Category() string { return "data" }
func (t *ListWidgetsTool) Description() string {
	return "List the dashboard widgets the user can read. Use one as a data source in show_ui: doc.data.<name> = {widget:\"<key>\"} and a block with source:\"data.<name>\"."
}
func (t *ListWidgetsTool) Parameters() map[string]any { return map[string]any{} }
func (t *ListWidgetsTool) ParallelSafe() bool         { return true }

func (t *ListWidgetsTool) Execute(ctx context.Context, _ map[string]any, c *contextmgr.ConversationContext) (any, error) {
	if t.catalog == nil {
		return map[string]any{"widgets": []WidgetInfo{}, "count": 0}, nil
	}
	list, err := t.catalog.ListWidgets(ctx, UIIdentityFrom(c))
	if err != nil {
		return nil, fmt.Errorf("list_widgets: %w", err)
	}
	if list == nil {
		list = []WidgetInfo{}
	}
	return map[string]any{"widgets": list, "count": len(list)}, nil
}
