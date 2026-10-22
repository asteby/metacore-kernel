package native

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"path"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Ingress plane: optional public HTTP traffic routed by the host to a native
// service (for example a server-side storefront renderer). It is the third
// leg of the metacore.native/v1 runtime, next to operations (host -> sidecar
// RPC on EnvSocket) and events (sidecar -> host on EnvEventSocket).
//
// # Transport
//
// When Spec.Ingress is set, the supervisor assigns a THIRD unix socket per
// service, exported to the sidecar as EnvIngressSocket. The sidecar listens on
// it and serves plain HTTP/1.1. It never binds a TCP port, and manifests can
// neither choose nor see host paths. The socket lives in the same
// host-private directory as the control socket, so only the host can dial it.
//
// # Routing authority
//
// The addon never decides which public hosts reach it. The host owns the
// mapping from public Host header (a verified tenant domain, a platform
// subdomain) to a service, and forwards only requests whose path is inside
// Ingress.PathPrefix. The path is forwarded unchanged (no prefix stripping)
// and the original Host header is preserved so the sidecar can render for it.
//
// # Header discipline
//
// IngressProxy is the host-neutral reverse proxy every host reuses. It:
//   - strips hop-by-hop headers in both directions (Connection, Keep-Alive,
//     TE, Trailer, Transfer-Encoding, Upgrade, Proxy-*, and anything listed in
//     Connection) and refuses protocol upgrades (501);
//   - drops client-supplied Forwarded, X-Forwarded-*, X-Real-Ip,
//     X-Request-Id, every X-Metacore-* header and every header the host
//     declares as trusted (IngressProxyOptions.TrustedHeaders);
//   - sets X-Forwarded-For (client IP as seen by the host, never appended to
//     a client value), X-Forwarded-Host and X-Forwarded-Proto, plus a
//     host-authored X-Request-Id that is echoed on the response;
//   - lets the host add trusted headers (e.g. X-Storefront-Domain) through
//     SetTrustedHeaders, after the client copies were removed.
//
// # Limits and errors
//
// Request bodies are capped at the effective MaxBodyBytes (413). The upstream
// must produce response headers within the effective Timeout (504); once
// headers arrive the body is streamed and flushed immediately, bounded only by
// the host server's own write timeouts and client disconnects. A dial or
// transport failure maps to 502. A path outside the prefix is 404.
//
// # Health
//
// A host must route ingress traffic to a service only while its Status is
// healthy (Status.AcceptsIngress); otherwise it answers 503 itself without
// dialing the socket.
const (
	IngressKindHTTP = "http"

	// EnvIngressSocket is the supervisor-assigned unix socket on which a
	// service with Spec.Ingress serves plain HTTP/1.1. Reserved like
	// EnvSocket: manifests cannot set or override it.
	EnvIngressSocket = "METACORE_NATIVE_INGRESS_SOCKET"

	DefaultIngressPathPrefix         = "/"
	DefaultIngressMaxBodyBytes int64 = 10 << 20
	MaxIngressMaxBodyBytes     int64 = 64 << 20
	DefaultIngressTimeout            = 30 * time.Second
	MaxIngressTimeout                = 5 * time.Minute

	HeaderRequestID      = "X-Request-Id"
	HeaderForwardedFor   = "X-Forwarded-For"
	HeaderForwardedHost  = "X-Forwarded-Host"
	HeaderForwardedProto = "X-Forwarded-Proto"

	// ReservedHeaderPrefix marks headers only the host may author. Any
	// client-supplied header with this prefix is dropped by IngressProxy.
	ReservedHeaderPrefix = "X-Metacore-"
)

var ingressPathPattern = regexp.MustCompile(`^/[A-Za-z0-9._~/-]*$`)

// Ingress declares that a native service accepts host-routed public HTTP
// traffic. Only the path prefix, body cap and header timeout are declarable;
// the public hosts that route to the service are always chosen by the host.
type Ingress struct {
	Kind         string `json:"kind"`
	PathPrefix   string `json:"path_prefix,omitempty"`
	MaxBodyBytes int64  `json:"max_body_bytes,omitempty"`
	Timeout      string `json:"timeout,omitempty"`
	// Listeners are public TCP ports the host forwards to the service (see
	// listeners.go). Optional for kind "http", required for kind "tcp".
	Listeners []Listener `json:"listeners,omitempty"`
}

