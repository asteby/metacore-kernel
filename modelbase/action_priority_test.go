package modelbase

import (
	"encoding/json"
	"testing"

	"github.com/asteby/metacore-kernel/manifest"
)

// The host serves manifest.ActionDef through modelbase.ActionDef via JSON; a
// field missing on either side is silently dropped and the SDK never sees it.
func TestActionDef_PriorityRoundTrip(t *testing.T) {
	raw, err := json.Marshal(manifest.ActionDef{Key: "share", Priority: "secondary"})
	if err != nil {
		t.Fatal(err)
	}
	var got ActionDef
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got.Priority != "secondary" {
		t.Fatalf("priority lost in round-trip: %s", raw)
	}
}
