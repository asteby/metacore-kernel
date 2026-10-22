package security

import (
	"testing"

	"github.com/asteby/metacore-kernel/manifest"
)

func TestCanManageDomain(t *testing.T) {
	c := Compile("mail", []manifest.Capability{{Kind: "domain:manage", Target: "mail"}})
	if err := c.CanManageDomain("mail"); err != nil {
		t.Fatalf("declared purpose must be allowed: %v", err)
	}
	if c.CanManageDomain("storefront") == nil {
		t.Fatal("undeclared purpose must be denied")
	}
	all := Compile("x", []manifest.Capability{{Kind: "domain:manage", Target: "*"}})
	if all.CanManageDomain("anything") != nil {
		t.Fatal("wildcard purpose must allow")
	}
	if Compile("y", nil).CanManageDomain("mail") == nil {
		t.Fatal("no capability must deny")
	}
	if (*Capabilities)(nil).CanManageDomain("mail") == nil {
		t.Fatal("nil capabilities granted")
	}
}
