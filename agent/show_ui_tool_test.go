package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/asteby/metacore-kernel/agent/contextmgr"
	"github.com/asteby/metacore-kernel/agent/tools"
	genuiv2 "github.com/asteby/metacore-kernel/genui/v2"
	manifestv3 "github.com/asteby/metacore-kernel/manifest/v3"
	"github.com/google/uuid"
)

// fakeHost implements UIAuthorizer the way a host would: models the user may
// read, a block catalog expanded with the kernel's own helper, and an audit log.
type fakeHost struct {
	readable  map[string]bool // "t:orders" / widget key -> allowed
	catalog   manifestv3.ForeignBlocks
	audited   []UIAuditEntry
	auditErr  error
	lastID    UIIdentity
	authCalls int
}

func (h *fakeHost) AuthorizeSources(_ context.Context, id UIIdentity, ss []UISource) error {
	h.authCalls++
	h.lastID = id
	var denied []string
	for _, s := range ss {
		if s.Kind == "q" && !h.readable[strings.SplitN(s.Value, "?", 2)[0]] {
			denied = append(denied, s.Name)
		}
		if s.Kind == "widget" && !h.readable[s.Value] {
			denied = append(denied, s.Name)
		}
	}
	if len(denied) > 0 {
		return &SourceDeniedError{Sources: denied}
	}
	return nil
}

func (h *fakeHost) ListBlocks(context.Context, UIIdentity) ([]BlockInfo, error) {
	var out []BlockInfo
	for addon, bs := range h.catalog {
		for k, b := range bs {
			out = append(out, BlockInfo{Ref: addon + "." + k, Title: b.Title})
		}
	}
	return out, nil
}

func (h *fakeHost) ListWidgets(context.Context, UIIdentity) ([]WidgetInfo, error) {
	return []WidgetInfo{{Key: "hub_fleet.dashboard.by_country", Title: "Flota por pais"}}, nil
}

func (h *fakeHost) ExpandRefs(_ context.Context, _ UIIdentity, doc map[string]any) (map[string]any, error) {
	return manifestv3.ExpandDocBlockRefs(doc, h.catalog)
}

func (h *fakeHost) RecordUI(_ context.Context, _ UIIdentity, e UIAuditEntry) error {
	h.audited = append(h.audited, e)
	if e.Accepted {
		return h.auditErr
	}
	return nil
}

func newFakeHost() *fakeHost {
	return &fakeHost{
		readable: map[string]bool{"t:orders": true, "hub_fleet.dashboard.by_country": true},
		catalog: manifestv3.ForeignBlocks{"hub_fleet": {"fleet_globe": manifestv3.BlockDef{
			Key: "fleet_globe", Title: "Globo de flota",
			UI: []byte(`{"version":"genui/v2","data":{"fleet":{"widget":"hub_fleet.dashboard.by_country"}},"blocks":[{"type":"metric","label":"Flota","source":"data.fleet","field":"n"}]}`),
		}}},
	}
}

func uiConv() *contextmgr.ConversationContext {
	return &contextmgr.ConversationContext{
		ConversationID: uuid.New(), OrganizationID: uuid.New(), UserID: uuid.New(),
		Metadata: map[string]any{ScopeMetadataKey: map[string]string{"branch_id": "b1"}},
	}
}

func salesChart(source map[string]any) map[string]any {
	return map[string]any{
		"version": "genui/v2",
		"data":    map[string]any{"sales": source},
		"blocks": []any{map[string]any{
			"type": "chart", "kind": "bar", "x": "month", "summary": "Ventas por mes",
			"series": []any{map[string]any{"key": "total", "label": "Total"}},
			"source": "data.sales",
		}},
	}
}

