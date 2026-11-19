package v3

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func txt(k string) map[string]any {
	return map[string]any{"type": "text", "text": map[string]any{"$t": k}}
}

func refNode(r string) map[string]any { return map[string]any{"type": "ref", "ref": r} }

func blockDef(key string, extra map[string]any, blocks ...any) map[string]any {
	b := map[string]any{"key": key, "ui": pageBody(blocks...)}
	for k, v := range extra {
		b[k] = v
	}
	return b
}

func blockWidget(key string, extra map[string]any) map[string]any {
	w := map[string]any{"key": key, "title": "inventory.widget." + key, "kind": "block"}
	for k, v := range extra {
		w[k] = v
	}
	return w
}

func withBlocks(blocks, widgets, pages []any, requires ...string) map[string]any {
	m := baseValid()
	c := map[string]any{}
	if blocks != nil {
		c["blocks"] = blocks
	}
	if widgets != nil {
		c["dashboard"] = widgets
	}
	if pages != nil {
		c["pages"] = pages
	}
	m["contributions"] = c
	for _, r := range requires {
		cp := m["compatibility"].(map[string]interface{})
		cp["requires"] = append(cp["requires"].([]interface{}), map[string]interface{}{"key": r, "version": ">=1.0.0"})
	}
	return m
}

func expectErr(t *testing.T, err error, want string) {
	t.Helper()
	if want == "" {
		if err != nil {
			t.Fatalf("expected valid, got: %v", err)
		}
		return
	}
	if err == nil {
		t.Fatalf("expected error containing %q, got nil", want)
	}
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("error %q does not contain %q", err, want)
	}
}

// TestDashboardBlock_Widget covers kind:"block" with an inline ui and the
// refresh_s floor. Messages are those of the layer that rejects first: the
// JSON schema owns refresh_s>=10, the Go rules own the cross-field ones.
func TestDashboardBlock_Widget(t *testing.T) {
	ok := pageBody(txt("inventory.hello"))
	stat := func(extra map[string]any) map[string]any {
		w := map[string]any{"key": "s", "title": "t", "kind": "stat", "query": map[string]any{"model": "Stock"}}
		for k, v := range extra {
			w[k] = v
		}
		return w
	}
	cases := []struct {
		name string
		w    map[string]any
		want string // "" = valid
	}{
		{"valid block with ui", blockWidget("a", map[string]any{"ui": ok, "size": "full", "refresh_s": 15.0}), ""},
		{"valid block, refresh_s at the floor", blockWidget("a", map[string]any{"ui": ok, "refresh_s": 10.0}), ""},
		{"block without ui nor block", blockWidget("a", nil), "needs exactly one of ui"},
		{"block with query", blockWidget("a", map[string]any{"ui": ok, "query": map[string]any{"model": "Stock"}}), "query is not allowed for kind=block"},
		{"block with expose", blockWidget("a", map[string]any{"ui": ok, "expose": "./X"}), "expose is not allowed for kind=block"},
		{"ui on a stat", stat(map[string]any{"ui": ok}), "ui is only allowed for kind=block"},
		{"ui on a custom", map[string]any{"key": "s", "title": "t", "kind": "custom", "expose": "./X", "ui": ok}, "ui is only allowed for kind=block"},
		{"refresh_s below 10 (schema)", blockWidget("a", map[string]any{"ui": ok, "refresh_s": 9.0}), "refresh_s"},
		{"refresh_s on a stat is accepted", stat(map[string]any{"refresh_s": 30.0}), ""},
		{"ui not genui (unknown block)", blockWidget("a", map[string]any{"ui": pageBody(map[string]any{"type": "carousel3d"})}), "genui.unknown_block"},
		{"ui literal i18n (schema)", blockWidget("a", map[string]any{"ui": map[string]any{"version": "genui/v2", "i18n": "literal", "blocks": []any{}}}), "i18n"},
		{"ui too large", blockWidget("a", map[string]any{"ui": pageBody(map[string]any{"type": "markdown", "markdown": strings.Repeat("x", MaxWidgetUIBytes)})}), "bytes"},
		{"ui too deep", blockWidget("a", map[string]any{"ui": pageBody(nestedStacks(10))}), "genui.limit.depth"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := Validate(mustJSON(t, withDashboard([]interface{}{tc.w}, false)))
			expectErr(t, err, tc.want)
		})
	}
}

