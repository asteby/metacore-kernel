package genuiv2

import (
	"fmt"
	"sort"
	"strings"
)

// Profile is a validation profile: the set of rules a SURFACE adds on top of
// the contract itself. The contract (schema + Parse) says what a genui/v2
// document may be; a profile says what THIS caller is willing to accept from an
// untrusted author, typically an LLM. It only ever narrows: a document that
// passes a profile also passes the bare contract, and the profile never
// rewrites the document (it rejects, with a stable code and a JSON path the
// author can act on).
//
// A nil Profile (Options.Profile) is the bare contract, as before. A zero
// numeric limit means "no profile limit" (the contract's own limits apply).
// Profiles are plain data plus an optional source hook: hosts build one with
// AgentProfile() and adjust it, no fork needed.
type Profile struct {
	// Name labels the profile in errors and audit records ("agent").
	Name string `json:"name"`

	// AllowedBlocks is the whitelist of block types (the schema's block names,
	// plus "ref" where the surface expands named blocks before validating:
	// "ref" is never in the schema, it only reports that the surface accepts
	// reference nodes). Anything else, ext/slot/settings/form... , is rejected.
	AllowedBlocks []string `json:"allowed_blocks"`

	// AllowedVerbs is the whitelist of action verbs (navigate, open, close,
	// set, refresh, emit...). A verb outside it is rejected wherever it sits.
	AllowedVerbs []string `json:"allowed_verbs"`
	// AllowedEmits is the whitelist of `emit` action ids. Empty = no emit at
	// all, even when "emit" is in AllowedVerbs: the host injects the ids its
	// client bridge understands.
	AllowedEmits []string `json:"allowed_emits,omitempty"`

	// AllowedSourceKinds is the whitelist of data source kinds: "q", "widget",
	// "inline", "plan". A kind outside it is rejected.
	AllowedSourceKinds []string `json:"allowed_source_kinds"`
	// AllowedQueryPrefixes narrows `q` sources to the read tokens whose prefix
	// is listed ("t" table, "r" record...). Empty = every prefix the contract has.
	AllowedQueryPrefixes []string `json:"allowed_query_prefixes,omitempty"`
	// SourceAllow is the host's static whitelist hook: it receives the source
	// name and its kind ("q" | "widget" | "plan") with its value (the q token,
	// the widget key or the plan) and rejects with an error whose text is shown
	// to the author. It is NOT the per-user permission check (that needs an
	// identity: see the agent package's SourceResolver); it is "this surface
	// only reads these models / these widgets". Nil = no hook.
	SourceAllow func(name, kind, value string) error `json:"-"`

	// SourceOnlyBlocks lists block types that may not carry literal data: their
	// rows/items/points and scalar value/spark must come from a declared source
	// (a `value` may be a binding object, never a typed number). It is how a
	// surface guarantees "no figures typed by the author" for charts, tables,
	// maps and metrics.
	SourceOnlyBlocks []string `json:"source_only_blocks,omitempty"`

	// Limits (0 = none). They are stricter than the contract's.
	MaxBytes      int `json:"max_bytes,omitempty"`
	MaxBlocks     int `json:"max_blocks,omitempty"`
	MaxDepth      int `json:"max_depth,omitempty"`
	MaxSources    int `json:"max_sources,omitempty"`
	MaxActions    int `json:"max_actions,omitempty"`
	MaxInlineRows int `json:"max_inline_rows,omitempty"`
	// MaxCharts caps the chart blocks (the heaviest renderer) per document.
	MaxCharts int `json:"max_charts,omitempty"`
}

// Stable codes of the profile layer. They are NOT in AllCodes(): the shared
// conformance vectors pin the contract, not a surface's policy.
const (
	CodeProfileBlock     = "genui.profile.block_not_allowed"
	CodeProfileVerb      = "genui.profile.action_not_allowed"
	CodeProfileEmit      = "genui.profile.emit_not_allowed"
	CodeProfileSource    = "genui.profile.source_not_allowed"
	CodeProfileLiteral   = "genui.profile.literal_data"
	CodeProfileComponent = "genui.profile.component_not_allowed"
	CodeProfileBytes     = "genui.profile.limit.bytes"
	CodeProfileBlocks    = "genui.profile.limit.blocks"
	CodeProfileDepth     = "genui.profile.limit.depth"
	CodeProfileSources   = "genui.profile.limit.sources"
	CodeProfileActions   = "genui.profile.limit.actions"
	CodeProfileInline    = "genui.profile.limit.inline_rows"
	CodeProfileCharts    = "genui.profile.limit.charts"
)

// ProfileCodes lists every stable profile code (docs and coverage tests).
func ProfileCodes() []string {
	return []string{
		CodeProfileBlock, CodeProfileVerb, CodeProfileEmit, CodeProfileSource, CodeProfileLiteral,
		CodeProfileComponent, CodeProfileBytes, CodeProfileBlocks, CodeProfileDepth, CodeProfileSources,
		CodeProfileActions, CodeProfileInline, CodeProfileCharts,
	}
}