func TestShowUITool(t *testing.T) {
	q := map[string]any{"q": "t:orders?group=month"}
	cardWith := func(a map[string]any) map[string]any {
		return map[string]any{"version": "genui/v2", "blocks": []any{map[string]any{"type": "card", "title": "x", "actions": []any{a}}}}
	}
	refDoc := func(ref string) map[string]any {
		return map[string]any{"version": "genui/v2", "blocks": []any{map[string]any{"type": "ref", "ref": ref}}}
	}
	blocks := make([]any, 30)
	for i := range blocks {
		blocks[i] = map[string]any{"type": "text", "text": "x"}
	}
	tests := []struct {
		name   string
		params map[string]any
		mut    func(h *fakeHost)
		code   string // "" = accepted
	}{
		{name: "valid chart from authorized source", params: map[string]any{"doc": salesChart(q), "fallback": "Ventas al alza."}},
		{name: "valid widget source", params: map[string]any{"doc": map[string]any{"version": "genui/v2",
			"data":   map[string]any{"w": map[string]any{"widget": "hub_fleet.dashboard.by_country"}},
			"blocks": []any{map[string]any{"type": "metric", "label": "Flota", "source": "data.w", "field": "n"}}}}},
		{name: "doc as JSON text", params: map[string]any{"doc": `{"version":"genui/v2","blocks":[{"type":"markdown","markdown":"hola"}]}`}},
		{name: "missing doc", params: map[string]any{}, code: CodeUINoDocument},
		{name: "forbidden block ext", params: map[string]any{"doc": map[string]any{"version": "genui/v2", "blocks": []any{map[string]any{"type": "ext", "name": "x.y"}}}}, code: genuiv2.CodeProfileBlock},
		{name: "action not allowed", params: map[string]any{"doc": cardWith(map[string]any{"do": "invoke", "action": "a.B.c"})}, code: genuiv2.CodeProfileVerb},
		{name: "emit not whitelisted", params: map[string]any{"doc": cardWith(map[string]any{"do": "emit", "id": "aby.x"})}, code: genuiv2.CodeProfileEmit},
		{name: "external link", params: map[string]any{"doc": cardWith(map[string]any{"do": "navigate", "route": map[string]any{"path": "https://evil.test/x"}})}, code: genuiv2.CodeUnsafeLink},
		{name: "source outside profile whitelist", params: map[string]any{"doc": salesChart(map[string]any{"q": "m:model"})}, code: genuiv2.CodeProfileSource},
		{name: "source denied for this user", params: map[string]any{"doc": salesChart(map[string]any{"q": "t:payroll?x=1"})}, code: CodeUISourceDenied},
		{name: "typed figures in chart", params: map[string]any{"doc": map[string]any{"version": "genui/v2", "blocks": []any{map[string]any{
			"type": "chart", "kind": "bar", "x": "m", "summary": "s",
			"series": []any{map[string]any{"key": "t", "label": "T"}}, "rows": []any{map[string]any{"m": "ene", "t": 5.0}}}}}}, code: genuiv2.CodeProfileLiteral},
		{name: "oversize document", params: map[string]any{"doc": map[string]any{"version": "genui/v2", "blocks": blocks}}, code: genuiv2.CodeProfileBlocks},
		{name: "inline undeclared", params: map[string]any{"doc": salesChart(map[string]any{"inline": []any{map[string]any{"month": "ene", "total": 1.0}}})}, code: CodeUIInlineUndecl},
		{name: "inline declared", params: map[string]any{
			"doc":            salesChart(map[string]any{"inline": []any{map[string]any{"month": "ene", "total": 1.0}}}),
			"inline_sources": []any{"sales"}}},
		{name: "ref valid", params: map[string]any{"doc": refDoc("hub_fleet.fleet_globe")}},
		{name: "ref unknown", params: map[string]any{"doc": refDoc("hub_fleet.nope")}, code: CodeUIRefUnresolved},
		{name: "ref whose widget the user cannot read", mut: func(h *fakeHost) { delete(h.readable, "hub_fleet.dashboard.by_country") },
			params: map[string]any{"doc": refDoc("hub_fleet.fleet_globe")}, code: CodeUISourceDenied},
		{name: "audit failure blocks display", mut: func(h *fakeHost) { h.auditErr = errors.New("db down") }, params: map[string]any{"doc": salesChart(q)}, code: CodeUIAuditFailed},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newFakeHost()
			if tc.mut != nil {
				tc.mut(h)
			}
			tool := NewShowUITool(UIHostFrom(h))
			conv := uiConv()
			res, err := tool.Execute(context.Background(), tc.params, conv)
			if tc.code != "" {
				var rej *UIRejection
				if !errors.As(err, &rej) {
					t.Fatalf("want rejection %s, got res=%v err=%v", tc.code, res, err)
				}
				if rej.Code != tc.code {
					t.Fatalf("code = %s (%s), want %s", rej.Code, rej.Message, tc.code)
				}
				if res != nil {
					t.Fatal("a rejection must not carry a result")
				}
				if !strings.Contains(rej.Error(), tc.code) {
					t.Fatalf("model-facing error lacks the code: %s", rej.Error())
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected: %v", err)
			}
			r, ok := res.(*ShowUIResult)
			if !ok || !r.OK || r.Version != genuiv2.Version || len(r.DocHash) != 64 || r.Fallback == "" {
				t.Fatalf("result: %+v", res)
			}
			if h.authCalls > 0 && (h.lastID.OrganizationID != conv.OrganizationID || h.lastID.UserID != conv.UserID || h.lastID.Scope["branch_id"] != "b1") {
				t.Fatalf("identity not taken from the conversation: %+v", h.lastID)
			}
			if len(h.audited) == 0 || !h.audited[len(h.audited)-1].Accepted || h.audited[len(h.audited)-1].DocHash != r.DocHash {
				t.Fatalf("audit: %+v", h.audited)
			}
			if strings.Contains(r.LLMText(), "series") {
				t.Fatalf("LLM text must not echo the document: %s", r.LLMText())
			}
		})
	}
}

