package netguard

import (
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"syscall"
	"testing"
)

func TestIsBlockedIP(t *testing.T) {
	for _, tc := range []struct {
		ip      string
		blocked bool
	}{
		{"127.0.0.1", true},
		{"127.0.0.2", true}, // bypass of the old string check
		{"127.255.255.254", true},
		{"::1", true},
		{"::ffff:127.0.0.1", true}, // IPv4-mapped
		{"::ffff:10.0.0.1", true},
		{"::ffff:169.254.169.254", true},
		{"::ffff:8.8.8.8", false},
		{"10.1.2.3", true},
		{"172.16.0.1", true},
		{"172.31.255.255", true},
		{"172.32.0.1", false},
		{"192.168.1.1", true},
		{"169.254.169.254", true},
		{"100.64.0.1", true},
		{"100.127.255.255", true},
		{"100.128.0.1", false},
		{"0.0.0.0", true},
		{"0.1.2.3", true},
		{"224.0.0.1", true},
		{"255.255.255.255", true},
		{"fd00::1", true},
		{"fe80::1", true},
		{"::", true},
		{"64:ff9b::7f00:1", true}, // NAT64 -> 127.0.0.1
		{"64:ff9b::808:808", false},
		{"2002:7f00:1::1", true}, // 6to4 -> 127.0.0.1
		{"8.8.8.8", false},
		{"158.69.208.1", false},
		{"2606:4700::1111", false},
	} {
		if got := IsBlockedIP(net.ParseIP(tc.ip)); got != tc.blocked {
			t.Errorf("IsBlockedIP(%s)=%v, want %v", tc.ip, got, tc.blocked)
		}
	}
	if !IsBlockedIP(nil) {
		t.Error("nil IP must be blocked")
	}
}

func TestHostAllowed(t *testing.T) {
	pats := []string{"cdn.example.com", "*.godsnutrition.com.mx"}
	for host, want := range map[string]bool{
		"cdn.example.com":               true,
		"img.godsnutrition.com.mx":      true,
		"a.b.godsnutrition.com.mx":      true,
		"godsnutrition.com.mx":          true,
		"evil.com":                      false,
		"evilgodsnutrition.com.mx":      false,
		"godsnutrition.com.mx.evil.com": false,
		"other.example.com":             false,
		"cdn.example.com:8443":          false, // port-specific rules stay explicit
		"":                              false,
	} {
		if got := HostAllowed(pats, host); got != want {
			t.Errorf("HostAllowed(%q)=%v, want %v", host, got, want)
		}
	}
	for _, bad := range []string{"*", "*.*", "*.com", "com", "", "a.*.com", "*."} {
		if IsValidHostPattern(bad) {
			t.Errorf("pattern %q must be invalid", bad)
		}
		if HostAllowed([]string{bad}, "anything.com") {
			t.Errorf("invalid pattern %q must not match", bad)
		}
	}
	if HostAllowed(nil, "a.com") {
		t.Error("empty list matches nothing")
	}
}

func TestClientBlocksLoopbackAndAllowsWithEscapeHatch(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits++ }))
	defer srv.Close()

	if _, err := NewClient(Options{}).Get(srv.URL); !errors.Is(err, ErrBlockedEgress) {
		t.Fatalf("err=%v, want ErrBlockedEgress", err)
	}
	if hits != 0 {
		t.Fatalf("internal server reached %d times", hits)
	}
	resp, err := NewClient(Options{AllowPrivate: true}).Get(srv.URL)
	if err != nil || hits != 1 {
		t.Fatalf("allowed call err=%v hits=%d", err, hits)
	}
	_ = resp.Body.Close()
}

func TestClientRevalidatesRedirects(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer target.Close()
	hop := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer hop.Close()

	// Host allowlist is re-applied on the redirect hop.
	c := NewClient(Options{AllowPrivate: true, AllowHosts: []string{"x.example.com"}})
	if _, err := c.Get(hop.URL); !errors.Is(err, ErrHostNotAllowed) {
		t.Fatalf("err=%v, want ErrHostNotAllowed", err)
	}
}

// A "public" server (its address is whitelisted by a test control) that
// redirects to loopback must be stopped when the redirect is dialed.
func TestClientBlocksRedirectToPrivate(t *testing.T) {
	var privateHits int
	private := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { privateHits++ }))
	defer private.Close()
	public := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, private.URL, http.StatusFound)
	}))
	defer public.Close()

	publicAddr := public.Listener.Addr().String()
	strict := dialControl(false)
	control := func(network, address string, c syscall.RawConn) error {
		if address == publicAddr { // pretend this one resolved to a public IP
			return nil
		}
		return strict(network, address, c)
	}
	_, err := newClient(Options{}, control).Get(public.URL)
	if !errors.Is(err, ErrBlockedEgress) {
		t.Fatalf("err=%v, want ErrBlockedEgress", err)
	}
	if privateHits != 0 {
		t.Fatalf("private server reached %d times", privateHits)
	}
}

func TestClientMaxRedirects(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, srv.URL, http.StatusFound)
	}))
	defer srv.Close()
	if _, err := NewClient(Options{AllowPrivate: true, MaxRedirects: 2}).Get(srv.URL); !errors.Is(err, ErrTooManyRedirects) {
		t.Fatalf("err=%v, want ErrTooManyRedirects", err)
	}
}
