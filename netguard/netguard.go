// Package netguard es la fuente única de la regla de salida (egress) del
// kernel: qué destinos de red puede alcanzar código que descarga por
// instrucción de un tenant, de un addon o de un archivo importado.
//
// La defensa real vive en DialControl, que corre DESPUÉS de resolver DNS y
// sobre la dirección exacta a la que se conecta el socket. Por eso cubre
// DNS rebinding, redirecciones y formas exóticas de escribir una IP
// (127.0.0.2, [::ffff:127.0.0.1], etc.). Las comprobaciones sobre el texto del
// host son solo una primera barrera barata.
package netguard

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"syscall"
	"time"
)

var (
	// ErrBlockedEgress se devuelve cuando el destino es interno o privado.
	ErrBlockedEgress = errors.New("netguard: destination not allowed")
	// ErrHostNotAllowed se devuelve cuando el host no está en AllowHosts.
	ErrHostNotAllowed = errors.New("netguard: host not in allowlist")
	// ErrTooManyRedirects se devuelve al exceder Options.MaxRedirects.
	ErrTooManyRedirects = errors.New("netguard: too many redirects")
)

// IsBlockedIP indica si ip es un destino que nunca debe alcanzarse: loopback
// (todo 127/8 y ::1), redes privadas, no especificada, link-local (incluye el
// metadata de nube 169.254.169.254), multicast, CGNAT 100.64/10, 0.0.0.0/8,
// rangos reservados y las formas IPv6 que incrustan una IPv4 (mapeada
// ::ffff:a.b.c.d, NAT64 64:ff9b::/96 y 6to4 2002::/16), que se evalúan por la
// IPv4 que llevan dentro. Una IP nil se bloquea.
func IsBlockedIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	if v4 := ip.To4(); v4 != nil {
		return blockedV4(v4)
	}
	ip = ip.To16()
	if ip == nil {
		return true
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsMulticast() {
		return true
	}
	// NAT64 64:ff9b::/96: la IPv4 va en los últimos 4 bytes.
	if ip[0] == 0x00 && ip[1] == 0x64 && ip[2] == 0xff && ip[3] == 0x9b && allZero(ip[4:12]) {
		return blockedV4(ip[12:16])
	}
	// 6to4 2002::/16: la IPv4 va en los bytes 2..5.
	if ip[0] == 0x20 && ip[1] == 0x02 {
		return blockedV4(ip[2:6])
	}
	// IPv4 "compatible" (::a.b.c.d, obsoleta): primeros 12 bytes en cero.
	if allZero(ip[:12]) {
		return true
	}
	return false
}

func allZero(b []byte) bool {
	for _, x := range b {
		if x != 0 {
			return false
		}
	}
	return true
}

func blockedV4(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsMulticast() {
		return true
	}
	switch {
	case ip[0] == 0: // 0.0.0.0/8
		return true
	case ip[0] == 100 && ip[1]&0xC0 == 64: // CGNAT 100.64.0.0/10
		return true
	case ip[0] == 192 && ip[1] == 0 && ip[2] == 0: // 192.0.0.0/24
		return true
	case ip[0] == 198 && ip[1]&0xFE == 18: // 198.18.0.0/15
		return true
	case ip[0] >= 240: // 240.0.0.0/4 reservado y broadcast
		return true
	}
	return false
}

// DialControl se engancha a net.Dialer.Control: corre tras la resolución DNS,
// sobre la dirección exacta que se va a marcar.
func DialControl(network, address string, c syscall.RawConn) error {
	return dialControl(false)(network, address, c)
}

func dialControl(allowPrivate bool) func(string, string, syscall.RawConn) error {
	return func(_, address string, _ syscall.RawConn) error {
		if allowPrivate {
			return nil
		}
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return fmt.Errorf("%w: %s", ErrBlockedEgress, address)
		}
		if i := strings.IndexByte(host, '%'); i >= 0 { // zona IPv6
			host = host[:i]
		}
		if IsBlockedIP(net.ParseIP(host)) {
			return fmt.Errorf("%w: %s", ErrBlockedEgress, host)
		}
		return nil
	}
}