func TestShowUIFailsClosedWithoutResolver(t *testing.T) {
	tool := NewShowUITool(UIHost{}) // no host ports at all
	_, err := tool.Execute(context.Background(), map[string]any{"doc": salesChart(map[string]any{"q": "t:orders"})}, uiConv())
	var rej *UIRejection
	if !errors.As(err, &rej) || rej.Code != CodeUISourcesNoHost || rej.Retryable {
		t.Fatalf("got %v", err)
	}
	// Text-only documents need no host at all.
	doc := map[string]any{"version": "genui/v2", "blocks": []any{map[string]any{"type": "markdown", "markdown": "ok"}}}
	if _, err := tool.Execute(context.Background(), map[string]any{"doc": doc}, uiConv()); err != nil {
		t.Fatal(err)
	}
}

func TestShowUIRejectionIsAudited(t *testing.T) {
	h := newFakeHost()
	tool := NewShowUITool(UIHostFrom(h))
	doc := map[string]any{"version": "genui/v2", "blocks": []any{map[string]any{"type": "ext", "name": "a.b"}}}
	_, _ = tool.Execute(context.Background(), map[string]any{"doc": doc}, uiConv())
	if len(h.audited) != 1 || h.audited[0].Accepted || h.audited[0].Code != genuiv2.CodeProfileBlock {
		t.Fatalf("audit: %+v", h.audited)
	}
}

func TestShowUICustomProfile(t *testing.T) {
	p := genuiv2.AgentProfile()
	p.AllowedEmits = []string{"aby.guide"}
	tool := NewShowUITool(UIHostFrom(newFakeHost()), WithUIProfile(p))
	doc := map[string]any{"version": "genui/v2", "blocks": []any{map[string]any{"type": "card", "title": "x",
		"actions": []any{map[string]any{"do": "emit", "id": "aby.guide"}}}}}
	if _, err := tool.Execute(context.Background(), map[string]any{"doc": doc}, uiConv()); err != nil {
		t.Fatal(err)
	}
}

func TestListCatalogTools(t *testing.T) {
	h := newFakeHost()
	res, err := NewListBlocksTool(h).Execute(context.Background(), nil, uiConv())
	if err != nil || res.(map[string]any)["count"] != 1 {
		t.Fatalf("%v %v", res, err)
	}
	res, err = NewListWidgetsTool(h).Execute(context.Background(), nil, uiConv())
	if err != nil || res.(map[string]any)["count"] != 1 {
		t.Fatalf("%v %v", res, err)
	}
	if res, _ := NewListBlocksTool(nil).Execute(context.Background(), nil, uiConv()); res.(map[string]any)["count"] != 0 {
		t.Fatal("nil catalog must list nothing")
	}
}

func TestShowUIEmbedAndDowngrade(t *testing.T) {
	h := newFakeHost()
	res, err := NewShowUITool(UIHostFrom(h)).Execute(context.Background(),
		map[string]any{"doc": salesChart(map[string]any{"q": "t:orders?group=month"}), "fallback": "Ventas al alza."}, uiConv())
	if err != nil {
		t.Fatal(err)
	}
	embeds := NewEmbedBuilder(noModels{}).FromToolResult("show_ui", res, nil)
	if len(embeds) != 1 || embeds[0].Type != "ui" || embeds[0].Version != genuiv2.Version || embeds[0].UIV2 == nil || embeds[0].Fallback != "Ventas al alza." {
		t.Fatalf("embeds: %+v", embeds)
	}

	tests := []struct {
		name   string
		caps   *genuiv2.Caps
		wantV2 bool
		wantV1 bool
	}{
		{name: "v2 client", caps: &genuiv2.Caps{Versions: []string{"genui/v2"}}, wantV2: true},
		{name: "v1-only client", caps: &genuiv2.Caps{Versions: []string{"genui/v1"}}, wantV1: true},
		{name: "nil caps = v1 client", caps: nil, wantV1: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := DowngradeEmbeds(embeds, tc.caps)
			if len(got) != 1 || got[0].Type != "ui" {
				t.Fatalf("got %+v", got)
			}
			if (got[0].UIV2 != nil) != tc.wantV2 || (got[0].UI != nil) != tc.wantV1 {
				t.Fatalf("v2=%v v1=%v: %+v", got[0].UIV2 != nil, got[0].UI != nil, got[0])
			}
		})
	}

	// A document the client cannot degrade still delivers the conclusion as text.
	broken := []ChatEmbed{{Type: "ui", Version: genuiv2.Version, Fallback: "Ventas al alza.", UIV2: genuiv2.Doc{"version": "genui/v2", "blocks": []any{}}}}
	got := DowngradeEmbeds(broken, &genuiv2.Caps{Versions: []string{"genui/v2"}})
	if len(got) != 1 || got[0].Type != "text" || got[0].Fallback != "Ventas al alza." || got[0].UIV2 != nil {
		t.Fatalf("text degradation: %+v", got)
	}
	if out := DowngradeEmbeds([]ChatEmbed{{Type: "record", ID: "1"}}, nil); len(out) != 1 || out[0].Type != "record" {
		t.Fatalf("passthrough: %+v", out)
	}
}

