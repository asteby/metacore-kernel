package v3

import (
	"strings"
	"testing"
)

func TestExpandDocBlockRefs(t *testing.T) {
	fleet := BlockDef{
		Key:        "fleet_globe",
		Permission: "fleet.read",
		UI:         []byte(`{"version":"genui/v2","data":{"fleet":{"widget":"hub_fleet.dashboard.by_country"}},"blocks":[{"type":"metric","label":"Flota","source":"data.fleet","field":"n"}]}`),
	}
	catalog := ForeignBlocks{"hub_fleet": {"fleet_globe": fleet}}
	mk := func(ref string) map[string]any {
		return map[string]any{"version": "genui/v2", "blocks": []any{map[string]any{"type": "ref", "ref": ref}}}
	}
	tests := []struct {
		name    string
		ref     string
		wantErr string
	}{
		{"valid", "hub_fleet.fleet_globe", ""},
		{"unknown block", "hub_fleet.nope", "declares no block"},
		{"unknown addon", "ghost.x", "not in compatibility.requires"},
		{"unqualified", "fleet_globe", "does not match any"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			in := mk(tc.ref)
			out, err := ExpandDocBlockRefs(in, catalog)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			blocks := out["blocks"].([]any)
			sec := blocks[0].(map[string]any)
			if sec["type"] != "section" || sec["permission"] != "fleet.read" {
				t.Fatalf("permission not preserved: %v", sec)
			}
			if _, ok := out["data"].(map[string]any)["fleet"]; !ok {
				t.Fatalf("data source not merged: %v", out["data"])
			}
			if in["blocks"].([]any)[0].(map[string]any)["type"] != "ref" {
				t.Fatal("input modified")
			}
		})
	}
	plain := map[string]any{"version": "genui/v2", "blocks": []any{map[string]any{"type": "text", "text": "hola"}}}
	if out, err := ExpandDocBlockRefs(plain, nil); err != nil || out["blocks"].([]any)[0].(map[string]any)["text"] != "hola" {
		t.Fatalf("plain doc: %v %v", out, err)
	}
}
