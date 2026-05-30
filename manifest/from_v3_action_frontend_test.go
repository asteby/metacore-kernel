package manifest_test

import (
	"testing"

	"github.com/asteby/metacore-kernel/manifest"
	v3 "github.com/asteby/metacore-kernel/manifest/v3"
)

// richManifestJSON exercises the additive v3 surface: an action carrying a
// declarative form (fields[] + confirm), an action delegating to a custom
// federated modal, and a top-level frontend federation block.
const richManifestJSON = `{
  "apiVersion": "asteby.com/v3",
  "kind": "Addon",
  "metadata": { "key": "checkout", "name": "Checkout", "version": "1.2.0" },
  "compatibility": { "requires": [{ "key": "kernel", "version": ">=3.0.0 <4.0.0" }] },
  "frontend": {
    "entry": "https://cdn.asteby.com/checkout/remoteEntry.js",
    "format": "federation",
    "expose": "./plugin",
    "container": "metacore_checkout",
    "layout": "immersive"
  },
  "contributions": {
    "actions": [
      {
        "key": "refund",
        "label": "Refund",
        "icon": "rotate-ccw",
        "target_model": "order",
        "handler": { "type": "wasm", "function": "RefundOrder" },
        "confirm": true,
        "confirm_message": "Refund this order? This cannot be undone.",
        "fields": [
          {
            "key": "amount",
            "label": "Amount",
            "type": "number",
            "required": true,
            "validation": { "min": 0 }
          },
          {
            "key": "reason",
            "label": "Reason",
            "type": "select",
            "options": [
              { "value": "fraud", "label": "Fraud" },
              { "value": "duplicate", "label": "Duplicate" }
            ],
            "default": "duplicate",
            "widget": "radio"
          }
        ]
      },
      {
        "key": "checkout",
        "label": "Open Checkout",
        "target_model": "order",
        "handler": { "type": "webhook", "url": "https://example.com/hook" },
        "modal": "checkout_panel",
        "placement": "create"
      }
    ]
  }
}`

func TestFromV3_ActionFieldsAndModal(t *testing.T) {
	m, err := v3.Parse([]byte(richManifestJSON))
	if err != nil {
		t.Fatalf("v3.Parse: %v", err)
	}

	out := manifest.FromV3(m)

	actions := out.Actions["order"]
	if len(actions) != 2 {
		t.Fatalf("expected 2 actions under model 'order', got %d", len(actions))
	}

	// Index by key so the test is order-independent.
	byKey := map[string]manifest.ActionDef{}
	for _, a := range actions {
		byKey[a.Key] = a
	}

	refund, ok := byKey["refund"]
	if !ok {
		t.Fatal("missing 'refund' action")
	}
	if refund.Label != "Refund" || refund.Icon != "rotate-ccw" {
		t.Errorf("refund label/icon = %q/%q, want Refund/rotate-ccw", refund.Label, refund.Icon)
	}
	if !refund.Confirm {
		t.Error("refund.Confirm = false, want true")
	}
	if refund.ConfirmMessage != "Refund this order? This cannot be undone." {
		t.Errorf("refund.ConfirmMessage = %q", refund.ConfirmMessage)
	}
	if refund.Trigger == nil || refund.Trigger.Type != "wasm" || refund.Trigger.Export != "RefundOrder" {
		t.Errorf("refund.Trigger = %+v, want wasm/RefundOrder", refund.Trigger)
	}
	if len(refund.Fields) != 2 {
		t.Fatalf("refund.Fields len = %d, want 2", len(refund.Fields))
	}

	amount := refund.Fields[0]
	if amount.Name != "amount" || amount.Label != "Amount" || amount.Type != "number" || !amount.Required {
		t.Errorf("amount field = %+v, want Name=amount Label=Amount Type=number Required=true", amount)
	}

	reason := refund.Fields[1]
	if reason.Name != "reason" || reason.Type != "select" {
		t.Errorf("reason field = %+v, want Name=reason Type=select", reason)
	}
	if reason.Default != "duplicate" {
		t.Errorf("reason.Default = %v, want duplicate", reason.Default)
	}
	if len(reason.Options) != 2 || reason.Options[0].Value != "fraud" || reason.Options[1].Label != "Duplicate" {
		t.Errorf("reason.Options = %+v, want [{fraud Fraud} {duplicate Duplicate}]", reason.Options)
	}

	checkout, ok := byKey["checkout"]
	if !ok {
		t.Fatal("missing 'checkout' action")
	}
	if checkout.Modal != "checkout_panel" {
		t.Errorf("checkout.Modal = %q, want checkout_panel", checkout.Modal)
	}
	if checkout.Placement != "create" {
		t.Errorf("checkout.Placement = %q, want create", checkout.Placement)
	}
	// 'refund' declared no placement → empty (host defaults to row).
	if refund.Placement != "" {
		t.Errorf("refund.Placement = %q, want empty (row default)", refund.Placement)
	}
	if checkout.Trigger == nil || checkout.Trigger.Type != "webhook" {
		t.Errorf("checkout.Trigger = %+v, want webhook", checkout.Trigger)
	}
	if len(checkout.Fields) != 0 {
		t.Errorf("checkout.Fields len = %d, want 0", len(checkout.Fields))
	}
}

func TestFromV3_Frontend(t *testing.T) {
	m, err := v3.Parse([]byte(richManifestJSON))
	if err != nil {
		t.Fatalf("v3.Parse: %v", err)
	}

	out := manifest.FromV3(m)

	if out.Frontend == nil {
		t.Fatal("out.Frontend is nil, want federation block")
	}
	fe := out.Frontend
	if fe.Entry != "https://cdn.asteby.com/checkout/remoteEntry.js" {
		t.Errorf("Frontend.Entry = %q", fe.Entry)
	}
	if fe.Format != "federation" {
		t.Errorf("Frontend.Format = %q, want federation", fe.Format)
	}
	if fe.Expose != "./plugin" {
		t.Errorf("Frontend.Expose = %q, want ./plugin", fe.Expose)
	}
	if fe.Container != "metacore_checkout" {
		t.Errorf("Frontend.Container = %q, want metacore_checkout", fe.Container)
	}
	if fe.Layout != "immersive" {
		t.Errorf("Frontend.Layout = %q, want immersive", fe.Layout)
	}
}