func TestForeignV2UIGoesThroughBuilderProfile(t *testing.T) {
	b := NewEmbedBuilder(noModels{})
	bad := map[string]any{"version": "genui/v2", "blocks": []any{map[string]any{"type": "ext", "name": "a.b"}}}
	if got := b.FromToolResult("other_tool", map[string]any{"ui": bad}, nil); len(got) != 0 {
		t.Fatalf("ext rendered: %+v", got)
	}
	ok := map[string]any{"version": "genui/v2", "blocks": []any{map[string]any{"type": "markdown", "markdown": "hola"}}}
	got := b.FromToolResult("other_tool", map[string]any{"ui": ok, "fallback": "hola"}, nil)
	if len(got) != 1 || got[0].UIV2 == nil || got[0].Version != genuiv2.Version {
		t.Fatalf("v2 embed: %+v", got)
	}
	v1 := map[string]any{"version": "genui/v1", "blocks": []any{map[string]any{"type": "text", "text": "Hola"}}}
	if got := b.FromToolResult("other_tool", map[string]any{"ui": v1}, nil); len(got) != 1 || got[0].UI == nil || got[0].UIV2 != nil {
		t.Fatalf("v1 embed: %+v", got)
	}
	two := DedupeEmbeds([]ChatEmbed{
		{Type: "ui", UIV2: genuiv2.Doc{"version": "genui/v2", "blocks": []any{map[string]any{"type": "text", "text": "a"}}}},
		{Type: "ui", UIV2: genuiv2.Doc{"version": "genui/v2", "blocks": []any{map[string]any{"type": "text", "text": "b"}}}},
	})
	if len(two) != 2 {
		t.Fatalf("deduped: %d", len(two))
	}
}

func TestShowChartDeprecatedStillWorksAndCanEmitUI(t *testing.T) {
	params := map[string]any{
		"title": "Ventas", "kind": "bar", "x_key": "month",
		"series": []any{map[string]any{"key": "total", "label": "Total"}},
		"rows":   []any{map[string]any{"month": "ene", "total": 10.0}, map[string]any{"month": "feb", "total": 12.0}},
	}
	b := NewEmbedBuilder(noModels{})

	legacy, err := NewShowChartTool().Execute(context.Background(), params, nil)
	if err != nil {
		t.Fatal(err)
	}
	if e := b.FromToolResult("show_chart", legacy, nil); len(e) != 1 || e[0].Type != "chart" || e[0].Chart == nil {
		t.Fatalf("legacy embed changed: %+v", e)
	}

	mig, err := NewShowChartTool().EmitUI().Execute(context.Background(), params, nil)
	if err != nil {
		t.Fatal(err)
	}
	if e := b.FromToolResult("show_chart", mig, nil); len(e) != 1 || e[0].Type != "ui" || e[0].UIV2 == nil {
		t.Fatalf("migrated embed: %+v", e)
	}

	// A kind with no v2 equivalent keeps the legacy embed instead of being drawn as another chart.
	params["kind"] = "radar"
	rad, _ := NewShowChartTool().EmitUI().Execute(context.Background(), params, nil)
	if e := b.FromToolResult("show_chart", rad, nil); len(e) != 1 || e[0].Type != "chart" {
		t.Fatalf("radar: %+v", e)
	}
}

func TestShowUISatisfiesChartClaim(t *testing.T) {
	reg := tools.NewToolRegistry()
	reg.Register(NewShowChartTool())
	reg.Register(NewShowUITool(UIHost{}))
	msg := "Aquí está el gráfico de ventas"
	if got := unbackedClaim(reg, msg, map[string]bool{}); got == "" {
		t.Fatal("claim without any tool must be flagged")
	}
	for _, ran := range []string{"show_ui", "show_chart"} {
		if got := unbackedClaim(reg, msg, map[string]bool{ran: true}); got != "" {
			t.Fatalf("%s success must back the claim, got %q", ran, got)
		}
	}
}
