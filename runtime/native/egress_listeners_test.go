package native

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestParseEgress(t *testing.T) {
	ok := map[string]EgressRule{
		"smtp.example.com":     {Host: "smtp.example.com"},
		"SMTP.Example.com:587": {Host: "smtp.example.com", Port: 587},
		"*:25":                 {AnyHost: true, Port: 25},
		"*:993":                {AnyHost: true, Port: 993},
		"web.whatsapp.com:443": {Host: "web.whatsapp.com", Port: 443},
	}
	for in, want := range ok {
		got, err := ParseEgress(in)
		if err != nil || got != want {
			t.Errorf("ParseEgress(%q) = %+v, %v; want %+v", in, got, err, want)
		}
	}
	bad := []string{
		"", "*", "*:*", "*:80", "*:443", "*:0", "*:65536", "*:abc", "*:025",
		"*.example.com", "*.example.com:25", "mail.*:25", "a*b:25",
		"host:0", "host:70000", "host:", ":25", "host:25:26", "host/path",
		"10.0.0.0/8", "host name:25", "[::1]:25",
		"127.0.0.1", "127.0.0.1:25", "169.254.169.254:80", "10.0.0.1", "localhost", "localhost:25", "app.localhost:25", "2130706433:25", "0x7f.1:25", "::1",
	}
	for _, in := range bad {
		if r, err := ParseEgress(in); err == nil {
			t.Errorf("ParseEgress(%q) = %+v; want error", in, r)
		}
	}
}

func TestNetworkPolicyAllows(t *testing.T) {
	n := NetworkPolicy{Egress: []string{"*:25", "smtp.relay.example:587", "api.example.com", "bogus*"}}
	cases := []struct {
		host string
		port int
		want bool
	}{
		{"mx.gmail.com", 25, true},
		{"mx.gmail.com", 587, false},
		{"smtp.relay.example", 587, true},
		{"SMTP.relay.example.", 587, true},
		{"smtp.relay.example", 25, true}, // covered by *:25
		{"smtp.relay.example", 465, false},
		{"api.example.com", 443, true}, // host-only entry: any port
		{"other.example.com", 443, false},
	}
	for _, c := range cases {
		if got := n.Allows(c.host, c.port); got != c.want {
			t.Errorf("Allows(%s,%d)=%v want %v", c.host, c.port, got, c.want)
		}
	}
	if (NetworkPolicy{}).Allows("x.com", 25) {
		t.Error("empty policy must deny")
	}
}

func TestSpecEgressWildcardPolicy(t *testing.T) {
	s := validSpec()
	s.Network.Egress = []string{"*:25", "*:587", "imap.example.com:993"}
	if err := s.Validate(); err != nil {
		t.Fatalf("port wildcards must validate: %v", err)
	}
	for _, bad := range []string{"*", "*:8080", "*.example.com", "*:*"} {
		s.Network.Egress = []string{bad}
		if err := s.Validate(); err == nil || !strings.Contains(err.Error(), "network.egress[0]") {
			t.Errorf("egress %q must be rejected, got %v", bad, err)
		}
	}
}