func TestBlockRefs_Validation(t *testing.T) {
	leaf := blockDef("fleet_map", nil, txt("inventory.map"))
	stat := map[string]any{"key": "s", "title": "t", "kind": "stat", "query": map[string]any{"model": "Stock"}, "block": "fleet_map"}
	clashA := map[string]any{"key": "a", "ui": map[string]any{"version": "genui/v2", "i18n": "keys", "data": map[string]any{"s": map[string]any{"q": "t:Stock"}}, "blocks": []any{txt("inventory.a")}}}
	clashB := map[string]any{"key": "b", "ui": map[string]any{"version": "genui/v2", "i18n": "keys", "data": map[string]any{"s": map[string]any{"q": "t:Other"}}, "blocks": []any{txt("inventory.b")}}}
	chain := func(keys ...string) []any {
		var out []any
		for i, k := range keys {
			if i+1 < len(keys) {
				out = append(out, blockDef(k, nil, refNode(keys[i+1])))
			} else {
				out = append(out, blockDef(k, nil, txt("inventory.t")))
			}
		}
		return out
	}
	byRef := func(ref string) []any { return []any{blockWidget("w", map[string]any{"block": ref})} }
	cases := []struct {
		name string
		m    map[string]any
		want string
	}{
		{"widget by block ref", withBlocks([]any{leaf}, byRef("fleet_map"), nil), ""},
		{"page with a ref node", withBlocks([]any{leaf}, nil, []any{page("p", pageBody(refNode("fleet_map"), txt("inventory.t")))}), ""},
		{"own ref qualified with the addon key", withBlocks([]any{leaf}, byRef("inventory.fleet_map"), nil), ""},
		{"foreign ref to a required addon", withBlocks(nil, byRef("hub.fleet_map"), nil, "hub"), ""},
		{"foreign ref to a non-required addon", withBlocks(nil, byRef("hub.fleet_map"), nil), "not in compatibility.requires"},
		{"unknown ref in a widget", withBlocks([]any{leaf}, byRef("ghost"), nil), "does not match any contributions.blocks"},
		{"unknown ref in a page", withBlocks([]any{leaf}, nil, []any{page("p", pageBody(refNode("ghost")))}), "does not match any contributions.blocks"},
		{"ui and block together", withBlocks([]any{leaf}, []any{blockWidget("w", map[string]any{"block": "fleet_map", "ui": pageBody(txt("inventory.x"))})}, nil), "both ui and block"},
		{"block on a stat", withBlocks([]any{leaf}, []any{stat}, nil), "block is only allowed for kind=block"},
		{"direct cycle", withBlocks([]any{blockDef("a", nil, refNode("a"))}, nil, nil), "ref cycle"},
		{"mutual cycle", withBlocks([]any{blockDef("a", nil, refNode("b")), blockDef("b", nil, refNode("a"))}, byRef("a"), nil), "ref cycle"},
		{"nesting at the limit", withBlocks(chain("a", "b", "c"), byRef("a"), nil), ""},
		{"nesting too deep", withBlocks(chain("a", "b", "c", "d"), byRef("a"), nil), "nests deeper"},
		{"ref node with extra keys", withBlocks([]any{leaf}, nil, []any{page("p", pageBody(map[string]any{"type": "ref", "ref": "fleet_map", "area": "x"}))}), "only accepts type and ref"},
		{"ref with a bad name", withBlocks([]any{leaf}, nil, []any{page("p", pageBody(refNode("Bad Key")))}), "must be"},
		{"data name clash", withBlocks([]any{clashA, clashB}, nil, []any{page("p", pageBody(refNode("a"), refNode("b")))}), "defines data source"},
		{"duplicate block key", withBlocks([]any{leaf, leaf}, nil, nil), "duplicated"},
		{"bad block key (schema)", withBlocks([]any{blockDef("Bad", nil, txt("inventory.t"))}, nil, nil), "key"},
		{"bad block permission", withBlocks([]any{blockDef("a", map[string]any{"permission": "No Good"}, txt("inventory.t"))}, nil, nil), "permission"},
		{"block without ui (schema)", withBlocks([]any{map[string]any{"key": "a"}}, nil, nil), "ui"},
		{"block ui invalid genui", withBlocks([]any{blockDef("a", nil, map[string]any{"type": "carousel3d"})}, nil, nil), "genui.unknown_block"},
		{"block ui too large", withBlocks([]any{blockDef("a", nil, map[string]any{"type": "markdown", "markdown": strings.Repeat("x", MaxBlockUIBytes)})}, nil, nil), "bytes"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			expectErr(t, Validate(mustJSON(t, tc.m)), tc.want)
		})
	}
}