// AgentProfile is the profile of an LLM author in a chat: read-only
// presentation. Visual blocks only (no ext, slot, settings, form, wizard,
// confirm, media), no component references, navigation limited to the app and
// state, no `invoke`/`submit` (writes go through human-approved proposals),
// `emit` only for ids the host injects, data only from sources (q on tables and
// records, dashboard widgets, a small inline), and limits well below the
// contract's. Pair it with AllowExternalLinks=false: Options() does.
//
// It returns a fresh copy each call; callers may modify it.
func AgentProfile() *Profile {
	return &Profile{
		Name: "agent",
		AllowedBlocks: []string{
			"stack", "grid", "tabs", "section",
			"text", "markdown", "alert", "empty", "card", "list", "links", "timeline", "diagram",
			"kpi_card", "stats", "metric", "table", "chart", "map", "view", "model_view",
			"ref",
		},
		AllowedVerbs:         []string{"navigate", "open", "close", "set", "refresh", "emit"},
		AllowedSourceKinds:   []string{"q", "widget", "inline"},
		AllowedQueryPrefixes: []string{"t", "r"},
		SourceOnlyBlocks:     []string{"chart", "table", "stats", "map", "metric", "kpi_card"},
		MaxBytes:             64 * 1024,
		MaxBlocks:            24,
		MaxDepth:             4,
		MaxSources:           4,
		MaxActions:           12,
		MaxInlineRows:        20,
		MaxCharts:            3,
	}
}

// Options is the validation options that apply this profile: external links
// off and the profile attached.
func (p *Profile) Options() Options { return Options{AllowExternalLinks: false, Profile: p} }

// AllowsBlock reports whether the profile accepts a block type.
func (p *Profile) AllowsBlock(typ string) bool { return in(p.AllowedBlocks, typ) }

// Clone returns an independent copy (slices included).
func (p *Profile) Clone() *Profile {
	c := *p
	c.AllowedBlocks = append([]string(nil), p.AllowedBlocks...)
	c.AllowedVerbs = append([]string(nil), p.AllowedVerbs...)
	c.AllowedEmits = append([]string(nil), p.AllowedEmits...)
	c.AllowedSourceKinds = append([]string(nil), p.AllowedSourceKinds...)
	c.AllowedQueryPrefixes = append([]string(nil), p.AllowedQueryPrefixes...)
	c.SourceOnlyBlocks = append([]string(nil), p.SourceOnlyBlocks...)
	return &c
}

