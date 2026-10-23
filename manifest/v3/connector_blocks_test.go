package v3

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

const blocksExample = "../../docs/spec/v3/examples/addon-connector-blocks-example.json"

// mutateExample loads the blocks example, applies mut to its generic JSON form
// and returns the re-encoded manifest.
func mutateExample(t *testing.T, mut func(doc map[string]any)) []byte {
	t.Helper()
	raw, err := os.ReadFile(blocksExample)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	mut(doc)
	out, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func sections(doc map[string]any) []any {
	c := doc["connectors"].([]any)[0].(map[string]any)
	return c["form_layout"].(map[string]any)["sections"].([]any)
}

func setBlock(doc map[string]any, si, bi int, b map[string]any) {
	sec := sections(doc)[si].(map[string]any)
	bl := sec["blocks"].([]any)
	bl[bi] = b
}

func TestConnectorBlocks_ExampleParsesBlocks(t *testing.T) {
	raw, err := os.ReadFile(blocksExample)
	if err != nil {
		t.Fatal(err)
	}
	m, err := Parse(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	fl := m.Connectors[0].FormLayout
	if !FormLayoutHasBlocks(fl) {
		t.Fatal("blocks lost in decode")
	}
	b := fl.Sections[1].Blocks[0]
	if b.Kind != FormBlockWebhookURL || b.Webhook != "events" || len(b.Links) != 1 {
		t.Fatalf("webhook block decoded wrong: %+v", b)
	}
	got := FormLayoutBlockExports(fl)
	if strings.Join(got, ",") != "handle_connector_test,handle_setup_test_order" {
		t.Fatalf("block exports = %v", got)
	}
}

func TestConnectorBlocks_Rejections(t *testing.T) {
	cases := []struct {
		name string
		mut  func(doc map[string]any)
		want string
	}{
		{"unknown webhook", func(d map[string]any) {
			setBlock(d, 1, 0, map[string]any{"key": "webhook_url", "kind": "webhook_url", "webhook": "nope"})
		}, `webhook "nope" is not a webhooks[].key`},
		{"webhook kind without webhook", func(d map[string]any) {
			setBlock(d, 1, 0, map[string]any{"key": "webhook_url", "kind": "webhook_url"})
		}, "webhook is required"},
		{"action without export", func(d map[string]any) {
			setBlock(d, 1, 1, map[string]any{"key": "test_order", "kind": "action", "label": "x"})
		}, "export is required"},
		{"instructions empty", func(d map[string]any) {
			setBlock(d, 0, 0, map[string]any{"key": "where_keys", "kind": "instructions"})
		}, "needs a description or links"},
		{"instructions with export", func(d map[string]any) {
			setBlock(d, 0, 0, map[string]any{"key": "where_keys", "kind": "instructions", "description": "x", "export": "fn"})
		}, "do not apply to kind instructions"},
		{"duplicated key across steps", func(d map[string]any) {
			setBlock(d, 1, 1, map[string]any{"key": "test_connection", "kind": "action", "export": "fn"})
		}, `"test_connection" is duplicated`},
		{"http link rejected by schema", func(d map[string]any) {
			setBlock(d, 0, 0, map[string]any{"key": "where_keys", "kind": "instructions",
				"links": []any{map[string]any{"label": "x", "url": "http://insecure.example"}}})
		}, "schema validation failed"},
		{"unknown kind rejected by schema", func(d map[string]any) {
			setBlock(d, 0, 0, map[string]any{"key": "where_keys", "kind": "iframe"})
		}, "schema validation failed"},
		{"blocks on a model form_layout", func(d map[string]any) {
			d["models"] = []any{map[string]any{
				"key": "Thing", "table": "things", "label": "Thing",
				"columns": []any{
					map[string]any{"name": "id", "type": "uuid", "primary_key": true, "default": "gen_random_uuid()"},
					map[string]any{"name": "organization_id", "type": "uuid", "not_null": true},
				},
				"form_layout": map[string]any{"mode": "steps", "sections": []any{map[string]any{
					"key": "a", "blocks": []any{map[string]any{"key": "b", "kind": "instructions", "description": "x"}},
				}}},
			}}
			d["capabilities"] = append(d["capabilities"].([]any), map[string]any{"kind": "db:write", "target": "addon_acmepay.*"})
		}, "blocks are only supported in connectors[].form_layout"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := Validate(mutateExample(t, tc.mut))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want error containing %q, got %v", tc.want, err)
			}
		})
	}
}

func TestConnectorBlocks_TooManyPerSection(t *testing.T) {
	errs := ValidateConnectorBlocks("connectors[0]", &FormLayout{Mode: "steps", Sections: []FormSection{{
		Key: "s", Blocks: func() []FormBlock {
			var bs []FormBlock
			for i := 0; i <= MaxFormBlocksPerSection; i++ {
				bs = append(bs, FormBlock{Key: "b" + string(rune('a'+i)), Kind: FormBlockInstructions, Description: "x"})
			}
			return bs
		}(),
	}}}, nil)
	if len(errs) == 0 || !strings.Contains(errs[0], "max 8") {
		t.Fatalf("expected a max-blocks error, got %v", errs)
	}
}

// Back-compat: a connector form_layout without blocks validates and decodes
// exactly as before (no blocks key in the re-encoded section).
func TestConnectorBlocks_AbsentIsNoop(t *testing.T) {
	raw := mutateExample(t, func(d map[string]any) {
		for _, s := range sections(d) {
			delete(s.(map[string]any), "blocks")
		}
	})
	m, err := Parse(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if FormLayoutHasBlocks(m.Connectors[0].FormLayout) {
		t.Fatal("unexpected blocks")
	}
	out, _ := json.Marshal(m.Connectors[0].FormLayout)
	if strings.Contains(string(out), "blocks") {
		t.Fatalf("empty blocks must be omitted: %s", out)
	}
}
