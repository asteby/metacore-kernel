package manifest_test

import (
	"testing"

	"github.com/asteby/metacore-kernel/manifest"
	v3 "github.com/asteby/metacore-kernel/manifest/v3"
)

// apiRoutesManifestJSON declares two contributions.api[] routes: a cacheable
// GET read and a POST write. It exercises parse → validate → FromV3 carrying
// the routes onto the host Manifest.APIRoutes.
const apiRoutesManifestJSON = `{
  "apiVersion": "asteby.com/v3",
  "kind": "Addon",
  "metadata": { "key": "storefront_api", "name": "Storefront API", "version": "1.0.0" },
  "compatibility": { "requires": [{ "key": "kernel", "version": ">=3.0.0 <4.0.0" }] },
  "contributions": {
    "api": [
      { "key": "product_get", "method": "GET", "path": "/storefront/products/:handle",
        "auth": "token", "scopes": ["catalog:read"],
        "cache": { "max_age": 60, "swr": 600 }, "do": "wasm:product_get" },
      { "key": "cart_create", "method": "POST", "path": "/storefront/carts",
        "auth": "token", "scopes": ["cart:write"], "do": "wasm:cart_create" }
    ]
  }
}`

func TestFromV3_CarriesAPIRoutesToHostManifest(t *testing.T) {
	if err := v3.Validate([]byte(apiRoutesManifestJSON)); err != nil {
		t.Fatalf("v3.Validate: %v", err)
	}
	m, err := v3.Parse([]byte(apiRoutesManifestJSON))
	if err != nil {
		t.Fatalf("v3.Parse: %v", err)
	}
	host := manifest.FromV3(m)
	if len(host.APIRoutes) != 2 {
		t.Fatalf("expected 2 host api routes, got %d", len(host.APIRoutes))
	}
	r := host.APIRoutes[0]
	if r.Key != "product_get" || r.Method != "GET" || r.Path != "/storefront/products/:handle" ||
		r.Auth != "token" || r.Cache == nil || r.Cache.MaxAge != 60 || r.Do != "wasm:product_get" {
		t.Fatalf("projection wrong: %+v", r)
	}
}

func TestFromV3_NoAPIRoutesStaysNil(t *testing.T) {
	m, err := v3.Parse([]byte(publicRoutesManifestJSON))
	if err != nil {
		t.Fatalf("v3.Parse: %v", err)
	}
	if host := manifest.FromV3(m); host.APIRoutes != nil {
		t.Fatalf("expected nil APIRoutes, got %+v", host.APIRoutes)
	}
}
