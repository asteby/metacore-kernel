package security

import (
	"testing"

	"github.com/asteby/metacore-kernel/manifest"
)

// QA-EXP-ROLES / EXP-ROL-16: el conector genérico de WooCommerce tenía su
// http:fetch fijado al host de un cliente. "connector:<conn>.<cred>" deja que el
// host salga de la configuración del conector de cada org.
func TestConnectorHostGrant(t *testing.T) {
	c := Compile("integration_woocommerce", []manifest.Capability{
		{Kind: "http:fetch", Target: "connector:woocommerce.store_url"},
		{Kind: "connector:read", Target: "woocommerce"},
	})
	refs := c.ConnectorHostRefs()
	if len(refs) != 1 || refs[0] != (ConnectorHostRef{Connector: "woocommerce", Credential: "store_url"}) {
		t.Fatalf("refs = %+v", refs)
	}
	// El target "connector:" nunca es un patrón de host literal.
	if err := c.CanFetch("https://connector:woocommerce.store_url/x"); err == nil {
		t.Fatal("connector target must not match as a literal host")
	}
	// Sin hosts resueltos → negado.
	if err := c.CanFetchHosts("https://shop.example.com/wp-json/wc/v3/orders", nil); err == nil {
		t.Fatal("unresolved connector host must be refused")
	}
	host := HostFromCredential("https://Shop.Example.com/")
	if host != "shop.example.com" {
		t.Fatalf("HostFromCredential = %q", host)
	}
	if err := c.CanFetchHosts("https://shop.example.com/wp-json/wc/v3/orders?sku=a%26b", []string{host}); err != nil {
		t.Fatalf("configured store host must be allowed: %v", err)
	}
	// Solo el host exacto: ni otro host ni subdominios.
	for _, u := range []string{"https://evil.example.com/", "https://x.shop.example.com/", "https://shop.example.com.evil.io/", "ftp://shop.example.com/"} {
		if err := c.CanFetchHosts(u, []string{host}); err == nil {
			t.Fatalf("%s must be refused", u)
		}
	}
	// El guard SSRF sigue aplicando aunque la credencial apunte a loopback/red privada.
	for _, cred := range []string{"http://127.0.0.1:8080", "10.0.0.5", "http://localhost"} {
		h := HostFromCredential(cred)
		if err := c.CanFetchHosts("http://"+h+"/", []string{h}); err == nil {
			t.Fatalf("SSRF target %q must be refused", cred)
		}
	}
}

func TestParseConnectorHostRef(t *testing.T) {
	for _, bad := range []string{"api.example.com", "connector:", "connector:woo", "connector:.x", "connector:woo.", "connector:woo.*", "connector:woo/x.y"} {
		if _, ok := ParseConnectorHostRef(bad); ok {
			t.Errorf("%q must not parse", bad)
		}
	}
	if HostFromCredential("") != "" || HostFromCredential("https://") != "" {
		t.Error("empty credential must yield no host")
	}
	if got := HostFromCredential("tienda.example.mx:8443"); got != "tienda.example.mx:8443" {
		t.Errorf("bare host with port = %q", got)
	}
}
