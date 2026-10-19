package preset

// hardening_test.go — r1's findings on #508: overlay role escalation, a
// bounded chain resolver, SVG validation, ranges and keyed-merge edge cases.

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	v3 "github.com/asteby/metacore-kernel/manifest/v3"
)

func baseWithRoles(t *testing.T, roles string) Resolved {
	t.Helper()
	b := pitslineBase(t)
	b.Defaults["roles"] = decode(t, `{"r": `+roles+`}`)["r"]
	return b
}

func TestFlatten_OverlayRoleEscalationIsRejected(t *testing.T) {
	base := baseWithRoles(t, `[
		{"key": "owner", "name": "Owner", "permissions": ["*"]},
		{"key": "agent", "name": "Agent", "permissions": ["inbox.read"]},
		{"name": "cajero", "permissions": ["pos.session.open"]},
		{"name": "auditor", "is_system": true}
	]`)
	cases := map[string]string{
		"wildcard":                 `[{"name": "cajero", "permissions": ["*"]}]`,
		"prefix wildcard":          `[{"key": "agent", "permissions": ["users.*"]}]`,
		"privileged field":         `[{"name": "cajero", "is_admin": true}]`,
		"unknown field":            `[{"name": "cajero", "grants_all": true}]`,
		"remove owner":             `[{"key": "owner", "_remove": true}]`,
		"remove flagged system":    `[{"name": "auditor", "_remove": true}]`,
		"re-permission owner":      `[{"key": "owner", "permissions": ["-users.user.manage"]}]`,
		"new role named admin":     `[{"key": "admin", "permissions": ["pos.sale.read"]}]`,
		"unknown permission (cat)": `[{"name": "cajero", "permissions": ["users.user.manage"]}]`,
	}
	known := func(p string) bool { return p == "pos.sale.read" || p == "pos.session.open" }
	for name, roles := range cases {
		t.Run(name, func(t *testing.T) {
			ov := overlayManifest(t, nil, nil, `{"roles": `+roles+`}`)
			_, _, err := FlattenWithOptions(base, ov, nil, FlattenOptions{KnownPermission: known})
			if !errors.Is(err, ErrRoleEscalation) {
				t.Fatalf("want ErrRoleEscalation, got %v", err)
			}
		})
	}
	// Allowed: a declared permission, a revocation, cosmetic fields on a
	// system role, removing an ordinary role.
	ok := overlayManifest(t, nil, nil, `{"roles": [
		{"name": "cajero", "permissions": ["pos.sale.read", "-pos.session.open"], "icon": "Wallet"},
		{"key": "owner", "label": "Dueño"},
		{"key": "agent", "_remove": true}
	]}`)
	got, _, err := FlattenWithOptions(base, ok, nil, FlattenOptions{KnownPermission: known})
	if err != nil {
		t.Fatalf("allowed overlay: %v", err)
	}
	roles := got.Defaults["roles"].([]interface{})
	if len(roles) != 3 {
		t.Fatalf("roles = %v", roles)
	}
	// Without a catalog only the structural checks run.
	if _, err := Flatten(base, overlayManifest(t, nil, nil, `{"roles": [{"name": "cajero", "permissions": ["users.user.manage"]}]}`)); err != nil {
		t.Fatalf("no catalog: %v", err)
	}
}

func TestDeclaredPermissions(t *testing.T) {
	m := &v3.Manifest{RBAC: &v3.RBAC{
		Permissions: []v3.PermissionDef{{Key: "pos.sale.read"}},
		Roles:       []v3.Role{{Key: "admin", Permissions: []string{"*", "pos.sale.write"}}},
	}}
	known := DeclaredPermissions(m, nil)
	if !known("pos.sale.read") || !known("pos.sale.write") || known("*") || known("users.user.manage") {
		t.Fatal("DeclaredPermissions catalog is wrong")
	}
}

