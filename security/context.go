package security

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/asteby/metacore-kernel/manifest"
)

// Capabilities is the compiled policy derived from an addon's declared
// manifest.Capabilities. The runtime consults it for every privileged action
// the addon attempts.
type Capabilities struct {
	addonKey string
	dbRead   []string // model globs: "orders", "addon_tickets.*"
	dbWrite  []string
	httpHost []string // host globs: "api.stripe.com", "*.slack.com"
	// connHost are http:fetch grants whose host is not fixed in the manifest
	// but comes from the org's connector configuration
	// ("connector:<connector>.<credential>", e.g. a store_url). The runtime
	// resolves them per invocation; see CanFetchHosts.
	connHost []ConnectorHostRef
	eventPub []string
	eventSub []string
	connRead []string        // connector keys: "github", "*" for any
	ctxScope map[string]bool // ctx_get slices: "user", "roles", "org_config"
}

// Compile turns a manifest's declarations into a Capabilities policy.
func Compile(addonKey string, caps []manifest.Capability) *Capabilities {
	c := &Capabilities{addonKey: addonKey}
	for _, cap := range caps {
		switch cap.Kind {
		case "db:read":
			c.dbRead = append(c.dbRead, cap.Target)
		case "db:write":
			c.dbWrite = append(c.dbWrite, cap.Target)
		case "http:fetch":
			if ref, ok := ParseConnectorHostRef(cap.Target); ok {
				c.connHost = append(c.connHost, ref)
				continue
			}
			c.httpHost = append(c.httpHost, cap.Target)
		case "event:emit":
			c.eventPub = append(c.eventPub, cap.Target)
		case "event:subscribe":
			c.eventSub = append(c.eventSub, cap.Target)
		case "connector:read":
			c.connRead = append(c.connRead, cap.Target)
		case "ctx:user", "ctx:roles", "ctx:org_config":
			// The target is ignored: each kind IS the slice. Kept as a set so
			// the ctx_get import can answer "which slices may this addon see".
			if c.ctxScope == nil {
				c.ctxScope = map[string]bool{}
			}
			c.ctxScope[strings.TrimPrefix(cap.Kind, "ctx:")] = true
		}
	}
	// Every addon is implicitly allowed to read/write its own schema.
	selfSchema := "addon_" + strings.ToLower(addonKey) + ".*"
	c.dbRead = append(c.dbRead, selfSchema)
	c.dbWrite = append(c.dbWrite, selfSchema)
	return c
}

// CanReadModel returns nil if the addon may read the given model.
func (c *Capabilities) CanReadModel(model string) error {
	if matchAny(c.dbRead, model) || matchAny(c.dbWrite, model) {
		return nil
	}
	return fmt.Errorf("addon %q lacks db:read %q", c.addonKey, model)
}

// CanWriteModel returns nil if the addon may write to the given model.
func (c *Capabilities) CanWriteModel(model string) error {
	if matchAny(c.dbWrite, model) {
		return nil
	}
	return fmt.Errorf("addon %q lacks db:write %q", c.addonKey, model)
}

// CanFetch returns nil if the addon may perform an outbound HTTP call to url.
// Defense in depth: SSRF targets (loopback, private ranges, cloud IMDS) are
// rejected even when the capability list would otherwise allow them.
func (c *Capabilities) CanFetch(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("invalid URL: %w", err)
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return fmt.Errorf("only http(s) allowed")
	}
	if isBlockedEgressHost(u.Hostname()) {
		return fmt.Errorf("egress to %q is blocked (SSRF guard)", u.Hostname())
	}
	if matchHTTPHost(c.httpHost, u.Host) {
		return nil
	}
	return fmt.Errorf("addon %q lacks http:fetch %q", c.addonKey, u.Host)
}

// ConnectorHostPrefix marks an http:fetch target whose host is read from a
// connector credential instead of being fixed in the manifest:
//
//	{ "kind": "http:fetch", "target": "connector:woocommerce.store_url" }
//
// grants egress to the host of the org's configured store_url (and only that
// exact host). It lets a generic connector (WooCommerce, Shopify, a self-hosted
// ERP…) talk to each tenant's own server without hardcoding any customer host.
const ConnectorHostPrefix = "connector:"

// ConnectorHostRef is a parsed "connector:<connector>.<credential>" target.
type ConnectorHostRef struct {
	Connector  string
	Credential string
}

// ParseConnectorHostRef parses a "connector:<connector>.<credential>" http:fetch
// target. ok is false for any other target (a plain host pattern).
func ParseConnectorHostRef(target string) (ConnectorHostRef, bool) {
	rest, found := strings.CutPrefix(strings.TrimSpace(target), ConnectorHostPrefix)
	if !found {
		return ConnectorHostRef{}, false
	}
	conn, cred, ok := strings.Cut(rest, ".")
	if !ok || conn == "" || cred == "" || strings.ContainsAny(rest, "*/: ") {
		return ConnectorHostRef{}, false
	}
	return ConnectorHostRef{Connector: conn, Credential: cred}, true
}

// ConnectorHostRefs returns the connector-derived http:fetch grants.
func (c *Capabilities) ConnectorHostRefs() []ConnectorHostRef {
	if c == nil {
		return nil
	}
	return append([]ConnectorHostRef(nil), c.connHost...)
}

// HostFromCredential extracts the host[:port] a connector credential points at.
// Accepts a full URL ("https://shop.example.com/") or a bare host
// ("shop.example.com"). Returns "" when the value has no usable host.
func HostFromCredential(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return ""
	}
	if !strings.Contains(v, "://") {
		v = "https://" + v
	}
	u, err := url.Parse(v)
	if err != nil || u.Hostname() == "" || strings.Contains(u.Host, "*") {
		return ""
	}
	return strings.ToLower(u.Host)
}

