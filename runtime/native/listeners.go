package native

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
)

// TCP listeners.
//
// Spec.Ingress routes public HTTP through the host's reverse proxy (and a
// non-nil Spec.Ingress ALWAYS means "this service has HTTP ingress"). Some
// services speak other TCP protocols directly to the internet (a mail server
// on 25/465/587/993, for instance). Spec.Listeners declares those public TCP
// ports so the host can open, forward and audit them. They live on Spec, not
// inside Ingress, so Ingress stays a comparable value and "Ingress != nil"
// keeps meaning HTTP. The contract is host-neutral and least-privilege:
//
//   - The addon declares only a logical name and the PUBLIC port it needs.
//     The host decides whether the port may be opened at all and may refuse
//     the install or mark the service blocked. Ports of well-known sensitive
//     services (SSH, HTTP/S, databases, ... see ForbiddenListenerPorts) are
//     never admitted, only mail-protocol ports are admitted by default
//     (AllowedListenerPorts) and any other port needs explicit hub approval
//     (Spec.ValidateWithApprovedPorts), and two services may never claim the
//     same public port (ListenerRegistry).
//   - The sidecar never binds the public port, and never a TCP port at all.
//     The supervisor assigns, per listener, a UNIX SOCKET inside the
//     host-private service directory (same directory, ownership and mode as
//     the control and ingress sockets, so only the supervisor user can dial
//     it) and exports its path as the env var returned by ListenerEnv(name).
//     The sidecar listens on that socket; a host-side L4 forwarder (systemd
//     socket, userspace proxy) accepts on the public port and pipes to it.
//     Because there is no shared loopback port, another local process or
//     addon cannot connect to the listener nor forge a PROXY header.
//   - proxy_protocol makes the forwarder prepend a HAProxy PROXY header
//     ("v1" text or "v2" binary) so the sidecar still learns the real client
//     address (indispensable for SPF/greylist/abuse decisions). The sidecar
//     MUST then expect the header on that listener. Since only the forwarder
//     can reach the unix socket, the header can be trusted.
//   - Listeners never imply an http health route; Spec.Health still applies.
//
// A service with no Listeners (and Ingress nil) opens no public port.

const (
	// EnvListenerPrefix prefixes the supervisor-assigned unix socket env var
	// of each declared listener: METACORE_NATIVE_LISTENER_<NAME> (name
	// upper-cased) = absolute socket path. Reserved: manifests cannot set it.
	EnvListenerPrefix = "METACORE_NATIVE_LISTENER_"

	ProxyProtocolNone = ""
	ProxyProtocolV1   = "v1"
	ProxyProtocolV2   = "v2"

	MaxListeners = 16
)

// AllowedListenerPorts is the closed allowlist of public ports a native
// service may declare WITHOUT special approval: the mail protocols (SMTP 25,
// POP3 110, IMAP 143, SMTPS 465, submission 587, IMAPS 993, POP3S 995,
// ManageSieve 4190). Any other port is refused by Validate and needs an
// explicit, recorded hub approval (ValidateWithApprovedPorts). Widening the
// default list is a kernel release.
var AllowedListenerPorts = []int{25, 110, 143, 465, 587, 993, 995, 4190}

// ForbiddenListenerPorts are never admitted, not even with hub approval: they
// belong to the host's own infrastructure or to well-known sensitive services
// (remote shell, web edge, DNS, databases, caches, container/orchestration
// control planes). It is a backstop for approvals, not the access rule (that
// is the allowlist).
var ForbiddenListenerPorts = map[int]string{
	21: "ftp", 22: "ssh", 23: "telnet", 53: "dns", 80: "http", 111: "rpcbind",
	135: "msrpc", 139: "netbios", 443: "https", 445: "smb", 2049: "nfs",
	2375: "docker", 2376: "docker-tls", 2379: "etcd", 2380: "etcd-peer",
	3306: "mysql", 3389: "rdp", 5432: "postgres", 5984: "couchdb",
	6379: "redis", 6443: "kubernetes-api", 8080: "http-alt", 8443: "https-alt",
	9200: "elasticsearch", 10250: "kubelet", 11211: "memcached", 27017: "mongodb",
}

func listenerPortAllowed(p int, approved []int) bool {
	for _, a := range AllowedListenerPorts {
		if a == p {
			return true
		}
	}
	for _, a := range approved {
		if a == p {
			return true
		}
	}
	return false
}

var listenerNamePattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)

// Listener is one public TCP port a native service asks the host to expose.
type Listener struct {
	// Name is the logical id (smtp, submission, imaps), unique per service.
	Name string `json:"name"`
	// Port is the PUBLIC TCP port (1-65535).
	Port int `json:"port"`
	// ProxyProtocol is "", "v1" or "v2".
	ProxyProtocol string `json:"proxy_protocol,omitempty"`
}

// ListenerEnv returns the env var name carrying the bind address for name.
func ListenerEnv(name string) string {
	return EnvListenerPrefix + strings.ToUpper(name)
}

func validateListeners(ls []Listener, approved ...int) error {
	if len(ls) > MaxListeners {
		return fmt.Errorf("native runtime: listeners allows at most %d entries", MaxListeners)
	}
	names := map[string]struct{}{}
	ports := map[int]struct{}{}
	for i, l := range ls {
		if !listenerNamePattern.MatchString(l.Name) {
			return fmt.Errorf("native runtime: listeners[%d].name %q must match %s", i, l.Name, listenerNamePattern)
		}
		if l.Port < 1 || l.Port > 65535 {
			return fmt.Errorf("native runtime: listeners[%d].port must be between 1 and 65535", i)
		}
		if !listenerPortAllowed(l.Port, approved) {
			return fmt.Errorf("native runtime: listeners[%d].port %d is not a mail port %v and has no hub approval", i, l.Port, AllowedListenerPorts)
		}
		if svc, banned := ForbiddenListenerPorts[l.Port]; banned {
			return fmt.Errorf("native runtime: listeners[%d].port %d is reserved for %s and cannot be exposed by a native service", i, l.Port, svc)
		}
		switch l.ProxyProtocol {
		case ProxyProtocolNone, ProxyProtocolV1, ProxyProtocolV2:
		default:
			return fmt.Errorf("native runtime: listeners[%d].proxy_protocol %q must be \"v1\", \"v2\" or empty", i, l.ProxyProtocol)
		}
		if _, dup := names[l.Name]; dup {
			return fmt.Errorf("native runtime: duplicate listener name %q", l.Name)
		}
		if _, dup := ports[l.Port]; dup {
			return fmt.Errorf("native runtime: duplicate listener port %d", l.Port)
		}
		names[l.Name] = struct{}{}
		ports[l.Port] = struct{}{}
	}
	return nil
}

// ListenerRegistry detects public-port collisions between services. A host
// (supervisor / installer) keeps one registry per machine and Claims the
// listeners of every service before opening anything; a port already owned by
// a different owner is refused. Owners are opaque strings, typically the
// installation id. It is safe for concurrent use.
type ListenerRegistry struct {
	mu    sync.Mutex
	ports map[int]string
}

// NewListenerRegistry returns an empty registry.
func NewListenerRegistry() *ListenerRegistry {
	return &ListenerRegistry{ports: map[int]string{}}
}

// Claim atomically reserves every listener port for owner. It validates the
// listeners (including forbidden ports) and fails, claiming nothing, if any
// port is held by another owner. Re-claiming for the same owner replaces its
// previous reservation (an upgrade that changes ports).
func (r *ListenerRegistry) Claim(owner string, ls []Listener) error {
	return r.ClaimApproved(owner, ls)
}

// ClaimApproved is Claim for listeners whose non-mail ports were explicitly
// approved by the hub (never a ForbiddenListenerPorts port).
func (r *ListenerRegistry) ClaimApproved(owner string, ls []Listener, approved ...int) error {
	if owner == "" {
		return errors.New("native runtime: listener owner is required")
	}
	if err := validateListeners(ls, approved...); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, l := range ls {
		if cur, taken := r.ports[l.Port]; taken && cur != owner {
			return fmt.Errorf("native runtime: listener port %d is already claimed by %q", l.Port, cur)
		}
	}
	for p, o := range r.ports {
		if o == owner {
			delete(r.ports, p)
		}
	}
	for _, l := range ls {
		r.ports[l.Port] = owner
	}
	return nil
}

// Release frees every port held by owner.
func (r *ListenerRegistry) Release(owner string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for p, o := range r.ports {
		if o == owner {
			delete(r.ports, p)
		}
	}
}