func TestMergeKeyed_IdentityRemoveAndRevocations(t *testing.T) {
	st := MergeStrategies["roles"]
	// link-style roles (key + display name): the overlay matches on key.
	base := decode(t, `{"r": [{"key": "agent", "name": "Agent", "permissions": ["inbox.read"]}]}`)["r"].([]interface{})
	got := mergeKeyed(base, decode(t, `{"r": [{"key": "agent", "permissions": ["inbox.write"]}]}`)["r"].([]interface{}), st)
	if len(got) != 1 || !reflect.DeepEqual(got[0].(map[string]interface{})["permissions"], []interface{}{"inbox.read", "inbox.write"}) {
		t.Fatalf("key identity: %v", got)
	}
	// A new item never keeps a literal "-perm".
	got = mergeKeyed(base, decode(t, `{"r": [{"key": "nuevo", "permissions": ["a", "-b"]}]}`)["r"].([]interface{}), st)
	if !reflect.DeepEqual(got[1].(map[string]interface{})["permissions"], []interface{}{"a"}) {
		t.Fatalf("new item permissions: %v", got[1])
	}
	// _remove then the same identity: a fresh item, not the base one revived.
	got = mergeKeyed(base, decode(t, `{"r": [{"key": "agent", "_remove": true}, {"key": "agent", "label": "Nuevo"}]}`)["r"].([]interface{}), st)
	if len(got) != 1 || got[0].(map[string]interface{})["permissions"] != nil || got[0].(map[string]interface{})["label"] != "Nuevo" {
		t.Fatalf("remove + re-add: %v", got)
	}
	// 1 and "1" are different identities.
	ids := Strategy{Kind: MergeKeyed, KeyFields: []string{"code"}}
	got = mergeKeyed(decode(t, `{"r": [{"code": 1, "v": "num"}]}`)["r"].([]interface{}), decode(t, `{"r": [{"code": "1", "v": "str"}]}`)["r"].([]interface{}), ids)
	if len(got) != 2 {
		t.Fatalf("1 and \"1\" collided: %v", got)
	}
}

func TestCombineRanges_UnsatisfiableAndUnbounded(t *testing.T) {
	if _, err := combineRanges(">=2.0.0", "<1.0.0"); err == nil {
		t.Fatal("disjoint ranges must fail")
	}
	if _, err := combineRanges("^0.4.0", "^0.5.0"); err == nil {
		t.Fatal("^0.4 and ^0.5 are disjoint")
	}
	if r, err := combineRanges(">=0.4.0 <1.0.0", "^0.6.0"); err != nil || r == "" {
		t.Fatalf("overlapping ranges: %q %v", r, err)
	}
	for _, r := range []string{"*", ">=1.0.0", ">0.9"} {
		if !Unbounded(r) {
			t.Errorf("%q has no upper bound", r)
		}
	}
	if Unbounded(">=0.9.1 <1.0.0") {
		t.Error("bounded range reported as unbounded")
	}
	ov := overlayManifest(t, nil, nil, `{"branding": {"platform_name": "X"}, "branding_file": "./defaults/branding.json"}`)
	ov.Preset.Extends.Version = ">=0.9.0"
	w := strings.Join(DefaultsWarnings(ov), "\n")
	if !strings.Contains(w, "no upper bound") || !strings.Contains(w, "branding is inline and also in branding_file") {
		t.Fatalf("warnings = %s", w)
	}
}

func chainPreset(key, version string, ext *v3.PresetExtends) *v3.Manifest {
	p := &v3.Preset{Extends: ext, Defaults: map[string]interface{}{"branding": map[string]interface{}{"platform_name": key}}}
	if ext == nil {
		p.Addons = []v3.PresetAddon{{Key: "pos", Version: "^0.42.0"}}
	}
	return &v3.Manifest{Kind: v3.KindPreset, Metadata: v3.Metadata{Key: key, Version: version}, Preset: p}
}

