package apiroute

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	v3 "github.com/asteby/metacore-kernel/manifest/v3"
)

func rt(key, method, path string) Route {
	return Route{Key: key, Method: method, Path: path, Auth: AuthToken, Do: "wasm:" + key}
}

func mustRegister(t *testing.T, r *Router, addon string, routes ...Route) {
	t.Helper()
	if err := r.Register(addon, routes); err != nil {
		t.Fatalf("register %s: %v", addon, err)
	}
}

func TestRouter_Precedence(t *testing.T) {
	r := NewRouter()
	mustRegister(t, r, "shop",
		rt("product", "GET", "/products/:handle"),
		rt("featured", "GET", "/products/featured"),
		rt("product_recs", "GET", "/products/:handle/recommendations"),
		rt("product_any", "GET", "/products/*rest"),
		rt("files_root", "GET", "/files"),
		rt("files", "GET", "/files/*"),
		rt("mixed_param", "GET", "/m/:a/x"),
		rt("mixed_wild", "GET", "/m/*rest"),
		rt("root", "GET", "/"),
	)
	cases := []struct {
		path   string
		key    string
		params map[string]string
	}{
		{"/products/featured", "featured", map[string]string{}},
		{"/products/llanta-205", "product", map[string]string{"handle": "llanta-205"}},
		{"/products/llanta-205/", "product", map[string]string{"handle": "llanta-205"}},
		{"/products/a/recommendations", "product_recs", map[string]string{"handle": "a"}},
		{"/products/a/b/c", "product_any", map[string]string{"rest": "a/b/c"}},
		{"/files", "files_root", map[string]string{}},
		{"/files/x/y", "files", map[string]string{"*": "x/y"}},
		{"/m/q/x", "mixed_param", map[string]string{"a": "q"}},
		{"/m/q/y", "mixed_wild", map[string]string{"rest": "q/y"}},
		{"/products/caf%C3%A9", "product", map[string]string{"handle": "café"}},
		{"/", "root", map[string]string{}},
	}
	for _, tc := range cases {
		got, params, ok := r.Match("GET", tc.path)
		if !ok {
			t.Fatalf("%s: no match", tc.path)
		}
		if got.Key != tc.key || got.Addon != "shop" {
			t.Fatalf("%s: matched %s.%s, want %s", tc.path, got.Addon, got.Key, tc.key)
		}
		if !reflect.DeepEqual(params, tc.params) {
			t.Fatalf("%s: params %v, want %v", tc.path, params, tc.params)
		}
	}
	// HEAD resolves to GET routes; methods are case-insensitive.
	if got, _, ok := r.Match("head", "/products/featured"); !ok || got.Key != "featured" {
		t.Fatalf("HEAD did not match GET route: %+v %v", got, ok)
	}
	if _, _, ok := r.Match("POST", "/products/featured"); ok {
		t.Fatal("POST must not match a GET route")
	}
	if _, _, ok := r.Match("GET", "/nope"); ok {
		t.Fatal("unexpected match")
	}
	if _, _, ok := r.Match("GET", "/products/%zz"); ok {
		t.Fatal("bad escape must not match")
	}
}

func TestRouter_MatchIsDeterministic(t *testing.T) {
	r := NewRouter()
	mustRegister(t, r, "a", rt("by_id", "GET", "/x/:id"))
	mustRegister(t, r, "b", rt("static", "GET", "/x/new"), rt("wild", "GET", "/x/*rest"))
	for i := 0; i < 50; i++ {
		if got, _, _ := r.Match("GET", "/x/new"); got.Key != "static" {
			t.Fatalf("iteration %d: got %s", i, got.Key)
		}
		if got, _, _ := r.Match("GET", "/x/42"); got.Key != "by_id" {
			t.Fatalf("iteration %d: got %s", i, got.Key)
		}
	}
}

func TestRouter_ConflictsAcrossAddons(t *testing.T) {
	r := NewRouter()
	mustRegister(t, r, "a", rt("p", "GET", "/products/:handle"))
	err := r.Register("b", []Route{rt("ok", "GET", "/other"), rt("q", "GET", "/products/:slug")})
	if !errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), `addon "a"`) {
		t.Fatalf("expected conflict with addon a, got %v", err)
	}
	// Atomic: b's non-conflicting route was not registered either.
	if _, _, ok := r.Match("GET", "/other"); ok {
		t.Fatal("failed Register must not apply any route")
	}
	// Other method on the same shape is fine.
	mustRegister(t, r, "b", rt("put", "PUT", "/products/:slug"))
	// Unregister frees the shape.
	r.Unregister("a")
	mustRegister(t, r, "c", rt("p", "GET", "/products/:id"))
	if got, _, _ := r.Match("GET", "/products/x"); got.Addon != "c" {
		t.Fatalf("expected c to own the shape, got %s", got.Addon)
	}
}

