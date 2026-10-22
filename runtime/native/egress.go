package native

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"regexp"
	"strconv"
	"strings"

	"github.com/asteby/metacore-kernel/netguard"
)

// Egress policy.
//
// NetworkPolicy.Egress is deny-by-default. Each entry is one of:
//
//	host           exact hostname, any port the host's default policy allows
//	host:port      exact hostname + exact TCP port        (smtp.example.com:587)
//	*:port         ANY destination host on one TCP port   (*:25)
//
// The third form exists for protocols whose destination is arbitrary by
// nature and cannot be enumerated in a signed manifest: SMTP delivery to the
// recipient's MX (*:25), or IMAP/SMTP submission to a server the tenant
// configures at runtime (*:993, *:587). It is the ONLY wildcard the contract
// admits, and it is deliberately narrow:
//
//   - The host part must be exactly "*". Partial wildcards ("*.example.com",
//     "mail.*"), a bare "*" without a port, "*:*", port ranges and CIDR remain
//     forbidden (they need a future contract version).
//   - The port must be explicit and belong to WildcardEgressPorts, a closed
//     allowlist of protocol ports (SMTP, SMTPS, submission, IMAP, IMAPS, POP3,
//     POP3S). Widening it is a kernel release, never a manifest decision.
//   - A wildcard entry does NOT lift the host's SSRF defences. Enforcers MUST
//     still refuse loopback, private, link-local and metadata destinations
//     (see package netguard) after resolving the name, exactly as for exact
//     entries; "any host" means "any PUBLIC host".
//
// Hostnames are strict ASCII DNS labels (no NUL, unicode, zone ids, empty
// labels); a trailing dot is normalized away. IP literals ("10.0.0.5",
// "169.254.169.254:25", "[::1]:25", numeric spellings), "*.localhost" and
// special-use suffixes (.local, .internal, .arpa, .svc, ...; see
// netguard.ForbiddenHostSuffixes) are rejected in every form: an entry names a
// public DNS name, and the destination address is always judged after
// resolution.
//
// The ONLY exception is the exact host-service grant "localhost:<port>" or
// "127.0.0.1:<port>" (explicit port, no wildcard, no range): a sidecar that
// must reach a service the host runs on its own loopback (published addons
// such as the storefront declare "localhost:8080"). It is a declaration, not
// an authorization: NetworkPolicy.DialContext still applies netguard and so
// never dials loopback; ops decides whether to honour these grants (see
// NetworkPolicy.LoopbackGrants) and the hub shows them for approval.
//
// # Approval and enforcement obligations
//
// "*:<port>" is a broad grant: the addon can reach any public host on that
// port, which is a data-exfiltration surface. The hub MUST require explicit
// (human) approval of every manifest that carries a wildcard egress entry, and
// ops MUST enforce NetworkPolicy before installing such an addon (for example
// the mail addon) -- never rely on the addon being well behaved. Enforcers
// should dial through NetworkPolicy.DialContext, which combines Allows with
// the netguard SSRF guard (private, loopback, link-local and cloud-metadata
// addresses such as 169.254.169.254 are refused after DNS resolution); a
// network-namespace / firewall rule is the defence in depth for sidecars that
// open sockets on their own.
//
// NetworkPolicy.Allows is the reference decision function hosts should use so
// every enforcer interprets the grammar the same way.

// WildcardEgressPorts is the closed set of ports on which "*:<port>" egress is
// admitted.
var WildcardEgressPorts = []int{25, 110, 143, 465, 587, 993, 995}

// EgressRule is a parsed Network.Egress entry.
type EgressRule struct {
	// Host is the exact hostname, lower-cased; empty when AnyHost.
	Host string
	// Port is the TCP port; 0 means "no port restriction" (host-only entry).
	Port int
	// AnyHost marks the "*:<port>" form.
	AnyHost bool
}

