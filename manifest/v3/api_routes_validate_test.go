package v3

import (
	"strings"
	"testing"
)

// withAPIRoutes returns a baseValid manifest carrying the supplied
// contributions.api[] entries.
func withAPIRoutes(routes ...interface{}) map[string]interface{} {
	m := baseValid()
	m["contributions"] = map[string]interface{}{"api": routes}
	return m
}

func apiRoute(overrides map[string]interface{}) map[string]interface{} {
	r := map[string]interface{}{
		"key":    "product_get",
		"method": "GET",
		"path":   "/storefront/products/:handle",
		"auth":   "token",
		"scopes": []interface{}{"catalog:read"},
		"cache":  map[string]interface{}{"max_age": 60, "swr": 600},
		"do":     "wasm:product_get",
	}
	for k, v := range overrides {
		if v == nil {
			delete(r, k)
			continue
		}
		r[k] = v
	}
	return r
}

func TestAPIRoutes_Valid(t *testing.T) {
	m := withAPIRoutes(
		apiRoute(nil),
		// Same path, other method.
		apiRoute(map[string]interface{}{"key": "product_put", "method": "PUT", "cache": nil, "scopes": []interface{}{"catalog:write"}}),
		// Static beats param: not a conflict.
		apiRoute(map[string]interface{}{"key": "product_featured", "path": "/storefront/products/featured"}),
		apiRoute(map[string]interface{}{"key": "cart_get", "path": "/storefront/carts/:id", "cache": map[string]interface{}{"private": true}}),
		apiRoute(map[string]interface{}{"key": "files", "path": "/files/*rest", "auth": "none", "scopes": nil, "cache": nil}),
		apiRoute(map[string]interface{}{"key": "any", "path": "/raw/*", "auth": "staff", "do": "compiled:raw"}),
		apiRoute(map[string]interface{}{"key": "root", "path": "/", "auth": "customer", "cache": nil}),
	)
	if err := Validate(mustJSON(t, m)); err != nil {
		t.Fatalf("expected valid, got: %v", err)
	}
}

func TestAPIRoutes_ParsedOntoContributions(t *testing.T) {
	m, _, err := ParseWithOptions(mustJSON(t, withAPIRoutes(apiRoute(nil))), Options{Strict: true})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(m.Contributions.API) != 1 {
		t.Fatalf("api not parsed: %+v", m.Contributions)
	}
	r := m.Contributions.API[0]
	if r.Cache == nil || r.Cache.MaxAge != 60 || r.Cache.SWR != 600 || r.Scopes[0] != "catalog:read" {
		t.Fatalf("route not parsed faithfully: %+v", r)
	}
}

func TestAPIRoutes_Rejections(t *testing.T) {
	cases := []struct {
		name   string
		routes []interface{}
		want   string
	}{
		{"missing auth", []interface{}{apiRoute(map[string]interface{}{"auth": nil})}, "auth"},
		{"bad method", []interface{}{apiRoute(map[string]interface{}{"method": "get"})}, "method"},
		{"head method", []interface{}{apiRoute(map[string]interface{}{"method": "HEAD"})}, "method"},
		{"bad auth", []interface{}{apiRoute(map[string]interface{}{"auth": "storefront"})}, "auth"},
		{"bad key", []interface{}{apiRoute(map[string]interface{}{"key": "ProductGet"})}, "key"},
		{"relative path", []interface{}{apiRoute(map[string]interface{}{"path": "products"})}, "path"},
		{"trailing slash", []interface{}{apiRoute(map[string]interface{}{"path": "/products/"})}, "trailing slash"},
		{"empty segment", []interface{}{apiRoute(map[string]interface{}{"path": "/a//b"})}, "empty segment"},
		{"bad param", []interface{}{apiRoute(map[string]interface{}{"path": "/p/:Handle"})}, "snake_case"},
		{"repeated param", []interface{}{apiRoute(map[string]interface{}{"path": "/p/:id/q/:id"})}, "repeated"},
		{"wildcard not last", []interface{}{apiRoute(map[string]interface{}{"path": "/p/*rest/x"})}, "last segment"},
		{"bad static", []interface{}{apiRoute(map[string]interface{}{"path": "/p/a b"})}, "characters"},
		{"bad scope", []interface{}{apiRoute(map[string]interface{}{"scopes": []interface{}{"catalog"}})}, "scope"},
		{"scopes on none", []interface{}{apiRoute(map[string]interface{}{"auth": "none"})}, "not allowed when auth"},
		{"cache on post", []interface{}{apiRoute(map[string]interface{}{"method": "POST"})}, "only allowed on GET"},
		{"private with max_age", []interface{}{apiRoute(map[string]interface{}{"cache": map[string]interface{}{"private": true, "max_age": 5}})}, "private excludes"},
		{"negative max_age", []interface{}{apiRoute(map[string]interface{}{"cache": map[string]interface{}{"max_age": -1}})}, "max_age"},
		{"webhook do", []interface{}{apiRoute(map[string]interface{}{"do": "webhook:x"})}, "do"},
		{"bare do", []interface{}{apiRoute(map[string]interface{}{"do": "product_get"})}, "do"},
		{"duplicate key", []interface{}{apiRoute(nil), apiRoute(map[string]interface{}{"path": "/other"})}, "duplicated"},
		{"shape conflict", []interface{}{apiRoute(nil), apiRoute(map[string]interface{}{"key": "product_by_slug", "path": "/storefront/products/:slug"})}, "conflicts with route"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := Validate(mustJSON(t, withAPIRoutes(tc.routes...)))
			if err == nil {
				t.Fatalf("expected rejection containing %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

func TestParseAPIPath(t *testing.T) {
	segs, err := ParseAPIPath("/storefront/products/:handle/*rest")
	if err != nil {
		t.Fatal(err)
	}
	if len(segs) != 4 || segs[2].Kind != APISegmentParam || segs[2].Value != "handle" || segs[3].Kind != APISegmentWildcard || segs[3].Value != "rest" {
		t.Fatalf("unexpected segments: %+v", segs)
	}
	if got := APIPathShape(segs); got != "/storefront/products/:/*" {
		t.Fatalf("shape = %q", got)
	}
	if segs, err := ParseAPIPath("/x/*"); err != nil || segs[1].Value != "*" {
		t.Fatalf("bare wildcard: %+v %v", segs, err)
	}
	if segs, err := ParseAPIPath("/"); err != nil || len(segs) != 0 || APIPathShape(segs) != "/" {
		t.Fatalf("root: %+v %v", segs, err)
	}
}