func TestResolveChain(t *testing.T) {
	pub := chainPreset("pitsline", "0.9.3", nil)
	mid := chainPreset("retail", "1.0.0", &v3.PresetExtends{Key: "pitsline", Version: "^0.9.0"})
	top := chainPreset("sieteleguas", "1.0.0", &v3.PresetExtends{Key: "retail", Version: "^1.0.0"})
	catalog := map[string]*v3.Manifest{"pitsline": pub, "retail": mid}
	calls := 0
	fetch := func(ext v3.PresetExtends) (*v3.Manifest, map[string][]byte, error) {
		calls++
		if calls > 10 {
			t.Fatal("ResolveChain is looping")
		}
		return catalog[ext.Key], map[string][]byte{}, nil
	}
	r, _, err := ResolveChain(top, nil, fetch, FlattenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if r.Key != "sieteleguas" || len(r.Lineage) != 3 || r.Defaults["branding"].(map[string]interface{})["platform_name"] != "sieteleguas" {
		t.Fatalf("resolved = %+v", r)
	}

	// A→B→A: fails, bounded.
	a := chainPreset("a", "1.0.0", &v3.PresetExtends{Key: "b", Version: "^1.0.0"})
	b := chainPreset("b", "1.0.0", &v3.PresetExtends{Key: "a", Version: "^1.0.0"})
	catalog = map[string]*v3.Manifest{"a": a, "b": b}
	calls = 0
	if _, _, err := ResolveChain(a, nil, fetch, FlattenOptions{}); !errors.Is(err, ErrCyclicExtends) {
		t.Fatalf("cycle: %v", err)
	}
	// Too deep: 4 presets.
	d := chainPreset("d", "1.0.0", &v3.PresetExtends{Key: "c", Version: "^1.0.0"})
	c := chainPreset("c", "1.0.0", &v3.PresetExtends{Key: "b2", Version: "^1.0.0"})
	b2 := chainPreset("b2", "1.0.0", &v3.PresetExtends{Key: "a2", Version: "^1.0.0"})
	catalog = map[string]*v3.Manifest{"c": c, "b2": b2, "a2": chainPreset("a2", "1.0.0", nil)}
	calls = 0
	if _, _, err := ResolveChain(d, nil, fetch, FlattenOptions{}); !errors.Is(err, ErrLineageTooDeep) {
		t.Fatalf("depth: %v", err)
	}
	// The host returned the wrong preset or version.
	catalog = map[string]*v3.Manifest{"retail": chainPreset("retail", "2.0.0", &v3.PresetExtends{Key: "pitsline", Version: "^0.9.0"})}
	if _, _, err := ResolveChain(top, nil, fetch, FlattenOptions{}); !errors.Is(err, ErrBaseMismatch) {
		t.Fatalf("mismatch: %v", err)
	}
	if _, _, err := ResolveChain(top, nil, nil, FlattenOptions{}); !errors.Is(err, ErrNeedsBase) {
		t.Fatalf("no fetch: %v", err)
	}
}

func TestFlattenWithOptions_RejectsBadFileKeys(t *testing.T) {
	base := pitslineBase(t)
	for _, k := range []string{"assets/../x", "assets/%2e%2e/x", `assets/a\b.svg`, "frontend/x.js", "assets/x/"} {
		if _, _, err := FlattenWithOptions(base, overlayManifest(t, nil, nil, ""), map[string][]byte{k: []byte("x")}, FlattenOptions{}); err == nil {
			t.Errorf("%q must be rejected", k)
		}
	}
}

func TestValidateSVG(t *testing.T) {
	if err := ValidateSVG([]byte(`<svg xmlns="http://www.w3.org/2000/svg"><path d="M0 0" fill="#f00"/></svg>`)); err != nil {
		t.Fatalf("plain svg: %v", err)
	}
	for _, bad := range []string{
		`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`,
		`<svg xmlns="http://www.w3.org/2000/svg" onload="alert(1)"/>`,
		`<svg xmlns="http://www.w3.org/2000/svg"><a href="javascript:alert(1)"><path d="M0 0"/></a></svg>`,
		`<svg xmlns="http://www.w3.org/2000/svg"><use href="https://evil/x.svg#a"/></svg>`,
		`<!DOCTYPE svg [<!ENTITY x "y">]><svg xmlns="http://www.w3.org/2000/svg"/>`,
		`<svg xmlns="http://www.w3.org/2000/svg"><foreignObject><div/></foreignObject></svg>`,
	} {
		if ValidateSVG([]byte(bad)) == nil {
			t.Errorf("accepted: %s", bad)
		}
	}
}

func TestUncleanAssetRefs(t *testing.T) {
	got := UncleanAssetRefs(map[string]interface{}{"k": map[string]interface{}{
		"a": "./assets/ok.svg", "b": "./assets/../x", "c": "./assets/%2e%2e/x", "d": `./assets/a\b`, "e": "./assets/dir/",
	}})
	want := []string{"./assets/%2e%2e/x", "./assets/../x", "./assets/a\\b", "./assets/dir/"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("UncleanAssetRefs = %q", got)
	}
}
