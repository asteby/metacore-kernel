package webhookout

import (
	"crypto/tls"
	"net/http"
	"net/url"
)

// CheckURL validates a destination against the policy BEFORE any connection
// (https only, no internal/private hosts, AllowHosts, AllowPathPrefixes). It is
// the early, accurate error; the dial-time guard of NewClient is the real
// defence. Exported so other kernel packages (webhooks) share one SSRF rule
// instead of growing a third variant.
func (p Policy) CheckURL(u *url.URL) error { return p.check(u) }

// NewClient returns the policy's egress client: no proxy, no redirects (a 3xx
// is returned as is), every connection checked on the resolved IP.
func (p Policy) NewClient() *http.Client { return p.newHTTPClient() }

// RedactJSON replaces every secret-keyed value of a JSON document (default keys
// plus extraKeys, case-insensitive, any depth) and returns the re-encoding. A
// body that is not JSON returns nil: it is neither parsed nor surfaced.
func RedactJSON(body []byte, extraKeys ...string) []byte {
	_, out := redactJSON(body, secretSet(extraKeys))
	return out
}

// NewClientTLS is NewClient with an explicit TLS configuration (private CA of an
// internal receiver, or tests). A nil cfg is NewClient.
func (p Policy) NewClientTLS(cfg *tls.Config) *http.Client {
	c := p.newHTTPClient()
	if tr, ok := c.Transport.(*http.Transport); ok && cfg != nil {
		tr.TLSClientConfig = cfg
	}
	return c
}

// CheckHost reports whether host (name or ip, optionally ":port") is one of the
// policy's explicitly authorized internal destinations.
func (p Policy) CheckHost(u *url.URL) bool { return matchInternal(p.AllowInternal, u) }
