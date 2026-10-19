package security

import (
	"testing"

	"github.com/asteby/metacore-kernel/manifest"
)

func TestCanSendEdgeCommand(t *testing.T) {
	c := Compile("cash_recycler", []manifest.Capability{{Kind: "edge:command", Target: "cashdro.cashdro"}, {Kind: "edge:command", Target: "drawer.*"}})
	for target, want := range map[[2]string]bool{
		{"cashdro", "cashdro"}: true, {"cashdro", "print"}: false, {"drawer", "open"}: true, {"", "cashdro"}: false, {"other", "cashdro"}: false,
	} {
		if got := c.CanSendEdgeCommand(target[0], target[1]) == nil; got != want {
			t.Errorf("%v: %v, want %v", target, got, want)
		}
	}
	if (*Capabilities)(nil).CanSendEdgeCommand("cashdro", "cashdro") == nil {
		t.Fatal("nil capabilities granted")
	}
}
