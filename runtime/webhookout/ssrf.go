package webhookout

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/asteby/metacore-kernel/netguard"
)

// Policy is the egress policy the HOST configures. The zero value is the safe
// one: https only, public destinations only, loopback / private / link-local /
// metadata / CGNAT denied (checked on the resolved IP at connect time, so DNS
// rebinding does not bypass it).
type Policy struct {
	// AllowInternal lists the internal destinations the host explicitly
	// authorizes (for example the base of the trusted core). Each entry is a
	// host name or IP literal, optionally with ":port" (lower-case). There are
	// no wildcards and no defaults: an internal host is reachable only if it is
	// named here. An entry authorizes that name/IP whatever it resolves to, so
	// list names you control.
	AllowInternal []string
	// AllowHosts optionally narrows PUBLIC destinations (exact host or
	// "*.domain", netguard syntax; a host with a non-default port must be
	// listed with the port). Empty = any public host on 443.
	AllowHosts []string
	// AllowInsecureHTTP permits plain http://, but ONLY toward a host that is
	// also in AllowInternal: cleartext never crosses the public internet.
	AllowInsecureHTTP bool
}

func matchInternal(list []string, u *url.URL) bool {
	h, hp := strings.ToLower(u.Hostname()), strings.ToLower(u.Host)
	for _, e := range list {
		e = strings.ToLower(strings.TrimSpace(e))
		if e != "" && (e == h || e == hp) {
			return true
		}
	}
	return false
}

// check validates the destination BEFORE any connection. The dial-time guard in
// transport() is the real defence; this gives early, accurate errors.
func (p Policy) check(u *url.URL) error {
	host := strings.ToLower(u.Hostname())
	if host == "" {
		return fmt.Errorf("%w: empty host", ErrDestinationBlocked)
	}
	internal := matchInternal(p.AllowInternal, u)
	switch u.Scheme {
	case "https":
	case "http":
		if !(p.AllowInsecureHTTP && internal) {
			return fmt.Errorf("%w: http:// is only allowed toward an explicitly authorized internal host", ErrDestinationBlocked)
		}
	default:
		return fmt.Errorf("%w: scheme %q", ErrDestinationBlocked, u.Scheme)
	}
	if internal {
		return nil
	}
	if ip := net.ParseIP(host); ip != nil {
		if netguard.IsBlockedIP(ip) {
			return fmt.Errorf("%w: %s is an internal address", ErrDestinationBlocked, host)
		}
	} else if netguard.HasForbiddenSuffix(host) || !strings.Contains(host, ".") {
		return fmt.Errorf("%w: %s is not a public host name", ErrDestinationBlocked, host)
	}
	if port := u.Port(); port != "" && port != "443" && u.Scheme == "https" {
		if !netguard.HostAllowed(p.AllowHosts, strings.ToLower(u.Host)) {
			return fmt.Errorf("%w: port %s is not authorized", ErrDestinationBlocked, port)
		}
		return nil
	}
	if len(p.AllowHosts) > 0 && !netguard.HostAllowed(p.AllowHosts, strings.ToLower(u.Host)) &&
		!netguard.HostAllowed(p.AllowHosts, host) {
		return fmt.Errorf("%w: %s is not in the allowed hosts", ErrDestinationBlocked, host)
	}
	return nil
}

// newHTTPClient builds the client: no proxy, no redirects (a 3xx is returned
// as is and counted as a rejection), per-connection IP guard except toward the
// explicitly authorized internal hosts.
func (p Policy) newHTTPClient() *http.Client {
	guarded := &net.Dialer{Timeout: 10 * time.Second, Control: netguard.DialControl}
	open := &net.Dialer{Timeout: 10 * time.Second}
	internal := append([]string(nil), p.AllowInternal...)
	tr := &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			h, _, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, fmt.Errorf("%w: %s", ErrDestinationBlocked, addr)
			}
			hl, al := strings.ToLower(h), strings.ToLower(addr)
			for _, e := range internal {
				e = strings.ToLower(strings.TrimSpace(e))
				if e != "" && (e == hl || e == al) {
					return open.DialContext(ctx, network, addr)
				}
			}
			return guarded.DialContext(ctx, network, addr)
		},
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		DisableKeepAlives:     true,
		ForceAttemptHTTP2:     true,
	}
	return &http.Client{
		Transport:     tr,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}
