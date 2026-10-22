package domains

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sort"
	"strings"
	"time"
)

// ErrNoRecords is the AUTHORITATIVE "nothing there": NXDOMAIN, or the name
// exists but has no record of that type. Any other error from a DNSResolver
// (timeout, SERVFAIL, network) is indeterminate and must never be treated as
// "missing".
var ErrNoRecords = errors.New("domains: no records")

// DNSResolver looks records up. Implementations return canonical values:
//
//	TXT          each record's text, chunks joined, unquoted
//	MX           "<priority> <host>" (host lower-case, no trailing dot)
//	CNAME        the target host
//	A / AAAA     the address text
//
// and ErrNoRecords (possibly wrapped) when the answer is authoritatively empty.
// Hosts that expose DNS to addons (the wasm dns_resolve import) inject their
// own implementation behind this interface.
type DNSResolver interface {
	// Name labels the vantage point in diagnostics ("system", "1.1.1.1").
	Name() string
	Lookup(ctx context.Context, name string, t RecordType) ([]string, error)
}

// NetResolver is the stdlib-backed DNSResolver. With a server it queries that
// recursive resolver directly (use several public ones to observe
// propagation); without, the system resolver. Prefer NewPublicResolver for
// anything reachable from an addon: the system resolver may answer internal
// names.
type NetResolver struct {
	label string
	r     *net.Resolver
}

// NewNetResolver builds a resolver. server is "host:port" ("1.1.1.1:53") or ""
// for the system configuration.
func NewNetResolver(label, server string) *NetResolver {
	r := &net.Resolver{PreferGo: true}
	if server != "" {
		d := &net.Dialer{Timeout: 5 * time.Second}
		r.Dial = func(ctx context.Context, network, _ string) (net.Conn, error) {
			return d.DialContext(ctx, network, server)
		}
	}
	if label == "" {
		label = "system"
		if server != "" {
			label = server
		}
	}
	return &NetResolver{label: label, r: r}
}

// PublicResolverAddr is the recursive resolver NewPublicResolver queries.
const PublicResolverAddr = "1.1.1.1:53"

// NewPublicResolver is the RECOMMENDED resolver for exposing DNS to addons
// (wasm dns_resolve) and for ownership checks: it always asks a public
// recursive resolver, so internal / split-horizon names and the host's search
// domains are never consulted.
func NewPublicResolver() *NetResolver { return NewNetResolver("public", PublicResolverAddr) }

func (n *NetResolver) Name() string { return n.label }

func (n *NetResolver) Lookup(ctx context.Context, name string, t RecordType) ([]string, error) {
	name = strings.TrimSuffix(name, ".")
	var out []string
	var err error
	switch t {
	case TypeTXT:
		out, err = n.r.LookupTXT(ctx, name)
	case TypeMX:
		var mx []*net.MX
		if mx, err = n.r.LookupMX(ctx, name); err == nil {
			for _, m := range mx {
				out = append(out, fmt.Sprintf("%d %s", m.Pref, canonHost(m.Host)))
			}
		}
	case TypeCNAME:
		var c string
		if c, err = n.r.LookupCNAME(ctx, name); err == nil {
			// The stdlib answers with the name itself when there is no CNAME.
			if canonHost(c) == canonHost(name) {
				return nil, ErrNoRecords
			}
			out = []string{canonHost(c)}
		}
	case TypeA, TypeAAAA:
		network := "ip4"
		if t == TypeAAAA {
			network = "ip6"
		}
		var ips []net.IP
		if ips, err = n.r.LookupIP(ctx, network, name); err == nil {
			for _, ip := range ips {
				out = append(out, ip.String())
			}
		}
	default:
		return nil, fmt.Errorf("domains: unsupported record type %q", t)
	}
	if err != nil {
		var de *net.DNSError
		if errors.As(err, &de) && de.IsNotFound {
			return nil, ErrNoRecords
		}
		return nil, err
	}
	if len(out) == 0 {
		return nil, ErrNoRecords
	}
	sort.Strings(out)
	return out, nil
}