func parseExpanded(t *testing.T, m map[string]any, foreign ForeignBlocks) (*Manifest, error) {
	t.Helper()
	parsed, err := Parse(mustJSON(t, m))
	if err != nil {
		t.Fatalf("manifest must be valid: %v", err)
	}
	return ExpandBlockRefs(parsed, foreign)
}

func jsonEq(t *testing.T, raw json.RawMessage, want any) {
	t.Helper()
	var got any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	var w any
	b, _ := json.Marshal(want)
	_ = json.Unmarshal(b, &w)
	if !reflect.DeepEqual(got, w) {
		t.Fatalf("got %s, want %s", raw, b)
	}
}

func TestExpandBlockRefs(t *testing.T) {
	src := map[string]any{"q": "t:Stock"}
	withData := map[string]any{"key": "fleet_map", "permission": "inventory.stock.index", "ui": map[string]any{
		"version": "genui/v2", "i18n": "keys", "data": map[string]any{"s": src}, "blocks": []any{txt("inventory.map")}}}
	plain := blockDef("kpis", nil, txt("inventory.k1"), txt("inventory.k2"))
	m := withBlocks([]any{withData, plain},
		[]any{blockWidget("w", map[string]any{"block": "fleet_map"})},
		[]any{page("p", pageBody(txt("inventory.top"), refNode("kpis"), refNode("fleet_map")))})

	em, err := parseExpanded(t, m, nil)
	if err != nil {
		t.Fatal(err)
	}
	w := em.Contributions.Dashboard[0]
	if w.Block != "" {
		t.Fatalf("expanded widget keeps block %q", w.Block)
	}
	section := map[string]any{"type": "section", "permission": "inventory.stock.index", "blocks": []any{txt("inventory.map")}}
	jsonEq(t, w.UI, map[string]any{"version": "genui/v2", "i18n": "keys", "data": map[string]any{"s": src}, "blocks": []any{section}})
	jsonEq(t, em.Contributions.Pages[0].UI, map[string]any{"version": "genui/v2", "i18n": "keys", "data": map[string]any{"s": src},
		"blocks": []any{txt("inventory.top"), txt("inventory.k1"), txt("inventory.k2"), section}})

	// The input is untouched.
	orig, _ := Parse(mustJSON(t, m))
	if _, err := ExpandBlockRefs(orig, nil); err != nil {
		t.Fatal(err)
	}
	if orig.Contributions.Dashboard[0].Block != "fleet_map" {
		t.Fatal("ExpandBlockRefs mutated its input")
	}

	// Idempotent: expanding the expansion changes nothing.
	again, err := ExpandBlockRefs(em, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(em.Contributions, again.Contributions) {
		t.Fatal("expansion is not idempotent")
	}
	// The expansion is itself a valid manifest with no ref nodes left.
	raw, _ := json.Marshal(em)
	if err := Validate(raw); err != nil {
		t.Fatalf("expanded manifest does not validate: %v", err)
	}
}

func TestExpandBlockRefs_Foreign(t *testing.T) {
	m := withBlocks(nil,
		[]any{blockWidget("w", map[string]any{"block": "hub.fleet_map"})},
		[]any{page("p", pageBody(refNode("hub.fleet_map")))}, "hub")

	// Without a catalog the foreign refs are left for the host.
	em, err := parseExpanded(t, m, nil)
	if err != nil {
		t.Fatal(err)
	}
	if em.Contributions.Dashboard[0].Block != "hub.fleet_map" || len(em.Contributions.Dashboard[0].UI) != 0 {
		t.Fatalf("foreign widget ref must stay a ref: %+v", em.Contributions.Dashboard[0])
	}
	if !strings.Contains(string(em.Contributions.Pages[0].UI), `"ref"`) {
		t.Fatal("foreign page ref must stay a ref")
	}

	// With the (already expanded) catalog they are resolved.
	cat := ForeignBlocks{"hub": {"fleet_map": BlockDef{Key: "fleet_map", UI: mustJSON(t, pageBody(txt("hub.map")))}}}
	em, err = parseExpanded(t, m, cat)
	if err != nil {
		t.Fatal(err)
	}
	jsonEq(t, em.Contributions.Dashboard[0].UI, pageBody(txt("hub.map")))
	jsonEq(t, em.Contributions.Pages[0].UI, pageBody(txt("hub.map")))

	// A catalog without that block is an error.
	_, err = parseExpanded(t, m, ForeignBlocks{"hub": {}})
	expectErr(t, err, `declares no block "fleet_map"`)
}
