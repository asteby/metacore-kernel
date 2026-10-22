package native

import (
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

func TestIngressListeners(t *testing.T) {
	tcp := Ingress{Kind: IngressKindTCP, Listeners: []Listener{
		{Name: "smtp", Port: 25, ProxyProtocol: ProxyProtocolV2},
		{Name: "submission", Port: 587},
		{Name: "imaps", Port: 993, ProxyProtocol: ProxyProtocolV1},
	}}
	if err := tcp.Validate(); err != nil {
		t.Fatalf("valid tcp ingress: %v", err)
	}
	if tcp.HTTPEnabled() {
		t.Error("tcp ingress must not enable HTTP")
	}
	mixed := Ingress{Kind: IngressKindHTTP, PathPrefix: "/hook", Listeners: []Listener{{Name: "smtp", Port: 25}}}
	if err := mixed.Validate(); err != nil || !mixed.HTTPEnabled() {
		t.Fatalf("http+listeners: %v", err)
	}
	if got := ListenerEnv("submission"); got != "METACORE_NATIVE_LISTENER_SUBMISSION" {
		t.Errorf("ListenerEnv = %s", got)
	}
	bad := map[string]Ingress{
		"tcp without listeners": {Kind: IngressKindTCP},
		"tcp with http fields":  {Kind: IngressKindTCP, PathPrefix: "/x", Listeners: []Listener{{Name: "a", Port: 25}}},
		"tcp with timeout":      {Kind: IngressKindTCP, Timeout: "5s", Listeners: []Listener{{Name: "a", Port: 25}}},
		"unknown kind":          {Kind: "udp", Listeners: []Listener{{Name: "a", Port: 25}}},
		"port 0":                {Kind: IngressKindTCP, Listeners: []Listener{{Name: "a", Port: 0}}},
		"port high":             {Kind: IngressKindTCP, Listeners: []Listener{{Name: "a", Port: 70000}}},
		"bad name":              {Kind: IngressKindTCP, Listeners: []Listener{{Name: "SMTP", Port: 25}}},
		"empty name":            {Kind: IngressKindTCP, Listeners: []Listener{{Port: 25}}},
		"bad proxy":             {Kind: IngressKindTCP, Listeners: []Listener{{Name: "a", Port: 25, ProxyProtocol: "v3"}}},
		"dup name":              {Kind: IngressKindTCP, Listeners: []Listener{{Name: "a", Port: 25}, {Name: "a", Port: 26}}},
		"dup port":              {Kind: IngressKindTCP, Listeners: []Listener{{Name: "a", Port: 25}, {Name: "b", Port: 25}}},
	}
	for name, ing := range bad {
		if err := ing.Validate(); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
	many := Ingress{Kind: IngressKindTCP}
	for i := 0; i <= MaxListeners; i++ {
		many.Listeners = append(many.Listeners, Listener{Name: "l" + string(rune('a'+i)), Port: 1000 + i})
	}
	if err := many.Validate(); err == nil {
		t.Error("too many listeners must fail")
	}
}

func TestNewIngressProxyRejectsTCPKind(t *testing.T) {
	_, err := NewIngressProxy("/tmp/x.sock", Ingress{Kind: IngressKindTCP, Listeners: []Listener{{Name: "a", Port: 25}}}, IngressProxyOptions{})
	if err == nil {
		t.Fatal("tcp ingress has no HTTP proxy")
	}
}

func TestSpecAcceptsTCPIngress(t *testing.T) {
	s := validSpec()
	s.Ingress = &Ingress{Kind: IngressKindTCP, Listeners: []Listener{{Name: "smtp", Port: 25, ProxyProtocol: "v2"}}}
	if err := s.Validate(); err != nil {
		t.Fatal(err)
	}
}