func TestRouter_ReRegisterReplacesSet(t *testing.T) {
	r := NewRouter()
	mustRegister(t, r, "a", rt("one", "GET", "/one"), rt("two", "GET", "/two"))
	// Same addon re-registering its own shapes is not a conflict.
	mustRegister(t, r, "a", rt("one", "GET", "/one"))
	if _, _, ok := r.Match("GET", "/two"); ok {
		t.Fatal("re-register must drop routes absent from the new set")
	}
	if n := len(r.Routes()); n != 1 {
		t.Fatalf("routes = %d", n)
	}
	mustRegister(t, r, "a")
	if n := len(r.Routes()); n != 0 {
		t.Fatalf("empty register must clear, got %d", n)
	}
}

func TestRouter_RejectsInvalidAndDuplicates(t *testing.T) {
	r := NewRouter()
	cases := []struct {
		name   string
		routes []Route
		want   error
	}{
		{"bad path", []Route{rt("x", "GET", "x")}, ErrInvalidRoute},
		{"bad method", []Route{rt("x", "TRACE", "/x")}, ErrInvalidRoute},
		{"bad auth", []Route{{Key: "x", Method: "GET", Path: "/x", Auth: "public", Do: "wasm:x"}}, ErrInvalidRoute},
		{"cache on post", []Route{{Key: "x", Method: "POST", Path: "/x", Auth: AuthToken, Cache: &Cache{MaxAge: 1}, Do: "wasm:x"}}, ErrInvalidRoute},
		{"dup key", []Route{rt("x", "GET", "/x"), rt("x", "GET", "/y")}, ErrInvalidRoute},
		{"same shape in set", []Route{rt("x", "GET", "/p/:a"), rt("y", "GET", "/p/:b")}, ErrConflict},
	}
	for _, tc := range cases {
		if err := r.Register("a", tc.routes); !errors.Is(err, tc.want) {
			t.Fatalf("%s: got %v, want %v", tc.name, err, tc.want)
		}
	}
	if err := r.Register("", []Route{rt("x", "GET", "/x")}); !errors.Is(err, ErrInvalidRoute) {
		t.Fatalf("empty addon key: %v", err)
	}
}

func TestRouter_Allowed(t *testing.T) {
	r := NewRouter()
	mustRegister(t, r, "a", rt("get", "GET", "/carts/:id"), rt("del", "DELETE", "/carts/:id"))
	if got := r.Allowed("/carts/1"); !reflect.DeepEqual(got, []string{"DELETE", "GET", "HEAD"}) {
		t.Fatalf("allowed = %v", got)
	}
	if got := r.Allowed("/nope"); len(got) != 0 {
		t.Fatalf("allowed = %v", got)
	}
}

func TestRoutesFromManifest(t *testing.T) {
	in := []v3.APIRoute{{Key: "p", Method: "GET", Path: "/p/:h", Auth: "token", Scopes: []string{"catalog:read"},
		Cache: &v3.APICache{MaxAge: 60, SWR: 600}, Do: "wasm:p"}}
	out := RoutesFromManifest(in)
	if len(out) != 1 || out[0].Auth != AuthToken || out[0].Cache == nil || out[0].Cache.SWR != 600 || out[0].Scopes[0] != "catalog:read" {
		t.Fatalf("conversion wrong: %+v", out)
	}
	in[0].Scopes[0] = "mutated"
	if out[0].Scopes[0] != "catalog:read" {
		t.Fatal("scopes must be copied")
	}
	if RoutesFromManifest(nil) != nil {
		t.Fatal("nil in, nil out")
	}
}

func TestRoute_Audience(t *testing.T) {
	in := []v3.APIRoute{
		{Key: "a", Method: "GET", Path: "/a", Auth: "token", Scopes: []string{"content:read"}, Do: "wasm:a"},
		{Key: "b", Method: "PUT", Path: "/b", Auth: "token", Scopes: []string{"content:write"}, Do: "wasm:b", Audience: "tooling"},
	}
	out := RoutesFromManifest(in)
	if out[0].Audience != "" || out[0].EffectiveAudience() != "store" || out[1].EffectiveAudience() != "tooling" {
		t.Fatalf("audiences: %+v", out)
	}
	if out[1].toManifest().Audience != "tooling" {
		t.Fatal("audience lost converting back")
	}
	r := NewRouter()
	if err := r.Register("x", out); err != nil {
		t.Fatalf("register: %v", err)
	}
	bad := out[0]
	bad.Audience = "public"
	if err := NewRouter().Register("y", []Route{bad}); err == nil {
		t.Fatal("unknown audience must be rejected at registration")
	}
}
