package native

import (
	"fmt"
	"strconv"
	"strings"
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
	rule.Host = strings.ToLower(host)
	return rule, nil
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