func in(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

// profileDataKeys are the keys whose content is data, not structure: the
// profile does not look for blocks or actions inside them.
var profileDataKeys = map[string]bool{"rows": true, "inline": true, "data": true, "state": true, "props": true, "params": true, "values": true, "filters": true, "args": true}

// literalKeys are the block fields that carry typed data.
var literalKeys = []string{"rows", "items", "points", "spark"}

// check runs the profile over an already schema-valid document. It panics with
// *Error like the rest of the validator (recovered by validatePrepared).
func (p *Profile) check(d obj) {
	if p.MaxBytes > 0 {
		if raw, err := marshal(d); err == nil && len(raw) > p.MaxBytes {
			panic(fail(CodeProfileBytes, "$", "perfil %s: el documento pesa %d bytes y el maximo es %d", p.Name, len(raw), p.MaxBytes))
		}
	}
	p.checkSources(asObj(d["data"]))

	st := &profileState{p: p}
	st.blocks(d["blocks"], "$.blocks", 1)
	if ov := asObj(d["overlays"]); len(ov) > 0 {
		names := make([]string, 0, len(ov))
		for k := range ov {
			names = append(names, k)
		}
		sort.Strings(names)
		for _, k := range names {
			st.blocks(asObj(ov[k])["blocks"], "$.overlays."+k+".blocks", 1)
		}
	}
	st.walkActions(d, "$")
}

func (p *Profile) checkSources(data obj) {
	if p.MaxSources > 0 && len(data) > p.MaxSources {
		panic(fail(CodeProfileSources, "$.data", "perfil %s: %d fuentes y el maximo es %d", p.Name, len(data), p.MaxSources))
	}
	names := make([]string, 0, len(data))
	for k := range data {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, name := range names {
		src := asObj(data[name])
		path := "$.data." + name
		for _, kind := range []string{"q", "widget", "inline", "plan"} {
			if !has(src, kind) {
				continue
			}
			if !in(p.AllowedSourceKinds, kind) {
				panic(fail(CodeProfileSource, path+"."+kind, "perfil %s: las fuentes %q no estan permitidas", p.Name, kind))
			}
			value := ""
			switch kind {
			case "q", "widget", "plan":
				value = str(src[kind])
			case "inline":
				if n := lenOf(src["inline"]); p.MaxInlineRows > 0 && n > p.MaxInlineRows {
					panic(fail(CodeProfileInline, path+".inline", "perfil %s: %d filas en linea y el maximo es %d", p.Name, n, p.MaxInlineRows))
				}
			}
			if kind == "q" && len(p.AllowedQueryPrefixes) > 0 {
				prefix := value
				if i := strings.IndexByte(value, ':'); i >= 0 {
					prefix = value[:i]
				}
				if !in(p.AllowedQueryPrefixes, prefix) {
					panic(fail(CodeProfileSource, path+".q", "perfil %s: la lectura %q no esta permitida (solo %s)", p.Name, prefix+":", strings.Join(p.AllowedQueryPrefixes, ", ")))
				}
			}
			if p.SourceAllow != nil && kind != "inline" {
				if err := p.SourceAllow(name, kind, value); err != nil {
					panic(fail(CodeProfileSource, path+"."+kind, "perfil %s: fuente %s no permitida: %s", p.Name, name, err.Error()))
				}
			}
		}
	}
}

type profileState struct {
	p       *Profile
	blockN  int
	chartN  int
	actionN int
}

func (s *profileState) blocks(v any, path string, depth int) {
	list, _ := v.([]any)
	for i, raw := range list {
		b, ok := raw.(obj)
		if !ok {
			continue
		}
		where := fmt.Sprintf("%s[%d]", path, i)
		typ := str(b["type"])
		p := s.p
		if !in(p.AllowedBlocks, typ) {
			panic(fail(CodeProfileBlock, where+".type", "perfil %s: el bloque %q no esta permitido (permitidos: %s)", p.Name, typ, strings.Join(p.AllowedBlocks, ", ")))
		}
		if s.blockN++; p.MaxBlocks > 0 && s.blockN > p.MaxBlocks {
			panic(fail(CodeProfileBlocks, where, "perfil %s: maximo %d bloques", p.Name, p.MaxBlocks))
		}
		if p.MaxDepth > 0 && depth > p.MaxDepth {
			panic(fail(CodeProfileDepth, where, "perfil %s: profundidad maxima %d", p.Name, p.MaxDepth))
		}
		if typ == "chart" {
			if s.chartN++; p.MaxCharts > 0 && s.chartN > p.MaxCharts {
				panic(fail(CodeProfileCharts, where, "perfil %s: maximo %d graficas", p.Name, p.MaxCharts))
			}
		}
		if in(p.SourceOnlyBlocks, typ) {
			for _, k := range literalKeys {
				if has(b, k) {
					panic(fail(CodeProfileLiteral, where+"."+k, "perfil %s: %s no admite %s tecleado; declara una fuente en data y usa source", p.Name, typ, k))
				}
			}
			if v, ok := b["value"]; ok {
				if o, isO := v.(obj); !isO || !has(o, "$ref") {
					panic(fail(CodeProfileLiteral, where+".value", "perfil %s: %s no admite una cifra tecleada en value; usa source/field o un binding a data", p.Name, typ))
				}
			}
		}
		if has(b, "component") {
			panic(fail(CodeProfileComponent, where+".component", "perfil %s: los componentes personalizados no estan permitidos", p.Name))
		}
		s.blocks(b["blocks"], where+".blocks", depth+1)
		if tabs, ok := b["tabs"].([]any); ok {
			for j, t := range tabs {
				to := asObj(t)
				tp := fmt.Sprintf("%s.tabs[%d]", where, j)
				if has(to, "component") {
					panic(fail(CodeProfileComponent, tp+".component", "perfil %s: los componentes personalizados no estan permitidos", p.Name))
				}
				s.blocks(to["blocks"], tp+".blocks", depth+1)
			}
		}
	}
}

// walkActions finds every action object ({do: ...}) outside data keys.
func (s *profileState) walkActions(v any, path string) {
	switch t := v.(type) {
	case []any:
		for i, x := range t {
			s.walkActions(x, fmt.Sprintf("%s[%d]", path, i))
		}
	case obj:
		if verb, ok := t["do"].(string); ok {
			p := s.p
			if s.actionN++; p.MaxActions > 0 && s.actionN > p.MaxActions {
				panic(fail(CodeProfileActions, path, "perfil %s: maximo %d acciones", p.Name, p.MaxActions))
			}
			if !in(p.AllowedVerbs, verb) {
				panic(fail(CodeProfileVerb, path+".do", "perfil %s: la accion %q no esta permitida (permitidas: %s)", p.Name, verb, strings.Join(p.AllowedVerbs, ", ")))
			}
			if verb == "emit" && !in(p.AllowedEmits, str(t["id"])) {
				panic(fail(CodeProfileEmit, path+".id", "perfil %s: emit %q no esta en la lista permitida", p.Name, str(t["id"])))
			}
		}
		keys := make([]string, 0, len(t))
		for k := range t {
			if !profileDataKeys[k] {
				keys = append(keys, k)
			}
		}
		sort.Strings(keys)
		for _, k := range keys {
			s.walkActions(t[k], path+"."+k)
		}
	}
}
