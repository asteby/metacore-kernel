package v3

import (
	"fmt"
	"strings"
	"testing"
)

func pageBody(blocks ...any) map[string]any {
	return map[string]any{"version": "genui/v2", "i18n": "keys", "blocks": blocks}
}

func withPages(pages []any, nav []any) map[string]any {
	m := baseValid()
	c := map[string]any{"pages": pages}
	if nav != nil {
		c["navigation"] = []any{map[string]any{"title": "inventory.nav.group", "items": nav}}
	}
	m["contributions"] = c
	return m
}

func page(key string, ui any) map[string]any {
	return map[string]any{"key": key, "title": "inventory.pages." + key, "ui": ui}
}

func nestedStacks(depth int) map[string]any {
	var b any = map[string]any{"type": "text", "text": map[string]any{"$t": "inventory.t"}}
	for i := 0; i < depth; i++ {
		b = map[string]any{"type": "stack", "children": []any{b}}
	}
	return b.(map[string]any)
}

func TestPages(t *testing.T) {
	okBlock := map[string]any{"type": "text", "text": map[string]any{"$t": "inventory.pages.hello"}}

	tests := []struct {
		name string
		m    map[string]any
		want string // "" = valid; else substring of the error
	}{
		{"valid page and nav", withPages([]any{page("fleet_map", pageBody(okBlock))},
			[]any{map[string]any{"title": "inventory.nav.map", "page": "fleet_map", "permission": "inventory.stock.index"}}), ""},
		{"valid with permission and icon", withPages([]any{func() any {
			p := page("fleet_map", pageBody(okBlock))
			p["permission"] = "hub_instances.index"
			p["icon"] = "MapPin"
			return p
		}()}, nil), ""},
		{"valid nested items nav", withPages([]any{page("a", pageBody(okBlock))},
			[]any{map[string]any{"title": "g", "items": []any{map[string]any{"title": "x", "page": "a"}}}}), ""},
		{"no pages keeps working", baseValid(), ""},
		{"duplicate key", withPages([]any{page("a", pageBody(okBlock)), page("a", pageBody(okBlock))}, nil), "is duplicated"},
		{"key not snake_case", withPages([]any{page("Fleet-Map", pageBody(okBlock))}, nil), "snake_case"},
		{"missing key", withPages([]any{map[string]any{"title": "t", "ui": pageBody(okBlock)}}, nil), "key"},
		{"missing title", withPages([]any{map[string]any{"key": "a", "ui": pageBody(okBlock)}}, nil), "title"},
		{"bad permission format", withPages([]any{func() any {
			p := page("a", pageBody(okBlock))
			p["permission"] = "Not A Permission"
			return p
		}()}, nil), "permission"},
		{"missing ui", withPages([]any{map[string]any{"key": "a", "title": "t"}}, nil), "ui"},
		{"literal i18n rejected", withPages([]any{page("a", map[string]any{"version": "genui/v2", "i18n": "literal", "blocks": []any{}})}, nil), "i18n"},
		{"wrong version", withPages([]any{page("a", map[string]any{"version": "genui/v9", "i18n": "keys", "blocks": []any{}})}, nil), "ui"},
		{"unknown block", withPages([]any{page("a", pageBody(map[string]any{"type": "carousel3d"}))}, nil), "genui.unknown_block"},
		{"proposed block not in schema yet (map)", withPages([]any{page("a", pageBody(map[string]any{"type": "map"}))}, nil), "genui.unknown_block"},
		{"too deep", withPages([]any{page("a", pageBody(nestedStacks(10)))}, nil), "genui.limit.depth"},
		{"depth at the limit is fine", withPages([]any{page("a", pageBody(nestedStacks(4)))}, nil), ""},
		{"too many blocks", withPages([]any{page("a", pageBody(manyBlocks(MaxPageBlocksProbe)...))}, nil), "genui."},
		{"body too large", withPages([]any{page("a", pageBody(map[string]any{"type": "markdown", "markdown": strings.Repeat("x", MaxPageUIBytes)}))}, nil), "bytes"},
		{"nav page unknown", withPages([]any{page("a", pageBody(okBlock))},
			[]any{map[string]any{"title": "x", "page": "ghost"}}), "does not match any contributions.pages"},
		{"nav page without pages", func() map[string]any {
			m := baseValid()
			m["contributions"] = map[string]any{"navigation": []any{map[string]any{"title": "g", "items": []any{map[string]any{"title": "x", "page": "ghost"}}}}}
			return m
		}(), "does not match any contributions.pages"},
		{"nav page with url", withPages([]any{page("a", pageBody(okBlock))},
			[]any{map[string]any{"title": "x", "page": "a", "url": "/x"}}), "cannot be combined"},
		{"nav page with model", withPages([]any{page("a", pageBody(okBlock))},
			[]any{map[string]any{"title": "x", "page": "a", "model": "Stock"}}), "cannot be combined"},
		{"nav page with view_type", withPages([]any{page("a", pageBody(okBlock))},
			[]any{map[string]any{"title": "x", "page": "a", "view_type": "kanban", "group_by": "status"}}), "cannot be combined"},
		{"nav page bad format", withPages([]any{page("a", pageBody(okBlock))},
			[]any{map[string]any{"title": "x", "page": "Bad Key"}}), "page"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := Validate(mustJSON(t, tc.m))
			if tc.want == "" {
				if err != nil {
					t.Fatalf("expected valid, got: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not contain %q", err, tc.want)
			}
		})
	}
}

// MaxPageBlocksProbe is comfortably above genui/v2's block limit.
const MaxPageBlocksProbe = 300

func manyBlocks(n int) []any {
	out := make([]any, n)
	for i := range out {
		out[i] = map[string]any{"type": "text", "text": map[string]any{"$t": fmt.Sprintf("inventory.k%d", i)}}
	}
	return out
}

func TestPages_TooManyPages(t *testing.T) {
	var pages []any
	for i := 0; i <= MaxContributionPages; i++ {
		pages = append(pages, page(fmt.Sprintf("p%d", i), pageBody(map[string]any{"type": "text", "text": map[string]any{"$t": "inventory.t"}})))
	}
	err := Validate(mustJSON(t, withPages(pages, nil)))
	if err == nil {
		t.Fatal("expected an error")
	}
}