// HTTPEnabled reports whether the host should mount the HTTP ingress proxy.
func (i Ingress) HTTPEnabled() bool { return i.Kind == IngressKindHTTP }

// EffectiveIngress is an Ingress with defaults applied.
type EffectiveIngress struct {
	PathPrefix   string
	MaxBodyBytes int64
	Timeout      time.Duration
}

func (i Ingress) Validate() error {
	if i.Kind != IngressKindHTTP && i.Kind != IngressKindTCP {
		return fmt.Errorf("native runtime: ingress.kind %q must be %q or %q", i.Kind, IngressKindHTTP, IngressKindTCP)
	}
	if err := validateListeners(i.Listeners); err != nil {
		return err
	}
	if i.Kind == IngressKindTCP {
		if len(i.Listeners) == 0 {
			return fmt.Errorf("native runtime: ingress.kind %q requires at least one listener", IngressKindTCP)
		}
		if i.PathPrefix != "" || i.MaxBodyBytes != 0 || i.Timeout != "" {
			return fmt.Errorf("native runtime: ingress.kind %q cannot set path_prefix, max_body_bytes or timeout", IngressKindTCP)
		}
		return nil
	}
	if i.PathPrefix != "" {
		if !ingressPathPattern.MatchString(i.PathPrefix) || path.Clean(i.PathPrefix) != i.PathPrefix || strings.Contains(i.PathPrefix, "..") {
			return fmt.Errorf("native runtime: ingress.path_prefix %q must be a clean absolute URL path", i.PathPrefix)
		}
	}
	if i.MaxBodyBytes < 0 || i.MaxBodyBytes > MaxIngressMaxBodyBytes {
		return fmt.Errorf("native runtime: ingress.max_body_bytes must be between 0 (default %d) and %d", DefaultIngressMaxBodyBytes, MaxIngressMaxBodyBytes)
	}
	if i.Timeout != "" {
		d, err := time.ParseDuration(i.Timeout)
		if err != nil || d <= 0 || d > MaxIngressTimeout {
			return fmt.Errorf("native runtime: ingress.timeout must be a positive duration up to %s", MaxIngressTimeout)
		}
	}
	return nil
}

// Effective applies defaults. Call it only on a validated Ingress.
func (i Ingress) Effective() EffectiveIngress {
	out := EffectiveIngress{PathPrefix: i.PathPrefix, MaxBodyBytes: i.MaxBodyBytes, Timeout: DefaultIngressTimeout}
	if out.PathPrefix == "" {
		out.PathPrefix = DefaultIngressPathPrefix
	}
	if out.MaxBodyBytes == 0 {
		out.MaxBodyBytes = DefaultIngressMaxBodyBytes
	}
	if d, err := time.ParseDuration(i.Timeout); err == nil && d > 0 {
		out.Timeout = d
	}
	return out
}

// Matches reports whether an URL path is inside the prefix on a segment
// boundary ("/shop" matches "/shop" and "/shop/x", never "/shopping").
func (e EffectiveIngress) Matches(urlPath string) bool {
	if e.PathPrefix == "/" {
		return strings.HasPrefix(urlPath, "/")
	}
	return urlPath == e.PathPrefix || strings.HasPrefix(urlPath, e.PathPrefix+"/")
}

// AcceptsIngress reports whether a host may route public traffic to a service
// in this state. No ingress without health.
func (s Status) AcceptsIngress() bool { return s.State == StateHealthy }