// Options configura NewClient. Los valores en cero son seguros.
type Options struct {
	// Timeout por intento (conexión, cabeceras y cuerpo). Por defecto 15 s.
	Timeout time.Duration
	// AllowHosts: patrones exactos o "*.dominio.tld". Vacío = cualquier host
	// público. Si se da una lista cuyos patrones son todos inválidos ("*",
	// "*.com") no se acepta ningún host: nunca se abre por error de captura.
	AllowHosts []string
	// MaxRedirects por petición. Por defecto 3. Cada salto revalida el host
	// contra AllowHosts y la IP en DialControl.
	MaxRedirects int
	// AllowPrivate desactiva la guarda de IP. SOLO para pruebas.
	AllowPrivate bool
}

// NewClient devuelve un http.Client para descargas iniciadas por un tenant:
// ignora proxies del entorno (para que la guarda vea el destino real),
// comprueba cada conexión con DialControl y cada redirección con AllowHosts.
func NewClient(o Options) *http.Client {
	return newClient(o, dialControl(o.AllowPrivate))
}

// newClient is NewClient with an injectable dial control (tests only).
func newClient(o Options, control func(string, string, syscall.RawConn) error) *http.Client {
	if o.Timeout <= 0 {
		o.Timeout = 15 * time.Second
	}
	if o.MaxRedirects <= 0 {
		o.MaxRedirects = 3
	}
	dialer := &net.Dialer{Timeout: 10 * time.Second, Control: control}
	transport := &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return dialer.DialContext(ctx, network, addr)
		},
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: o.Timeout,
		MaxIdleConns:          10,
		IdleConnTimeout:       30 * time.Second,
	}
	allow := append([]string(nil), o.AllowHosts...)
	return &http.Client{
		Timeout:   o.Timeout,
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) > o.MaxRedirects {
				return ErrTooManyRedirects
			}
			if len(allow) > 0 && !HostAllowed(allow, strings.ToLower(req.URL.Host)) {
				return fmt.Errorf("%w: %s", ErrHostNotAllowed, req.URL.Host)
			}
			return nil
		},
	}
}

// HostAllowed indica si host (en minúsculas, con puerto si lo lleva) coincide
// con algún patrón válido. Un patrón debe resolver a un dominio concreto con
// TLD: "*", "*.*" y "*.com" se ignoran porque anularían la política. Una lista
// vacía no permite nada: quien quiera "cualquier host público" no llama aquí.
func HostAllowed(patterns []string, host string) bool {
	for _, p := range patterns {
		if !IsValidHostPattern(p) {
			continue
		}
		if p == host {
			return true
		}
		if strings.HasPrefix(p, "*.") {
			suffix := strings.TrimPrefix(p, "*.")
			if strings.HasSuffix(host, "."+suffix) || host == suffix {
				return true
			}
		}
	}
	return false
}

// IsValidHostPattern exige un dominio registrable (al menos dos etiquetas no
// vacías) y que el comodín, si existe, sea solo la etiqueta más a la izquierda.
func IsValidHostPattern(p string) bool {
	if p == "" || p == "*" {
		return false
	}
	host := p
	if i := strings.LastIndex(host, ":"); i >= 0 && !strings.Contains(host[i+1:], ".") {
		host = host[:i]
	}
	host = strings.TrimPrefix(host, "*.")
	if strings.Contains(host, "*") {
		return false
	}
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

// ForbiddenHostSuffixes are special-use / private name spaces (RFC 6761, 6762,
// 8375, Kubernetes, mDNS, reverse zones) that no addon-facing policy (native
// egress grammar, dns_resolve) may name, even under a domain the tenant owns.
var ForbiddenHostSuffixes = []string{
	"local", "localhost", "internal", "arpa", "cluster.local", "svc", "lan",
	"home", "corp", "intranet", "private",
}

// HasForbiddenSuffix reports whether name (lower-case, no trailing dot) equals
// or sits under one of ForbiddenHostSuffixes.
func HasForbiddenSuffix(name string) bool {
	name = strings.ToLower(strings.TrimSuffix(name, "."))
	for _, suf := range ForbiddenHostSuffixes {
		if name == suf || strings.HasSuffix(name, "."+suf) {
			return true
		}
	}
	return false
}
