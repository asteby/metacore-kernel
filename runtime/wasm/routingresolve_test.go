package wasm

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/asteby/metacore-kernel/manifest"
	"github.com/asteby/metacore-kernel/routing"
	"github.com/google/uuid"
)

func TestExecuteRoutingResolve_WinnerAndIsSelf(t *testing.T) {
	org := uuid.New()
	table := routing.Build([]routing.Contribution{
		{AddonKey: "inventory", Routes: []manifest.RouteDef{
			{Domain: "order_fulfillment", Handler: "stock_direct"},
		}},
		{AddonKey: "warehouse", Routes: []manifest.RouteDef{
			{
				Domain:   "order_fulfillment",
				Match:    map[string]string{"product_type": "storable"},
				Handler:  "stock_allocate",
				Priority: 100,
			},
		}},
	}, func(string) bool { return true })

	inv := &invocation{
		addonKey: "inventory",
		orgID:    org,
		routingTable: func(ctx context.Context, orgID uuid.UUID) (*routing.Table, error) {
			return table, nil
		},
	}

	raw := executeRoutingResolve(context.Background(), inv, []byte(
		`{"domain":"order_fulfillment","attrs":{"product_type":"storable"}}`,
	))
	var env map[string]any
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	if env["success"] != true {
		t.Fatalf("envelope = %s", raw)
	}
	data := env["data"].(map[string]any)
	if data["resolved"] != true || data["handler"] != "stock_allocate" {
		t.Fatalf("data = %#v", data)
	}
	if data["addon_key"] != "warehouse" || data["is_self"] != false {
		t.Fatalf("expected warehouse winner, is_self=false; got %#v", data)
	}
}

func TestExecuteRoutingResolve_Unavailable(t *testing.T) {
	inv := &invocation{addonKey: "inventory", orgID: uuid.New()}
	raw := executeRoutingResolve(context.Background(), inv, []byte(`{"domain":"order_fulfillment"}`))
	var env map[string]any
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	if env["success"] != false {
		t.Fatalf("want failure, got %s", raw)
	}
	errObj := env["error"].(map[string]any)
	if errObj["code"] != "routing_unavailable" {
		t.Fatalf("code = %v", errObj["code"])
	}
}
