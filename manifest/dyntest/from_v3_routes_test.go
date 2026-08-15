package dyntest

import (
	"strings"
	"testing"

	"github.com/asteby/metacore-kernel/manifest"
	v3 "github.com/asteby/metacore-kernel/manifest/v3"
	"github.com/asteby/metacore-kernel/routing"
)

// routesManifestJSON is the workshop side of the fulfillment table: it claims
// service lines, gated on its own installation.
const routesManifestJSON = `{
  "apiVersion": "asteby.com/v3",
  "kind": "Addon",
  "metadata": { "key": "workshop", "name": "Workshop", "version": "1.0.0" },
  "compatibility": { "requires": [ { "key": "kernel", "version": ">=0.1.0" } ] },
  "models": [
    {
      "key": "WorkOrder",
      "table": "work_orders",
      "label": "Work orders",
      "columns": [
        { "name": "id", "type": "uuid", "primary_key": true },
        { "name": "organization_id", "type": "uuid", "not_null": true },
        { "name": "code", "type": "text" }
      ]
    }
  ],
  "contributions": {
    "routes": [
      {
        "domain": "fulfillment",
        "match": { "product_type": "service" },
        "handler": "service_workorder",
        "priority": 100,
        "condition": { "addon_installed": "workshop" }
      }
    ]
  }
}`

// TestRoutesParseAndProject walks contributions.routes[] through the kernel
// chain: v3.Parse → FromV3 → legacy Validate → routing.Table.
func TestRoutesParseAndProject(t *testing.T) {
	m, err := v3.Parse([]byte(routesManifestJSON))
	if err != nil {
		t.Fatalf("v3.Parse rejected manifest with contributions.routes: %v", err)
	}
	r := m.Contributions.Routes[0]
	if r.Domain != "fulfillment" || r.Handler != "service_workorder" || r.Priority != 100 {
		t.Errorf("v3 route decoded as %+v", r)
	}
	if r.Match["product_type"] != "service" {
		t.Errorf("v3 route match = %v", r.Match)
	}
	if r.Condition == nil || r.Condition.AddonInstalled != "workshop" {
		t.Errorf("v3 route condition = %+v", r.Condition)
	}

	host := manifest.FromV3(m)
	if len(host.Routes) != 1 {
		t.Fatalf("host routes = %+v", host.Routes)
	}
	hr := host.Routes[0]
	if hr.Domain != "fulfillment" || hr.Handler != "service_workorder" || hr.Priority != 100 ||
		hr.Match["product_type"] != "service" || hr.Condition == nil || hr.Condition.AddonInstalled != "workshop" {
		t.Errorf("host route = %+v", hr)
	}
	if err := host.Validate("2.0.0"); err != nil {
		t.Fatalf("legacy Validate rejected converted manifest: %v", err)
	}

	// The projected route resolves through the routing table for an org that
	// has workshop, and is absent for one that does not.
	contribs := []routing.Contribution{{AddonKey: host.Key, Routes: host.Routes}}
	attrs := map[string]string{"product_type": "service"}
	got, addon, ok := routing.Build(contribs, func(k string) bool { return k == "workshop" }).Resolve("fulfillment", attrs)
	if !ok || got != "service_workorder" || addon != "workshop" {
		t.Errorf("resolved (%q, %q, %v), want service_workorder from workshop", got, addon, ok)
	}
	if _, _, ok := routing.Build(contribs, func(string) bool { return false }).Resolve("fulfillment", attrs); ok {
		t.Error("workshop route resolved for an org without workshop installed")
	}
}

// TestRouteRejectedWhenAmbiguous asserts two routes with the same match at the
// same priority are rejected: the winner would depend on declaration order.
func TestRouteRejectedWhenAmbiguous(t *testing.T) {
	dup := `,
      {
        "domain": "fulfillment",
        "match": { "product_type": "service" },
        "handler": "other_handler",
        "priority": 100
      }`
	bad := strings.Replace(routesManifestJSON, `"condition": { "addon_installed": "workshop" }
      }`, `"condition": { "addon_installed": "workshop" }
      }`+dup, 1)
	if _, err := v3.Parse([]byte(bad)); err == nil {
		t.Fatal("v3.Parse accepted two routes with the same match and priority")
	}
}

// TestRouteRejectedWhenHandlerMissing asserts the handler is required.
func TestRouteRejectedWhenHandlerMissing(t *testing.T) {
	bad := strings.Replace(routesManifestJSON, `"handler": "service_workorder",`, "", 1)
	if _, err := v3.Parse([]byte(bad)); err == nil {
		t.Fatal("v3.Parse accepted a route with no handler")
	}
}
