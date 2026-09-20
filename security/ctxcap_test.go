package security

import (
	"testing"

	"github.com/asteby/metacore-kernel/manifest"
)

func TestCanReadContext_LeastPrivilege(t *testing.T) {
	none := Compile("a", nil)
	for _, s := range []string{"user", "roles", "org_config"} {
		if none.CanReadContext(s) == nil {
			t.Fatalf("no declaration must not grant ctx:%s", s)
		}
	}
	c := Compile("a", []manifest.Capability{{Kind: "ctx:org_config", Target: "*"}})
	if c.CanReadContext("org_config") != nil {
		t.Fatal("declared ctx:org_config must be granted")
	}
	if c.CanReadContext("user") == nil || c.CanReadContext("roles") == nil {
		t.Fatal("ctx:org_config must not leak user/roles")
	}
	var nilCaps *Capabilities
	if nilCaps.CanReadContext("user") == nil {
		t.Fatal("nil policy must deny")
	}
}