func (r EgressRule) String() string {
	h := r.Host
	if r.AnyHost {
		h = "*"
	}
	if r.Port == 0 {
		return h
	}
	return h + ":" + strconv.Itoa(r.Port)
}

var egressLabelRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// ParseEgress parses and validates one Network.Egress entry.
func ParseEgress(target string) (EgressRule, error) {
	if target == "" || len(target) > 300 {
		return EgressRule{}, fmt.Errorf("must be an exact hostname, hostname:port or *:port")
	}
	host, portStr, hasPort := strings.Cut(target, ":")
	if strings.Contains(portStr, ":") {
		return EgressRule{}, fmt.Errorf("%q: IPv6 literals and extra ':' are not supported", target)
	}
	rule := EgressRule{}
	if hasPort {
		p, err := strconv.Atoi(portStr)
		if err != nil || p < 1 || p > 65535 || strconv.Itoa(p) != portStr {
			return EgressRule{}, fmt.Errorf("%q: port must be an integer between 1 and 65535", target)
		}
		rule.Port = p
	}
	if strings.Contains(host, "*") {
		if host != "*" {
			return EgressRule{}, fmt.Errorf("%q: only the whole-host wildcard \"*:<port>\" is allowed, not partial wildcards", target)
		}
		if !hasPort {
			return EgressRule{}, fmt.Errorf("%q: a wildcard host requires an explicit port (\"*:<port>\")", target)
		}
		if !wildcardPortAllowed(rule.Port) {
			return EgressRule{}, fmt.Errorf("%q: wildcard egress is only admitted on ports %v", target, WildcardEgressPorts)
		}
		rule.AnyHost = true
		return rule, nil
	}
	h := strings.ToLower(strings.TrimSuffix(host, "."))
	if h == "" {
		return EgressRule{}, fmt.Errorf("%q: hostname is empty", target)
	}
	// Host-service grants: EXACTLY localhost:<port> or 127.0.0.1:<port>
	// (a sidecar talking to a service the host runs on its own loopback, e.g.
	// the storefront renderer's dev server). Never without a port, never a
	// wildcard or a range, never any other loopback spelling.
	if h == "localhost" || h == "127.0.0.1" {
		if !hasPort {
			return EgressRule{}, fmt.Errorf("%q: a loopback grant requires an explicit port (%s:<port>)", target, h)
		}
		rule.Host = h
		return rule, nil
	}
	if err := checkEgressHost(target, h); err != nil {
		return EgressRule{}, err
	}
	rule.Host = h
	return rule, nil
}

// IsLoopbackGrant reports whether the rule is one of the exact host-service
// grants (localhost:<port> / 127.0.0.1:<port>). NetworkPolicy.DialContext
// never dials them (netguard blocks loopback): the host decides whether to
// honour them, with its own dialer, via NetworkPolicy.LoopbackGrants.
func (r EgressRule) IsLoopbackGrant() bool {
	return !r.AnyHost && r.Port != 0 && (r.Host == "localhost" || r.Host == "127.0.0.1")
}

// checkEgressHost enforces the hostname grammar: ASCII DNS labels only (no NUL,
// unicode, zone ids, empty labels), no IP literal or numeric spelling of one,
// no localhost and no special-use suffix (.internal, .local, .arpa, ...).
func checkEgressHost(target, h string) error {
	if len(h) > 253 {
		return fmt.Errorf("%q: hostname too long", target)
	}
	if _, err := netip.ParseAddr(h); err == nil || strings.HasPrefix(h, "[") {
		return fmt.Errorf("%q: IP literals are not allowed, use a hostname", target)
	}
	labels := strings.Split(h, ".")
	for _, l := range labels {
		if !egressLabelRe.MatchString(l) {
			return fmt.Errorf("%q: %q is not a valid hostname label", target, l)
		}
	}
	last := labels[len(labels)-1]
	allNumeric := true
	for _, c := range last {
		if !(c >= '0' && c <= '9') && !(c == 'x' || (c >= 'a' && c <= 'f')) {
			allNumeric = false
			break
		}
	}
	// A TLD never starts with a digit: "2130706433", "0x7f.1", "1.2.3.4" are
	// IPv4 spellings.
	if allNumeric && last[0] >= '0' && last[0] <= '9' {
		return fmt.Errorf("%q: numeric hosts are not allowed, use a hostname", target)
	}
	if netguard.HasForbiddenSuffix(h) {
		return fmt.Errorf("%q: special-use names (.local, .internal, .arpa, localhost, ...) are not valid egress destinations", target)
	}
	return nil
}

