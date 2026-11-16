package manifest

import (
	"strings"
	"testing"
)

func TestResolveSettingScopedEntityPrecedence(t *testing.T) {
	org := map[string]any{"k": "org"}
	ent := map[string]any{"k": "entity"}
	br := map[string]any{"k": "branch"}
	mk := map[string]any{"k": "market"}

	cases := []struct {
		name  string
		scope string
		want  any
	}{
		{"org key ignores every override", SettingScopeOrg, "org"},
		{"empty scope is org", "", "org"},
		{"entity key: entity over org", SettingScopeEntity, "entity"},
		{"branch key: branch over entity over org", SettingScopeBranch, "branch"},
		{"market key: market over org (entity not consulted)", SettingScopeMarket, "market"},
	}
	for _, c := range cases {
		d := SettingDef{Key: "k", Scope: c.scope, DefaultValue: "default"}
		if got := ResolveSettingScopedEntity(d, org, ent, br, mk); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}

	// A branch key with no branch override inherits the entity's.
	d := SettingDef{Key: "k", Scope: SettingScopeBranch, DefaultValue: "default"}
	if got := ResolveSettingScopedEntity(d, org, ent, nil, nil); got != "entity" {
		t.Errorf("branch key must inherit the entity override, got %v", got)
	}
	// No entity values at all: exactly the pre-entity resolution.
	if got := ResolveSettingScopedEntity(d, org, nil, nil, nil); got != "org" {
		t.Errorf("nil entity layer must fall through to org, got %v", got)
	}
	// The old entry point is unchanged.
	if got := ResolveSettingScoped(SettingDef{Key: "k", Scope: SettingScopeBranch}, org, br, nil); got != "branch" {
		t.Errorf("ResolveSettingScoped changed: %v", got)
	}
	// A nil override never shadows a lower layer.
	if got := ResolveSettingScopedEntity(SettingDef{Key: "k", Scope: SettingScopeEntity}, org, map[string]any{"k": nil}, nil, nil); got != "org" {
		t.Errorf("a nil entity value must not shadow the org value, got %v", got)
	}
}

func TestValidateEntityScope(t *testing.T) {
	m := &Manifest{Key: "acme", Name: "Acme", Version: "1.0.0",
		Settings: []SettingDef{{Key: "k", Type: "string", Scope: "entity"}}}
	if err := m.Validate("1.0.0"); err != nil && strings.Contains(err.Error(), "scope") {
		t.Fatalf("entity scope rejected: %v", err)
	}
}