// IngressProxyOptions are host hooks. All are optional.
type IngressProxyOptions struct {
	// TrustedHeaders are header names only the host may author (for
	// example "X-Storefront-Domain"). Client copies are always removed
	// before SetTrustedHeaders runs.
	TrustedHeaders []string
	// SetTrustedHeaders adds host-authored headers to the outbound request.
	SetTrustedHeaders func(in *http.Request, out http.Header)
	// ClientIP returns the client address for X-Forwarded-For. Defaults to
	// the IP in RemoteAddr; hosts behind a trusted load balancer override it.
	ClientIP func(*http.Request) string
	// Scheme returns "https" or "http" for X-Forwarded-Proto. Defaults to
	// "https" when the request arrived over TLS.
	Scheme func(*http.Request) string
	// RequestID returns the id for X-Request-Id. Defaults to 128 random bits.
	RequestID func(*http.Request) string
	// Transport overrides the unix-socket transport (tests, instrumentation).
	Transport http.RoundTripper
	ErrorLog  *log.Logger
}

// IngressProxy is an http.Handler that forwards public requests to a native
// service's ingress unix socket with the header discipline described above.
type IngressProxy struct {
	cfg  EffectiveIngress
	opts IngressProxyOptions
	rp   *httputil.ReverseProxy
}

type ingressCtxKey struct{}

type ingressReqState struct {
	id string

	mu    sync.Mutex
	timer *time.Timer
}

// arm starts the response-header timer once. It runs when the request body has
// been fully sent (or immediately when there is none), so a slow upload of a
// large body is not charged against the time the sidecar takes to answer.
func (s *ingressReqState) arm(d time.Duration, cancel func(error)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.timer != nil {
		return
	}
	s.timer = time.AfterFunc(d, func() { cancel(errIngressTimeout) })
}

// stop disarms the timer and reports whether it had already fired.
func (s *ingressReqState) stop() (fired bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.timer == nil {
		return false
	}
	return !s.timer.Stop()
}

// armOnDone calls onDone once the wrapped body reaches EOF or fails.
type armOnDone struct {
	io.ReadCloser
	once   sync.Once
	onDone func()
}

func (b *armOnDone) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err != nil {
		b.once.Do(b.onDone)
	}
	return n, err
}

var errIngressTimeout = errors.New("native ingress: upstream response header timeout")

// NewIngressProxy builds a proxy for one service. socketPath is the
// supervisor-assigned EnvIngressSocket path.
func NewIngressProxy(socketPath string, ingress Ingress, opts IngressProxyOptions) (*IngressProxy, error) {
	if err := ingress.Validate(); err != nil {
		return nil, err
	}
	if !ingress.HTTPEnabled() {
		return nil, fmt.Errorf("native ingress: kind %q has no HTTP ingress to proxy", ingress.Kind)
	}
	if opts.Transport == nil {
		if socketPath == "" {
			return nil, errors.New("native ingress: socket path is required")
		}
		opts.Transport = &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", socketPath)
			},
			MaxIdleConns:        64,
			MaxIdleConnsPerHost: 64,
			IdleConnTimeout:     90 * time.Second,
			DisableCompression:  true,
		}
	}
	p := &IngressProxy{cfg: ingress.Effective(), opts: opts}
	trusted := make([]string, 0, len(opts.TrustedHeaders))
	for _, h := range opts.TrustedHeaders {
		trusted = append(trusted, http.CanonicalHeaderKey(h))
	}
	p.rp = &httputil.ReverseProxy{
		Transport:     opts.Transport,
		FlushInterval: -1,
		ErrorLog:      opts.ErrorLog,
		Rewrite: func(pr *httputil.ProxyRequest) {
			st, _ := pr.In.Context().Value(ingressCtxKey{}).(*ingressReqState)
			pr.Out.URL.Scheme = "http"
			pr.Out.URL.Host = "native-ingress"
			pr.Out.Host = pr.In.Host
			h := pr.Out.Header
			for name := range h {
				if strings.HasPrefix(name, ReservedHeaderPrefix) {
					h.Del(name)
				}
			}
			for _, name := range append([]string{"Forwarded", "X-Real-Ip", HeaderRequestID, HeaderForwardedFor, HeaderForwardedHost, HeaderForwardedProto}, trusted...) {
				h.Del(name)
			}
			h.Set(HeaderForwardedFor, p.clientIP(pr.In))
			h.Set(HeaderForwardedHost, pr.In.Host)
			h.Set(HeaderForwardedProto, p.scheme(pr.In))
			if st != nil {
				h.Set(HeaderRequestID, st.id)
			}
			if opts.SetTrustedHeaders != nil {
				opts.SetTrustedHeaders(pr.In, h)
			}
		},
		ModifyResponse: func(resp *http.Response) error {
			if st, ok := resp.Request.Context().Value(ingressCtxKey{}).(*ingressReqState); ok {
				// The timer fired between the sidecar answering and now: the
				// request context is already cancelled, so answer 504 instead of
				// cutting the response off after its headers.
				if st.stop() {
					return errIngressTimeout
				}
				resp.Header.Set(HeaderRequestID, st.id)
			}
			return nil
		},
		ErrorHandler: p.handleError,
	}
	return p, nil
}