// isIPOrLocal reports whether host (as seen at dial time) is an IP literal or
// a local name: such destinations never match a wildcard.
func isIPOrLocal(host string) bool {
	h := strings.ToLower(strings.TrimSuffix(host, "."))
	if i := strings.IndexByte(h, '%'); i >= 0 {
		return true // zone id
	}
	if _, err := netip.ParseAddr(h); err == nil {
		return true
	}
	return checkEgressHost(h, h) != nil
}

func wildcardPortAllowed(p int) bool {
	for _, a := range WildcardEgressPorts {
		if a == p {
			return true
		}
	}
	return false
}

// Allows reports whether the policy permits an outbound TCP connection to
// host:port. It implements ONLY the manifest grammar; callers must still apply
// the SSRF guard to the resolved address. An invalid entry never matches.
func (n NetworkPolicy) Allows(host string, port int) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	for _, entry := range n.Egress {
		r, err := ParseEgress(entry)
		if err != nil {
			continue
		}
		if r.Port != 0 && r.Port != port {
			continue
		}
		if r.AnyHost {
			// "any host" means any PUBLIC DNS name, never an IP or local name.
			if !isIPOrLocal(host) {
				return true
			}
			continue
		}
		if r.Host == host {
			return true
		}
	}
	return false
}

// ErrEgressDenied is returned by DialContext when the policy does not admit
// the destination.
var ErrEgressDenied = errors.New("native runtime: egress destination not allowed by network policy")

// LoopbackGrants returns the exact host-service grants (localhost:<port>,
// 127.0.0.1:<port>) the policy declares. DialContext deliberately does NOT
// dial them -- netguard refuses loopback -- so whether a sidecar may reach the
// host's own loopback is a decision of ops, which can dial these (and only
// these) with its own dialer.
func (n NetworkPolicy) LoopbackGrants() []EgressRule {
	var out []EgressRule
	for _, e := range n.Egress {
		if r, err := ParseEgress(e); err == nil && r.IsLoopbackGrant() {
			out = append(out, r)
		}
	}
	return out
}

// DialContext is the reference enforcement point: it dials address only if
// the policy Allows the (hostname, port) pair AND the resolved address passes
// the netguard SSRF guard (netguard.ErrBlockedEgress for loopback, private,
// link-local, CGNAT and cloud-metadata addresses such as 169.254.169.254).
// Only tcp networks and hostnames are admitted: an IP-literal destination is
// refused outright. Wire it as http.Transport.DialContext or hand it to any
// client that opens connections on behalf of the sidecar.
func (n NetworkPolicy) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	switch network {
	case "tcp", "tcp4", "tcp6":
	default:
		return nil, fmt.Errorf("%w: network %q", ErrEgressDenied, network)
	}
	host, portStr, err := net.SplitHostPort(address)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrEgressDenied, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return nil, fmt.Errorf("%w: bad port", ErrEgressDenied)
	}
	if _, err := netip.ParseAddr(strings.TrimSuffix(host, ".")); err == nil {
		return nil, fmt.Errorf("%w: IP literal %s", ErrEgressDenied, host)
	}
	if !n.Allows(host, port) {
		return nil, fmt.Errorf("%w: %s:%d", ErrEgressDenied, host, port)
	}
	d := net.Dialer{Control: netguard.DialControl}
	return d.DialContext(ctx, network, address)
}
