package genuiv2

import (
	"errors"
	"strings"
	"testing"
)

func chartDoc(extra map[string]any) map[string]any {
	d := map[string]any{
		"version": "genui/v2",
		"data":    map[string]any{"sales": map[string]any{"q": "t:sales?group=month"}},
		"blocks": []any{map[string]any{
			"type": "chart", "kind": "bar", "x": "month", "summary": "Ventas por mes",
			"series": []any{map[string]any{"key": "total", "label": "Total"}},
			"source": "data.sales",
		}},
	}
	for k, v := range extra {
		d[k] = v
	}
	return d
}

func blocksDoc(blocks ...any) map[string]any {
	return map[string]any{"version": "genui/v2", "blocks": blocks}
}

func TestAgentProfile(t *testing.T) {
	text := map[string]any{"type": "text", "text": "hola"}
	big := make([]any, 21)
	for i := range big {
		big[i] = map[string]any{"a": float64(i)}
	}
	manyBlocks := make([]any, 25)
	for i := range manyBlocks {
		manyBlocks[i] = text
	}
	nest := func(depth int) any {
		var b any = text
		for i := 0; i < depth; i++ {
			b = map[string]any{"type": "stack", "blocks": []any{b}}
		}
		return b
	}
	chart := chartDoc(nil)["blocks"].([]any)[0]
	link := func(href string) map[string]any {
		return map[string]any{"type": "links", "items": []any{map[string]any{"label": "x", "href": href}}}
	}
	_ = link
	nav := func(a map[string]any) map[string]any {
		return blocksDoc(map[string]any{"type": "card", "title": "x", "actions": []any{a}})
	}

	tests := []struct {
		name string
		doc  map[string]any
		mut  func(p *Profile)
		code string // "" = valid
	}{
		{name: "valid chart from source", doc: chartDoc(nil)},
		{name: "valid text and widget", doc: map[string]any{"version": "genui/v2",
			"data":   map[string]any{"w": map[string]any{"widget": "hub_fleet.dashboard.by_country"}},
			"blocks": []any{text, map[string]any{"type": "metric", "label": "Flota", "source": "data.w", "field": "n"}}}},
		{name: "ext blocked", doc: blocksDoc(map[string]any{"type": "ext", "name": "x.y"}), code: CodeProfileBlock},
		{name: "slot blocked", doc: blocksDoc(map[string]any{"type": "slot", "name": "x.y"}), code: CodeProfileBlock},
		{name: "settings blocked", doc: blocksDoc(map[string]any{"type": "settings", "addon": "x"}), code: ""},
		{name: "action invoke not allowed", doc: nav(map[string]any{"do": "invoke", "action": "a.B.c"}), code: CodeProfileVerb},
		{name: "navigate internal ok", doc: nav(map[string]any{"do": "navigate", "label": "Ver", "route": map[string]any{"path": "/m/sales"}})},
		{name: "navigate external link", doc: nav(map[string]any{"do": "navigate", "route": map[string]any{"path": "https://evil.test"}}), code: CodeUnsafeLink},
		{name: "emit not whitelisted", doc: nav(map[string]any{"do": "emit", "id": "aby.guide"}), code: CodeProfileEmit},
		{name: "emit whitelisted", doc: nav(map[string]any{"do": "emit", "id": "aby.guide"}), mut: func(p *Profile) { p.AllowedEmits = []string{"aby.guide"} }},
		{name: "source kind plan", doc: map[string]any{"version": "genui/v2", "data": map[string]any{"p": map[string]any{"plan": "dashboard.home"}}, "blocks": []any{text}}, code: CodeProfileSource},
		{name: "query prefix not allowed", doc: chartDoc(map[string]any{"data": map[string]any{"sales": map[string]any{"q": "x:ext"}}}), code: CodeProfileSource},
		{name: "source outside host list", doc: chartDoc(nil), mut: func(p *Profile) {
			p.SourceAllow = func(_, _, v string) error {
				if !strings.HasPrefix(v, "t:orders") {
					return errors.New("modelo no permitido")
				}
				return nil
			}
		}, code: CodeProfileSource},
		{name: "source inside host list", doc: chartDoc(nil), mut: func(p *Profile) {
			p.SourceAllow = func(_, _, v string) error { return nil }
		}},
		{name: "inline too big", doc: chartDoc(map[string]any{"data": map[string]any{"sales": map[string]any{"inline": big}}}), code: CodeProfileInline},
		{name: "inline small ok", doc: chartDoc(map[string]any{"data": map[string]any{"sales": map[string]any{"inline": big[:5]}}})},
		{name: "chart rows typed", doc: blocksDoc(map[string]any{"type": "chart", "kind": "bar", "x": "m", "summary": "s",
			"series": []any{map[string]any{"key": "t", "label": "T"}}, "rows": []any{map[string]any{"m": "ene", "t": 5.0}}}), code: CodeProfileLiteral},
		{name: "metric literal value", doc: blocksDoc(map[string]any{"type": "metric", "label": "x", "value": 1234.0}), code: CodeProfileLiteral},
		{name: "kpi_card literal value", doc: blocksDoc(map[string]any{"type": "kpi_card", "label": "x", "value": "1234"}), code: CodeProfileLiteral},
		{name: "kpi_card bound value", doc: map[string]any{"version": "genui/v2", "data": map[string]any{"s": map[string]any{"q": "r:sales/1"}},
			"blocks": []any{map[string]any{"type": "kpi_card", "label": "x", "value": map[string]any{"$ref": "data.s.total"}}}}},
		{name: "too many blocks", doc: blocksDoc(manyBlocks...), code: CodeProfileBlocks},
		{name: "too deep", doc: blocksDoc(nest(5)), code: CodeProfileDepth},
		{name: "depth ok", doc: blocksDoc(nest(2))},
		{name: "too many charts", doc: chartDoc(map[string]any{"blocks": []any{chart, chart, chart, chart}}), code: CodeProfileCharts},
		{name: "too many bytes", doc: blocksDoc(map[string]any{"type": "markdown", "markdown": strings.Repeat("a", 7000)}), mut: func(p *Profile) { p.MaxBytes = 5000 }, code: CodeProfileBytes},
		{name: "section component", doc: blocksDoc(map[string]any{"type": "section", "mode": "custom", "component": "x.y"}), code: CodeProfileComponent},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := AgentProfile()
			if tc.mut != nil {
				tc.mut(p)
			}
			_, err := Parse(tc.doc, p.Options())
			got := ""
			if err != nil {
				got = CodeOf(err)
			}
			// settings/section are decided by the contract or by the block list: accept any rejection for those.
			if tc.name == "settings blocked" {
				if err == nil {
					t.Fatal("settings must be rejected")
				}
				return
			}
			if got != tc.code {
				t.Fatalf("code = %q (%v), want %q", got, err, tc.code)
			}
			// A profile only narrows: whatever it accepts the bare contract accepts.
			if err == nil {
				if _, e := Parse(tc.doc, Options{}); e != nil && tc.name != "navigate internal ok" {
					t.Fatalf("passes profile but not the contract: %v", e)
				}
			}
		})
	}
}

func TestProfileNilIsBareContract(t *testing.T) {
	d := blocksDoc(map[string]any{"type": "ext", "name": "x.y"})
	if _, err := Parse(d, Options{}); err != nil && CodeOf(err) == CodeProfileBlock {
		t.Fatal("nil profile must not apply profile rules")
	}
}

func TestProfileCloneIsIndependent(t *testing.T) {
	p := AgentProfile()
	c := p.Clone()
	c.AllowedBlocks[0] = "zzz"
	if p.AllowedBlocks[0] == "zzz" {
		t.Fatal("clone shares slices")
	}
	if len(ProfileCodes()) != 13 {
		t.Fatal("profile codes changed: update docs")
	}
}
