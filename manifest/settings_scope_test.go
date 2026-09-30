package manifest

import "testing"

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