func TestSpecListeners(t *testing.T) {
	ok := validSpec()
	ok.Listeners = []Listener{
		{Name: "smtp", Port: 25, ProxyProtocol: ProxyProtocolV2},
		{Name: "submission", Port: 587},
		{Name: "imaps", Port: 993, ProxyProtocol: ProxyProtocolV1},
	}
	if err := ok.Validate(); err != nil {
		t.Fatalf("valid listeners: %v", err)
	}
	if ok.Ingress != nil {
		t.Fatal("listeners must not imply HTTP ingress")
	}
	if got := ListenerEnv("submission"); got != "METACORE_NATIVE_LISTENER_SUBMISSION" {
		t.Errorf("ListenerEnv = %s", got)
	}
	bad := map[string][]Listener{
		"port 0":     {{Name: "a", Port: 0}},
		"port high":  {{Name: "a", Port: 70000}},
		"bad name":   {{Name: "SMTP", Port: 25}},
		"empty name": {{Port: 25}},
		"bad proxy":  {{Name: "a", Port: 25, ProxyProtocol: "v3"}},
		"dup name":   {{Name: "a", Port: 25}, {Name: "a", Port: 26}},
		"dup port":   {{Name: "a", Port: 25}, {Name: "b", Port: 25}},
		"ssh":        {{Name: "a", Port: 22}},
		"http":       {{Name: "a", Port: 80}},
		"https":      {{Name: "a", Port: 443}},
		"postgres":   {{Name: "a", Port: 5432}},
		"redis":      {{Name: "a", Port: 6379}},
	}
	for name, ls := range bad {
		s := validSpec()
		s.Listeners = ls
		if err := s.Validate(); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
	many := validSpec()
	for i := 0; i <= MaxListeners; i++ {
		many.Listeners = append(many.Listeners, Listener{Name: "l" + string(rune('a'+i)), Port: 1000 + i})
	}
	if err := many.Validate(); err == nil {
		t.Error("too many listeners must fail")
	}
}

// Ingress must stay comparable (ops compares it with ==) and HTTP-only.
func TestIngressStaysComparable(t *testing.T) {
	a, b := Ingress{Kind: IngressKindHTTP}, Ingress{Kind: IngressKindHTTP}
	if a != b {
		t.Fatal("Ingress must be comparable")
	}
	if err := (Ingress{Kind: "tcp"}).Validate(); err == nil {
		t.Fatal("Ingress kind tcp no longer exists")
	}
}

func TestListenerRegistryCollisions(t *testing.T) {
	r := NewListenerRegistry()
	if err := r.Claim("mailer-1", []Listener{{Name: "smtp", Port: 25}, {Name: "sub", Port: 587}}); err != nil {
		t.Fatal(err)
	}
	if err := r.Claim("other", []Listener{{Name: "x", Port: 9999}, {Name: "y", Port: 587}}); err == nil {
		t.Fatal("port 587 is owned by mailer-1")
	}
	// A failed claim reserves nothing.
	if err := r.Claim("third", []Listener{{Name: "x", Port: 9999}}); err != nil {
		t.Fatalf("9999 must be free after failed claim: %v", err)
	}
	if err := r.Claim("mailer-1", []Listener{{Name: "smtp", Port: 25}}); err != nil {
		t.Fatalf("same owner re-claim: %v", err)
	}
	if err := r.Claim("other", []Listener{{Name: "y", Port: 587}}); err != nil {
		t.Fatalf("587 freed by mailer-1 re-claim: %v", err)
	}
	r.Release("other")
	if err := r.Claim("mailer-1", []Listener{{Name: "sub", Port: 587}}); err != nil {
		t.Fatal(err)
	}
	if err := r.Claim("x", []Listener{{Name: "ssh", Port: 22}}); err == nil {
		t.Fatal("forbidden port must fail")
	}
	if err := r.Claim("", nil); err == nil {
		t.Fatal("owner required")
	}
}

func TestNetworkPolicyDialContext(t *testing.T) {
	n := NetworkPolicy{Egress: []string{"*:25", "api.example.com:443"}}
	ctx := context.Background()
	// Denied by the grammar.
	if _, err := n.DialContext(ctx, "tcp", "mx.example.com:2222"); !errors.Is(err, ErrEgressDenied) {
		t.Errorf("port not allowed: %v", err)
	}
	if _, err := n.DialContext(ctx, "udp", "mx.example.com:25"); !errors.Is(err, ErrEgressDenied) {
		t.Errorf("udp: %v", err)
	}
	// IP literals are refused even when *:25 would match any host.
	for _, a := range []string{"127.0.0.1:25", "169.254.169.254:25", "10.0.0.5:25", "[::1]:25"} {
		if _, err := n.DialContext(ctx, "tcp", a); !errors.Is(err, ErrEgressDenied) {
			t.Errorf("%s: %v", a, err)
		}
	}
	// A name resolving to loopback passes the grammar and dies in netguard.
	if _, err := n.DialContext(ctx, "tcp", "localhost:25"); err == nil {
		t.Error("localhost must never be dialled")
	}
}
