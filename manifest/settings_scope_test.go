package manifest

import (
	"encoding/json"
	"strings"
	"testing"

	v3 "github.com/asteby/metacore-kernel/manifest/v3"
)

func TestResolveSetting(t *testing.T) {
	branch := SettingDef{Key: "stock_policy", Scope: SettingScopeBranch, DefaultValue: "block"}
	org := SettingDef{Key: "round_totals", DefaultValue: false}
	cases := []struct {
		name      string
		def       SettingDef
		orgV, brV map[string]any
		want      any
	}{
		{"branch override wins", branch, map[string]any{"stock_policy": "warn"}, map[string]any{"stock_policy": "approval"}, "approval"},
		{"no override falls to org", branch, map[string]any{"stock_policy": "warn"}, map[string]any{}, "warn"},
		{"nil override does not shadow", branch, map[string]any{"stock_policy": "warn"}, map[string]any{"stock_policy": nil}, "warn"},
		{"nothing falls to default", branch, nil, nil, "block"},
		{"org-scoped ignores branch values", org, map[string]any{"round_totals": true}, map[string]any{"round_totals": false}, true},
		{"org-scoped default", org, nil, map[string]any{"round_totals": true}, false},
	}
	for _, c := range cases {
		if got := ResolveSetting(c.def, c.orgV, c.brV); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

func TestReasonRequiredDef(t *testing.T) {
	var nilPolicy *ReasonRequiredDef
	if nilPolicy.RequiresAction("cancel") || nilPolicy.Min() != DefaultReasonMinLength {
		t.Fatal("nil policy must require nothing")
	}
	p := &ReasonRequiredDef{Actions: []string{"cancel"}, MinLength: 10}
	if !p.RequiresAction("cancel") || p.RequiresAction("void") || p.Min() != 10 {
		t.Fatalf("policy misread: %+v", p)
	}
}

func TestResolveSettingScopedPrecedence(t *testing.T) {
	market := SettingDef{Key: "ship_free_over", Scope: SettingScopeMarket, DefaultValue: "def"}
	branch := SettingDef{Key: "stock_policy", Scope: SettingScopeBranch, DefaultValue: "def"}
	org := SettingDef{Key: "round", DefaultValue: "def"}
	o := func(k string, v any) map[string]any { return map[string]any{k: v} }
	cases := []struct {
		name string
		def  SettingDef
		org  map[string]any
		br   map[string]any
		mk   map[string]any
		want any
	}{
		{"market wins", market, o("ship_free_over", "org"), o("ship_free_over", "br"), o("ship_free_over", "mk"), "mk"},
		{"market falls to org (branch layer ignored)", market, o("ship_free_over", "org"), o("ship_free_over", "br"), nil, "org"},
		{"market nil value does not shadow", market, o("ship_free_over", "org"), nil, o("ship_free_over", nil), "org"},
		{"market falls to default", market, nil, nil, nil, "def"},
		{"branch scope ignores market layer", branch, o("stock_policy", "org"), o("stock_policy", "br"), o("stock_policy", "mk"), "br"},
		{"branch scope without branch uses org", branch, o("stock_policy", "org"), nil, o("stock_policy", "mk"), "org"},
		{"org scope ignores both", org, o("round", "org"), o("round", "br"), o("round", "mk"), "org"},
		{"org scope default", org, nil, o("round", "br"), o("round", "mk"), "def"},
	}
	for _, c := range cases {
		if got := ResolveSettingScoped(c.def, c.org, c.br, c.mk); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

// Without a market ResolveSettingScoped is exactly ResolveSetting.
func TestResolveSettingScopedNoMarketEqualsLegacy(t *testing.T) {
	defs := []SettingDef{
		{Key: "a", Scope: SettingScopeBranch, DefaultValue: 1},
		{Key: "a", DefaultValue: 1},
		{Key: "a", Scope: SettingScopeMarket, DefaultValue: 1},
	}
	vals := []map[string]any{nil, {}, {"a": 2}, {"a": nil}}
	for _, d := range defs {
		for _, ov := range vals {
			for _, bv := range vals {
				if got, want := ResolveSettingScoped(d, ov, bv, nil), ResolveSetting(d, ov, bv); got != want {
					t.Errorf("def=%+v org=%v br=%v: scoped=%v legacy=%v", d, ov, bv, got, want)
				}
			}
		}
	}
}

func TestMapSettingsProjectsDescriptionRefAndOptionsSource(t *testing.T) {
	got := mapSettings([]v3.Setting{
		{Key: "wh", Type: "dynamic_select", Label: "l", Description: "settings.wh.help", Ref: "Warehouse", Scope: "market"},
		{Key: "src", Type: "dynamic_select", OptionsSource: "list_series"},
		{Key: "plain", Type: "string"},
	})
	if got[0].Description != "settings.wh.help" || got[0].Ref != "Warehouse" || got[0].Scope != SettingScopeMarket {
		t.Fatalf("ref setting not projected: %+v", got[0])
	}
	if got[1].OptionsSource != "list_series" || got[1].Ref != "" {
		t.Fatalf("options_source not projected: %+v", got[1])
	}
	if p := got[2]; p.Description != "" || p.Ref != "" || p.OptionsSource != "" {
		t.Fatalf("plain setting grew fields: %+v", p)
	}
	// Old payloads (no new fields) serialize without them.
	b, _ := json.Marshal(got[2])
	if string(b) != `{"key":"plain","label":"","type":"string"}` {
		t.Fatalf("legacy JSON shape changed: %s", b)
	}
}

func TestValidateMarketScope(t *testing.T) {
	m := &Manifest{Key: "acme", Name: "Acme", Version: "1.0.0",
		Settings: []SettingDef{{Key: "k", Type: "string", Scope: "market"}}}
	if err := m.Validate("1.0.0"); err != nil && strings.Contains(err.Error(), "scope") {
		t.Fatalf("market scope rejected: %v", err)
	}
	m.Settings[0].Scope = "galaxy"
	if err := m.Validate("1.0.0"); err == nil || !strings.Contains(err.Error(), "org|branch|market") {
		t.Fatalf("unknown scope must be rejected, got %v", err)
	}
}