// Ingress returns the effective configuration the proxy enforces.
func (p *IngressProxy) Ingress() EffectiveIngress { return p.cfg }

// CloseIdleConnections drops pooled upstream connections (call on Stop or
// when the service's socket is replaced).
func (p *IngressProxy) CloseIdleConnections() {
	if t, ok := p.opts.Transport.(interface{ CloseIdleConnections() }); ok {
		t.CloseIdleConnections()
	}
}

func (p *IngressProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	id := ""
	if p.opts.RequestID != nil {
		id = p.opts.RequestID(r)
	}
	if id == "" {
		id = newRequestID()
	}
	if !p.cfg.Matches(r.URL.Path) {
		ingressError(w, id, http.StatusNotFound)
		return
	}
	if r.Header.Get("Upgrade") != "" {
		ingressError(w, id, http.StatusNotImplemented)
		return
	}
	if r.ContentLength > p.cfg.MaxBodyBytes {
		ingressError(w, id, http.StatusRequestEntityTooLarge)
		return
	}
	ctx, cancel := context.WithCancelCause(r.Context())
	defer cancel(nil)
	st := &ingressReqState{id: id}
	defer st.stop()
	arm := func() { st.arm(p.cfg.Timeout, cancel) }
	if r.Body != nil && r.Body != http.NoBody && r.ContentLength != 0 {
		r.Body = &armOnDone{ReadCloser: http.MaxBytesReader(w, r.Body, p.cfg.MaxBodyBytes), onDone: arm}
	} else {
		arm()
	}
	ctx = context.WithValue(ctx, ingressCtxKey{}, st)
	p.rp.ServeHTTP(w, r.WithContext(ctx))
}

func (p *IngressProxy) handleError(w http.ResponseWriter, r *http.Request, err error) {
	id := ""
	if st, ok := r.Context().Value(ingressCtxKey{}).(*ingressReqState); ok {
		id = st.id
	}
	var tooLarge *http.MaxBytesError
	switch {
	case errors.As(err, &tooLarge):
		ingressError(w, id, http.StatusRequestEntityTooLarge)
	case errors.Is(context.Cause(r.Context()), errIngressTimeout):
		ingressError(w, id, http.StatusGatewayTimeout)
	default:
		if p.opts.ErrorLog != nil {
			p.opts.ErrorLog.Printf("native ingress: request %s: %v", id, err)
		}
		ingressError(w, id, http.StatusBadGateway)
	}
}

func (p *IngressProxy) clientIP(r *http.Request) string {
	if p.opts.ClientIP != nil {
		if ip := p.opts.ClientIP(r); ip != "" {
			return ip
		}
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

func (p *IngressProxy) scheme(r *http.Request) string {
	if p.opts.Scheme != nil {
		if s := p.opts.Scheme(r); s != "" {
			return s
		}
	}
	if r.TLS != nil {
		return "https"
	}
	return "http"
}

func ingressError(w http.ResponseWriter, id string, code int) {
	if id != "" {
		w.Header().Set(HeaderRequestID, id)
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_, _ = w.Write([]byte(http.StatusText(code) + "\n"))
}

func newRequestID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
