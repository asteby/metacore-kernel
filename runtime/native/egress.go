package native

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
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
// IP literals ("10.0.0.5", "169.254.169.254:25", "[::1]:25") and "localhost"
// (or *.localhost) are never valid hosts in ANY form: an entry names a public
// DNS name, and the destination address is always judged after resolution.
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

// ParseEgress parses and validates one Network.Egress entry.
func ParseEgress(target string) (EgressRule, error) {
	if target == "" || strings.ContainsAny(target, "/ \t\r\n") {
		return EgressRule{}, fmt.Errorf("must be an exact hostname, hostname:port or *:port")
	}
	host, portStr, hasPort := strings.Cut(target, ":")
	if strings.HasPrefix(host, "[") || strings.HasSuffix(host, "]") {
		return EgressRule{}, fmt.Errorf("%q: IP literals are not allowed, use a hostname", target)
	}
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
	if host == "" {
		return EgressRule{}, fmt.Errorf("%q: hostname is empty", target)
	}
	if err := rejectIPOrLocalHost(target, host); err != nil {
		return EgressRule{}, err
	}
	rule.Host = strings.ToLower(host)
	return rule, nil
}

// rejectIPOrLocalHost refuses IP literals (including decimal/hex/octal-looking
// IPv4 forms whose last label is numeric) and localhost names.
func rejectIPOrLocalHost(target, host string) error {
	h := strings.ToLower(strings.TrimSuffix(host, "."))
	if _, err := netip.ParseAddr(h); err == nil {
		return fmt.Errorf("%q: IP literals are not allowed, use a hostname", target)
	}
	if h == "localhost" || strings.HasSuffix(h, ".localhost") {
		return fmt.Errorf("%q: localhost is not a valid egress destination", target)
	}
	last := h[strings.LastIndexByte(h, '.')+1:]
	numeric := last != ""
	for _, c := range last {
		if !(c >= '0' && c <= '9') && !(c == 'x' || (c >= 'a' && c <= 'f')) {
			numeric = false
			break
		}
	}
	// A TLD is never all digits; "0x7f.1", "2130706433" are IPv4 spellings.
	if numeric && (last[0] >= '0' && last[0] <= '9') {
		return fmt.Errorf("%q: numeric hosts are not allowed, use a hostname", target)
	}
	return nil
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
		if r.AnyHost || r.Host == host {
			return true
		}
	}
	return false
}

// ErrEgressDenied is returned by DialContext when the policy does not admit
// the destination.
var ErrEgressDenied = errors.New("native runtime: egress destination not allowed by network policy")

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
	if rejectIPOrLocalHost(address, host) != nil {
		return nil, fmt.Errorf("%w: %s", ErrEgressDenied, host)
	}
	if !n.Allows(host, port) {
		return nil, fmt.Errorf("%w: %s:%d", ErrEgressDenied, host, port)
	}
	d := net.Dialer{Control: netguard.DialControl}
	return d.DialContext(ctx, network, address)
}
