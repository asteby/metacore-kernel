package capability

import "testing"

// Every well-known contract carries human copy in Spanish and English, and
// belongs to a registered domain: admin screens never fall back to the key.
func TestContractsAreDescribed(t *testing.T) {
	for _, k := range Keys() {
		c := Contracts[k]
		for _, l := range []string{"es", "en"} {
			if c.Title[l] == "" || c.Description[l] == "" {
				t.Errorf("%s: missing %s title/description", k, l)
			}
		}
		if _, ok := Domains[DomainOf(k)]; !ok {
			t.Errorf("%s: domain %q is not registered", k, DomainOf(k))
		}
	}
}

func TestCheckoutContracts(t *testing.T) {
	for _, k := range []string{CommercePaymentsSession, CommercePaymentsConfirm, CommercePaymentsStatus} {
		if !Contracts[k].Checkout {
			t.Errorf("%s should be a checkout contract", k)
		}
	}
	if Contracts[CommercePaymentsRefund].Checkout {
		t.Error("refund is not needed to check out")
	}
}

func TestDomainOf(t *testing.T) {
	cases := map[string]string{
		CommercePaymentsSession:        "commerce.payments",
		CommercePaymentsTerminalCreate: "commerce.payments",
		CommercePaymentsRefundStatus:   "commerce.payments",
		MessagingWhatsAppSend:          "messaging.whatsapp",
		GeoGeocode:                     "geo",
		RegionPostalLocate:             "region",
		"acme.loyalty.earn":            "acme.loyalty",
		"solo":                         "solo",
	}
	for k, want := range cases {
		if got := DomainOf(k); got != want {
			t.Errorf("DomainOf(%q) = %q, want %q", k, got, want)
		}
	}
	keys := DomainKeys()
	if len(keys) == 0 || keys[0] != "commerce.payments" {
		t.Fatalf("payments should sort first: %v", keys)
	}
}

func TestTextGet(t *testing.T) {
	tx := Text{"es": "Pagos", "en": "Payments"}
	for locale, want := range map[string]string{"es": "Pagos", "es-MX": "Pagos", "en-US": "Payments", "fr": "Pagos", "": "Pagos"} {
		if got := tx.Get(locale); got != want {
			t.Errorf("Get(%q) = %q, want %q", locale, got, want)
		}
	}
	if (Text{"en": "Only"}).Get("es") != "Only" || Text(nil).Get("es") != "" {
		t.Error("fallbacks")
	}
}

// A host contract registered with its own copy keeps it.
func TestRegisterKeepsOwnCopy(t *testing.T) {
	key := "acme.loyalty.earn"
	t.Cleanup(func() { delete(Contracts, key) })
	if err := Register(Contract{Key: key, Kind: KindAction, Title: Text{"es": "Sumar puntos"}}); err != nil {
		t.Fatal(err)
	}
	if Contracts[key].Title.Get("es") != "Sumar puntos" {
		t.Fatal("own title lost")
	}
}
