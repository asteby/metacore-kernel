package native

import (
	"fmt"
	"regexp"
	"strings"
)

// TCP listeners.
//
// Ingress.Kind "http" routes public HTTP through the host's reverse proxy.
// Some services speak other TCP protocols directly to the internet (a mail
// server on 25/465/587/993, for instance). Ingress.Listeners declares those
// public TCP ports so the host can open, forward and audit them. The contract
// stays host-neutral and least-privilege:
//
//   - The addon declares only a logical name and the PUBLIC port it needs.
//     The host decides whether the port may be opened at all (port conflicts
//     with other services on the machine, firewall, tenant limits) and may
//     refuse the install or mark the service blocked.
//   - The sidecar never binds the public port. Rootless sidecars cannot bind
//     ports < 1024 anyway. The supervisor assigns, per listener, a loopback
//     address the sidecar must bind and exports it as the env var returned by
//     ListenerEnv(name); a host-side L4 forwarder (systemd socket, nftables
//     redirect, userspace proxy) maps the public port to it.
//   - proxy_protocol makes the forwarder prepend a HAProxy PROXY header
//     ("v1" text or "v2" binary) so the sidecar still learns the real client
//     address (indispensable for SPF/greylist/abuse decisions). The sidecar
//     MUST then expect the header on that listener, and the listener must be
//     reachable only from the forwarder (loopback), never directly.
//   - Listeners never imply an http health route; Spec.Health still applies.
//
// A kind "tcp" ingress carries ONLY listeners; a kind "http" ingress may carry
// them too. A service with Ingress nil opens no public port of any kind.

const (
	IngressKindTCP = "tcp"

	// EnvListenerPrefix prefixes the supervisor-assigned bind address env var
	// of each declared listener: METACORE_NATIVE_LISTENER_<NAME> (name
	// upper-cased) = "127.0.0.1:<port>". Reserved: manifests cannot set it.
	EnvListenerPrefix = "METACORE_NATIVE_LISTENER_"

	ProxyProtocolNone = ""
	ProxyProtocolV1   = "v1"
	ProxyProtocolV2   = "v2"

	MaxListeners = 16
)

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

func validateListeners(ls []Listener) error {
	if len(ls) > MaxListeners {
		return fmt.Errorf("native runtime: ingress.listeners allows at most %d entries", MaxListeners)
	}
	names := map[string]struct{}{}
	ports := map[int]struct{}{}
	for i, l := range ls {
		if !listenerNamePattern.MatchString(l.Name) {
			return fmt.Errorf("native runtime: ingress.listeners[%d].name %q must match %s", i, l.Name, listenerNamePattern)
		}
		if l.Port < 1 || l.Port > 65535 {
			return fmt.Errorf("native runtime: ingress.listeners[%d].port must be between 1 and 65535", i)
		}
		switch l.ProxyProtocol {
		case ProxyProtocolNone, ProxyProtocolV1, ProxyProtocolV2:
		default:
			return fmt.Errorf("native runtime: ingress.listeners[%d].proxy_protocol %q must be \"v1\", \"v2\" or empty", i, l.ProxyProtocol)
		}
		if _, dup := names[l.Name]; dup {
			return fmt.Errorf("native runtime: duplicate ingress listener name %q", l.Name)
		}
		if _, dup := ports[l.Port]; dup {
			return fmt.Errorf("native runtime: duplicate ingress listener port %d", l.Port)
		}
		names[l.Name] = struct{}{}
		ports[l.Port] = struct{}{}
	}
	return nil
}
