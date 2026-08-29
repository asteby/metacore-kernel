package dispatch

import (
	"encoding/json"
	"testing"
)

func TestStampEventName(t *testing.T) {
	// Domain payload without "event" → stamped.
	out := stampEventName([]byte(`{"sales_order_id":"abc"}`), "warehouse.stock_picked")
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if m["event"] != "warehouse.stock_picked" {
		t.Fatalf("expected stamped event name, got %v", m["event"])
	}
	if m["sales_order_id"] != "abc" {
		t.Fatalf("payload fields must survive, got %v", m)
	}

	// Object already naming its event → untouched.
	orig := []byte(`{"event":"custom.name","x":1}`)
	if got := stampEventName(orig, "other.name"); string(got) != string(orig) {
		t.Fatalf("existing event key must win, got %s", got)
	}

	// Non-object payloads pass through.
	arr := []byte(`[1,2,3]`)
	if got := stampEventName(arr, "e"); string(got) != string(arr) {
		t.Fatalf("array must pass through, got %s", got)
	}
}
