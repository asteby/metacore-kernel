package manifest_test

import (
	"strings"
	"testing"

	"github.com/asteby/metacore-kernel/manifest"
	v3 "github.com/asteby/metacore-kernel/manifest/v3"
)

// Fase D (asteby-platform-continuation-2026-09-10.md §6): tools[] and
// subscriptions[] accept handler.type=native + operation, projected through
// the EXACT same handlerToTrigger path actions already use — one invoker,
// one validation contract, no second dispatch mechanism.
const nativeSubscriptionAndToolJSON = `{
  "apiVersion": "asteby.com/v3",
  "kind": "Addon",
  "metadata": { "key": "link_inbox", "name": "Link Inbox", "version": "1.0.0" },
  "compatibility": { "requires": [{ "key": "kernel", "version": ">=3.0.0 <4.0.0" }] },
  "capabilities": [
    { "kind": "event:subscribe", "target": "connector_whatsapp.whatsapp.message.received" }
  ],
  "contributions": {
    "subscriptions": [{
      "event": "connector_whatsapp.whatsapp.message.received",
      "handler": { "type": "native", "operation": "ingest_native_event" }
    }],
    "tools": [{
      "key": "send_whatsapp_message",
      "description": "Send a WhatsApp message to a contact",
      "handler": { "type": "native", "operation": "send_message" }
    }]
  }
}`

func TestFromV3SubscriptionNativeMapsOperation(t *testing.T) {
	m, err := v3.Parse([]byte(nativeSubscriptionAndToolJSON))
	if err != nil {
		t.Fatalf("v3 parse: %v", err)
	}
	legacy := manifest.FromV3(m)
	if len(legacy.Subscriptions) != 1 {
		t.Fatalf("expected 1 subscription, got %#v", legacy.Subscriptions)
	}
	sub := legacy.Subscriptions[0]
	if sub.Event != "connector_whatsapp.whatsapp.message.received" {
		t.Fatalf("event not carried over: %#v", sub)
	}
	if sub.Trigger == nil || sub.Trigger.Type != "native" || sub.Trigger.Operation != "ingest_native_event" {
		t.Fatalf("native subscription trigger was lost: %#v", sub.Trigger)
	}
	if err := legacy.Validate("3.0.0"); err != nil {
		t.Fatalf("strict validate: %v", err)
	}
}

func TestFromV3ToolNativeMapsOperation(t *testing.T) {
	m, err := v3.Parse([]byte(nativeSubscriptionAndToolJSON))
	if err != nil {
		t.Fatalf("v3 parse: %v", err)
	}
	legacy := manifest.FromV3(m)
	if len(legacy.Tools) != 1 {
		t.Fatalf("expected 1 tool, got %#v", legacy.Tools)
	}
	tool := legacy.Tools[0]
	if tool.Trigger == nil || tool.Trigger.Type != "native" || tool.Trigger.Operation != "send_message" {
		t.Fatalf("native tool trigger was lost: %#v", tool.Trigger)
	}
	if err := legacy.Validate("3.0.0"); err != nil {
		t.Fatalf("strict validate: %v", err)
	}
}

func TestFromV3SubscriptionNativeRequiresOperation(t *testing.T) {
	bad := strings.Replace(nativeSubscriptionAndToolJSON, `, "operation": "ingest_native_event"`, "", 1)
	m, err := v3.Parse([]byte(bad))
	if err != nil {
		t.Fatalf("v3 parse: %v", err)
	}
	legacy := manifest.FromV3(m)
	if err := legacy.Validate("3.0.0"); err == nil || !strings.Contains(err.Error(), "subscriptions[0].trigger.operation") {
		t.Fatalf("expected subscriptions[0] operation-required error, got %v", err)
	}
}

func TestFromV3ToolNativeRequiresOperation(t *testing.T) {
	bad := strings.Replace(nativeSubscriptionAndToolJSON, `, "operation": "send_message"`, "", 1)
	m, err := v3.Parse([]byte(bad))
	if err != nil {
		t.Fatalf("v3 parse: %v", err)
	}
	legacy := manifest.FromV3(m)
	if err := legacy.Validate("3.0.0"); err == nil || !strings.Contains(err.Error(), "tools[0].trigger.operation") {
		t.Fatalf("expected tools[0] operation-required error, got %v", err)
	}
}

// A wasm-typed subscription must keep working exactly like before Fase D —
// handlerToTrigger is a superset, not a rewrite.
func TestFromV3SubscriptionWasmUnaffectedByNativeSupport(t *testing.T) {
	wasmJSON := `{
	  "apiVersion": "asteby.com/v3",
	  "kind": "Addon",
	  "metadata": { "key": "link_inbox", "name": "Link Inbox", "version": "1.0.0" },
	  "compatibility": { "requires": [{ "key": "kernel", "version": ">=3.0.0 <4.0.0" }] },
	  "capabilities": [
	    { "kind": "event:subscribe", "target": "connector_whatsapp.whatsapp.message.received" }
	  ],
	  "contributions": {
	    "subscriptions": [{
	      "event": "connector_whatsapp.whatsapp.message.received",
	      "handler": { "type": "wasm", "function": "ingest_message" }
	    }]
	  }
	}`
	m, err := v3.Parse([]byte(wasmJSON))
	if err != nil {
		t.Fatalf("v3 parse: %v", err)
	}
	legacy := manifest.FromV3(m)
	if len(legacy.Subscriptions) != 1 || legacy.Subscriptions[0].Trigger == nil {
		t.Fatalf("wasm subscription did not project: %#v", legacy.Subscriptions)
	}
	trigger := legacy.Subscriptions[0].Trigger
	if trigger.Type != "wasm" || trigger.Export != "ingest_message" {
		t.Fatalf("wasm export was lost: %#v", trigger)
	}
	if err := legacy.Validate("3.0.0"); err != nil {
		t.Fatalf("strict validate: %v", err)
	}
}
