package security

import "testing"

func TestCanFetch_RejectsWildcardTargets(t *testing.T) {
	cases := []struct {
		name    string
		target  string
		probe   string
		allowed bool
	}{
		{"bare star rejected", "*", "https://evil.com/x", false},
		{"tld-only wildcard rejected", "*.com", "https://evil.com/x", false},
		{"double-wildcard rejected", "*.*", "https://evil.com/x", false},
		{"exact host ok", "api.stripe.com", "https://api.stripe.com/v1/charges", true},
		{"subdomain wildcard ok", "*.slack.com", "https://hooks.slack.com/services/abc", true},
		{"non-matching host blocked", "api.stripe.com", "https://api.evil.com/x", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := Compile("demo", nil)
			c.httpHost = append(c.httpHost, tc.target)
			err := c.CanFetch(tc.probe)
			if tc.allowed && err != nil {
				t.Fatalf("expected allow, got %v", err)
			}
			if !tc.allowed && err == nil {
				t.Fatalf("expected deny, got allow")
			}
		})
	}
}

func TestCanFetch_SSRFGuardIsIndependentOfCapability(t *testing.T) {
	c := Compile("demo", nil)
	c.httpHost = append(c.httpHost, "*.internal.example.com")
	// Even with a matching capability, loopback/IMDS must be blocked.
	for _, u := range []string{
		"http://127.0.0.1/admin",
		"http://169.254.169.254/latest/meta-data/",
		"http://10.0.0.5/",
	} {
		if err := c.CanFetch(u); err == nil {
			t.Errorf("expected SSRF block for %q", u)
		}
	}
}

func TestIsValidHTTPHostPattern(t *testing.T) {
	good := []string{"api.stripe.com", "*.slack.com", "sub.domain.example.com"}
	bad := []string{"", "*", "*.*", "*.com", "localhost", "com", "*foo.bar.com"}
	for _, g := range good {
		if !isValidHTTPHostPattern(g) {
			t.Errorf("expected %q to be valid", g)
		}
	}
	for _, b := range bad {
		if isValidHTTPHostPattern(b) {
			t.Errorf("expected %q to be invalid", b)
		}
	}
}