// CanFetchHosts is CanFetch widened with exact hosts resolved at runtime from
// the connector-derived grants (ConnectorHostRefs). The scheme check and the
// SSRF guard apply exactly as in CanFetch: a credential pointing at loopback or
// a private range is still refused.
func (c *Capabilities) CanFetchHosts(rawURL string, hosts []string) error {
	err := c.CanFetch(rawURL)
	if err == nil || len(hosts) == 0 {
		return err
	}
	u, perr := url.Parse(rawURL)
	if perr != nil || (u.Scheme != "https" && u.Scheme != "http") || isBlockedEgressHost(u.Hostname()) {
		return err
	}
	got := strings.ToLower(u.Host)
	for _, h := range hosts {
		if h != "" && h == got {
			return nil
		}
	}
	return err
}

// matchHTTPHost is stricter than matchAny: a capability pattern must resolve
// to a concrete domain with a TLD. Bare "*", "*.*", "*.com" et al are rejected
// because they would neutralise the capability system — a marketplace publisher
// could exfiltrate to any host by declaring one of them.
func matchHTTPHost(patterns []string, value string) bool {
	for _, p := range patterns {
		if !isValidHTTPHostPattern(p) {
			continue
		}
		if p == value {
			return true
		}
		if strings.HasPrefix(p, "*.") {
			suffix := strings.TrimPrefix(p, "*.")
			if strings.HasSuffix(value, "."+suffix) || value == suffix {
				return true
			}
		}
	}
	return false
}

// isValidHTTPHostPattern enforces that a http:fetch target has at least one
// dot and that any wildcard is a leftmost-label wildcard above a registrable
// domain (so "*.example.com" is fine, "*.com" is not).
func isValidHTTPHostPattern(p string) bool {
	if p == "" || p == "*" {
		return false
	}
	// Strip optional port for validation.
	host := p
	if i := strings.LastIndex(host, ":"); i >= 0 && !strings.Contains(host[i+1:], ".") {
		host = host[:i]
	}
	if strings.HasPrefix(host, "*.") {
		host = strings.TrimPrefix(host, "*.")
	}
	// Reject leftover wildcards anywhere else.
	if strings.Contains(host, "*") {
		return false
	}
	// Require a registrable domain: at least two labels, each non-empty.
	labels := strings.Split(host, ".")
	if len(labels) < 2 {
		return false
	}
	for _, l := range labels {
		if l == "" {
			return false
		}
	}
	return true
}

// isBlockedEgressHost rejects hosts that no addon should reach, regardless of
// the capability list. Covers loopback, private ranges and cloud instance
// metadata endpoints.
func isBlockedEgressHost(h string) bool {
	switch strings.ToLower(h) {
	case "", "localhost", "127.0.0.1", "::1", "0.0.0.0":
		return true
	case "169.254.169.254", "metadata.google.internal", "metadata":
		return true
	}
	if strings.HasPrefix(h, "10.") || strings.HasPrefix(h, "192.168.") {
		return true
	}
	if strings.HasPrefix(h, "172.") {
		parts := strings.SplitN(h, ".", 3)
		if len(parts) >= 2 {
			if n, err := strconv.Atoi(parts[1]); err == nil && n >= 16 && n <= 31 {
				return true
			}
		}
	}
	return false
}

// CanEmit returns nil if the addon may publish a given event.
func (c *Capabilities) CanEmit(event string) error {
	if matchAny(c.eventPub, event) {
		return nil
	}
	return fmt.Errorf("addon %q lacks event:emit %q", c.addonKey, event)
}

// CanSubscribe returns nil if the addon may subscribe to a given event.
func (c *Capabilities) CanSubscribe(event string) error {
	if matchAny(c.eventSub, event) {
		return nil
	}
	return fmt.Errorf("addon %q lacks event:subscribe %q", c.addonKey, event)
}

// CanReadConnector returns nil if the addon may read the credentials of a given
// connector via the connector_get capability. The target is the connector key
// ("github") or "*" for any. Without a matching `connector:read` capability the
// guest's connector_get returns a forbidden envelope.
func (c *Capabilities) CanReadConnector(connectorKey string) error {
	if matchAny(c.connRead, connectorKey) {
		return nil
	}
	return fmt.Errorf("addon %q lacks connector:read %q", c.addonKey, connectorKey)
}

// CanReadContext returns nil if the addon declared the `ctx:<scope>` capability
// (scope: "user" | "roles" | "org_config") that unlocks that slice of the
// ctx_get host import. Least privilege: there is no implicit grant and no
// wildcard — an addon that declares only ctx:org_config never sees the user.
func (c *Capabilities) CanReadContext(scope string) error {
	if c != nil && c.ctxScope[scope] {
		return nil
	}
	addon := ""
	if c != nil {
		addon = c.addonKey
	}
	return fmt.Errorf("addon %q lacks ctx:%s", addon, scope)
}

// matchAny reports whether value matches any of the glob patterns. A pattern
// of "*" matches everything; trailing ".*" matches any suffix; otherwise the
// pattern must equal value.
func matchAny(patterns []string, value string) bool {
	for _, p := range patterns {
		if p == "*" || p == value {
			return true
		}
		if strings.HasSuffix(p, ".*") {
			prefix := strings.TrimSuffix(p, ".*")
			if strings.HasPrefix(value, prefix+".") || value == prefix {
				return true
			}
		}
		if strings.HasPrefix(p, "*.") {
			suffix := strings.TrimPrefix(p, "*.")
			if strings.HasSuffix(value, "."+suffix) || value == suffix {
				return true
			}
		}
	}
	return false
}
