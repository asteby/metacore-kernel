package apiroute

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/google/uuid"
)

// DefaultMaxBodyBytes is the default request body limit (1 MiB).
const DefaultMaxBodyBytes int64 = 1 << 20

// Dispatcher invokes a route handler. target is Route.Do ("wasm:<export>",
// "compiled:<fn>"); req.Addon names the addon that owns it. A WASM
// implementation marshals req, invokes the export for (orgID, req.Addon) and
// returns DecodeResponse(output). Returning an *Error answers with its status
// and code; any other error becomes ErrHandlerFailed (502).
type Dispatcher interface {
	Invoke(ctx context.Context, orgID uuid.UUID, target string, req Request) (Response, error)
}

// DispatcherFunc adapts a function to Dispatcher.
type DispatcherFunc func(ctx context.Context, orgID uuid.UUID, target string, req Request) (Response, error)

// Invoke implements Dispatcher.
func (f DispatcherFunc) Invoke(ctx context.Context, orgID uuid.UUID, target string, req Request) (Response, error) {
	return f(ctx, orgID, target, req)
}

// AddonEnabledFunc reports whether addonKey is installed and enabled for the
// organization. Routes of a disabled addon answer 404.
type AddonEnabledFunc func(ctx context.Context, orgID uuid.UUID, addonKey string) (bool, error)

// DomainResolver maps a request domain (e.g. "mitienda.mx") to the
// organization that has it verified. The host implements it over its own
// domain registry; the kernel keeps no domain table. Return uuid.Nil (or an
// error) for an unknown or unverified domain.
type DomainResolver interface {
	ResolveDomain(ctx context.Context, domain string) (uuid.UUID, error)
}

// DomainResolverFunc adapts a function to DomainResolver.
type DomainResolverFunc func(ctx context.Context, domain string) (uuid.UUID, error)

// ResolveDomain implements DomainResolver.
func (f DomainResolverFunc) ResolveDomain(ctx context.Context, domain string) (uuid.UUID, error) {
	return f(ctx, domain)
}

// Inbound is a transport-neutral HTTP request handed to Gateway.Handle.
type Inbound struct {
	// Method is the HTTP method.
	Method string
	// Path is the (percent-escaped) path relative to the gateway mount.
	Path string
	// Query is the parsed query string.
	Query map[string][]string
	// Headers are the request headers (any case).
	Headers http.Header
	// Body is the raw request body.
	Body []byte
	// Domain is the storefront domain the request is for (the Fiber adapter
	// takes it from X-Storefront-Domain, else the Host header, port
	// stripped). Used only when Principal.OrgID is uuid.Nil and a
	// DomainResolver is configured.
	Domain string
	// Principal is the caller, already resolved by the host.
	Principal Principal
}

// Gateway serves API routes: match, authorize, invoke, post-process. It is
// framework-agnostic; see Handler for the Fiber adapter. Safe for concurrent
// use.
type Gateway struct {
	router     *Router
	dispatcher Dispatcher
	maxBody    int64
	enabled    AddonEnabledFunc
	reqHeaders map[string]struct{}
	domains    DomainResolver
	log        *slog.Logger
}

// Option tunes a Gateway.
type Option func(*Gateway)

// WithMaxBodyBytes overrides the request body limit (DefaultMaxBodyBytes).
// n <= 0 keeps the default.
func WithMaxBodyBytes(n int64) Option {
	return func(g *Gateway) {
		if n > 0 {
			g.maxBody = n
		}
	}
}

// WithAddonEnabled gates every route on its addon being enabled for the
// request's organization. Without it every registered route serves every
// organization.
func WithAddonEnabled(f AddonEnabledFunc) Option {
	return func(g *Gateway) { g.enabled = f }
}

// WithDomainResolver lets Handle fill a missing Principal.OrgID from
// Inbound.Domain (hosted multi-tenant mode: a platform token or an anonymous
// caller acting for whichever organization owns the domain). A principal
// that already carries an organization is never re-scoped by the domain.
func WithDomainResolver(d DomainResolver) Option {
	return func(g *Gateway) { g.domains = d }
}

// WithRequestHeaders adds request headers (case-insensitive) to the
// DefaultRequestHeaders allowlist forwarded to handlers.
func WithRequestHeaders(names ...string) Option {
	return func(g *Gateway) {
		for _, n := range names {
			if n = strings.ToLower(strings.TrimSpace(n)); n != "" {
				g.reqHeaders[n] = struct{}{}
			}
		}
	}
}

// WithLogger sets the logger used for handler failures (never bodies or
// credentials). Defaults to slog.Default().
func WithLogger(l *slog.Logger) Option {
	return func(g *Gateway) {
		if l != nil {
			g.log = l
		}
	}
}

// NewGateway builds a Gateway over router and dispatcher.
func NewGateway(router *Router, dispatcher Dispatcher, opts ...Option) *Gateway {
	if router == nil {
		router = NewRouter()
	}
	g := &Gateway{
		router:     router,
		dispatcher: dispatcher,
		maxBody:    DefaultMaxBodyBytes,
		reqHeaders: map[string]struct{}{},
		log:        slog.Default(),
	}
	for _, h := range DefaultRequestHeaders {
		g.reqHeaders[h] = struct{}{}
	}
	for _, o := range opts {
		if o != nil {
			o(g)
		}
	}
	return g
}

// Router returns the gateway's route table.
func (g *Gateway) Router() *Router { return g.router }

// MaxBodyBytes returns the effective request body limit, so transports can
// cap reads before buffering.
func (g *Gateway) MaxBodyBytes() int64 { return g.maxBody }

// Handle serves one request. On success the Response is final (status
// validated, headers allowlisted, Cache-Control/ETag derived, 304 applied).
// On failure the error wraps one of the typed *Error sentinels (ErrNotFound,
// ErrMethodNotAllowed, ErrUnauthorized, ErrForbidden, ErrOrgUnresolved,
// ErrPayloadTooLarge,
// ErrBadResponse, ErrHandlerFailed) or an *Error from the dispatcher; render
// it with ErrorResponse.
func (g *Gateway) Handle(ctx context.Context, in Inbound) (Response, error) {
	if int64(len(in.Body)) > g.maxBody {
		return Response{}, ErrPayloadTooLarge
	}
	route, params, ok := g.router.Match(in.Method, in.Path)
	if !ok {
		if allowed := g.router.Allowed(in.Path); len(allowed) > 0 {
			return Response{}, &methodNotAllowedError{allowed: allowed}
		}
		return Response{}, ErrNotFound
	}
	p := in.Principal
	if err := authorize(route, p); err != nil {
		return Response{}, err
	}
	if p.OrgID == uuid.Nil {
		org, err := g.resolveDomain(ctx, in.Domain)
		if err != nil {
			return Response{}, err
		}
		p.OrgID = org
		in.Principal = p
	}
	if g.enabled != nil {
		on, err := g.enabled(ctx, p.OrgID, route.Addon)
		if err != nil {
			return Response{}, fmt.Errorf("apiroute: addon enabled check: %w", err)
		}
		if !on {
			return Response{}, fmt.Errorf("%w: addon %q not enabled", ErrNotFound, route.Addon)
		}
	}
	if g.dispatcher == nil {
		return Response{}, ErrNoDispatcher
	}

	req := g.buildRequest(route, params, in)
	resp, err := g.dispatcher.Invoke(ctx, p.OrgID, route.Do, req)
	if err != nil {
		var typed *Error
		if errors.As(err, &typed) {
			return Response{}, err
		}
		g.log.ErrorContext(ctx, "apiroute: handler failed",
			"addon", route.Addon, "route", route.Key, "org_id", p.OrgID, "error", err)
		return Response{}, fmt.Errorf("%w: %v", ErrHandlerFailed, err)
	}
	return finalize(route, in, resp)
}

// resolveDomain resolves the organization of a principal that carries none.
func (g *Gateway) resolveDomain(ctx context.Context, domain string) (uuid.UUID, error) {
	domain = strings.ToLower(strings.TrimSpace(domain))
	if g.domains == nil || domain == "" {
		return uuid.Nil, ErrOrgUnresolved
	}
	org, err := g.domains.ResolveDomain(ctx, domain)
	if err != nil {
		return uuid.Nil, fmt.Errorf("%w: domain %q: %v", ErrOrgUnresolved, domain, err)
	}
	if org == uuid.Nil {
		return uuid.Nil, fmt.Errorf("%w: domain %q", ErrOrgUnresolved, domain)
	}
	return org, nil
}

// authorize enforces the route's principal kind and scopes. "token" routes
// also admit customer principals (a customer session is a storefront caller
// too); "customer" and "staff" admit only their own kind.
func authorize(r Route, p Principal) error {
	k := p.kind()
	switch r.Auth {
	case AuthNone:
		return nil
	case AuthToken:
		if k != AuthToken && k != AuthCustomer {
			return fmt.Errorf("%w: route %s.%s requires an API token", ErrUnauthorized, r.Addon, r.Key)
		}
	case AuthCustomer:
		if k != AuthCustomer {
			return fmt.Errorf("%w: route %s.%s requires a customer session", ErrUnauthorized, r.Addon, r.Key)
		}
	case AuthStaff:
		if k != AuthStaff {
			return fmt.Errorf("%w: route %s.%s requires a staff session", ErrUnauthorized, r.Addon, r.Key)
		}
	default:
		return fmt.Errorf("%w: route %s.%s has unknown auth %q", ErrForbidden, r.Addon, r.Key, r.Auth)
	}
	for _, s := range r.Scopes {
		if !p.HasScope(s) {
			return fmt.Errorf("%w: missing scope %q", ErrForbidden, s)
		}
	}
	return nil
}

func (g *Gateway) buildRequest(route Route, params map[string]string, in Inbound) Request {
	query := make(map[string][]string, len(in.Query))
	for k, vs := range in.Query {
		query[k] = append([]string(nil), vs...)
	}
	if params == nil {
		params = map[string]string{}
	}
	p := in.Principal
	auth := RequestAuth{Kind: p.kind(), CustomerID: p.CustomerID}
	if len(p.Scopes) > 0 {
		auth.Scopes = append([]string(nil), p.Scopes...)
	}
	if p.TokenID != uuid.Nil {
		auth.TokenID = p.TokenID.String()
	}
	if p.UserID != uuid.Nil {
		auth.UserID = p.UserID.String()
	}
	return Request{
		Addon:   route.Addon,
		Route:   route.Key,
		Method:  strings.ToUpper(in.Method),
		Path:    in.Path,
		Params:  params,
		Query:   query,
		Headers: filterRequestHeaders(in.Headers, g.reqHeaders),
		Body:    encodeBody(in.Body),
		Auth:    auth,
		Org:     p.OrgID,
	}
}

// finalize validates and post-processes a handler Response:
//
//   - status 0 → 200; outside 100–599 → ErrBadResponse;
//   - headers outside ResponseHeaders (or carrying CR/LF) are dropped;
//   - 1xx/204/304 carry no body; a body without Content-Type is JSON;
//   - without a handler Cache-Control, a cacheable route's 2xx GET gets one
//     derived from Route.Cache, anything else "no-store";
//   - a GET 200 with a body and no ETag gets a weak ETag of the body, and a
//     matching If-None-Match turns it into a 304 that keeps only
//     Cache-Control, ETag and Vary.
func finalize(route Route, in Inbound, resp Response) (Response, error) {
	status := resp.Status
	if status == 0 {
		status = http.StatusOK
	}
	if status < 100 || status > 599 {
		return Response{}, fmt.Errorf("%w: status %d out of range", ErrBadResponse, resp.Status)
	}
	out := Response{Status: status, Headers: sanitizeResponseHeaders(resp.Headers), Body: resp.Body}
	if status < 200 || status == http.StatusNoContent || status == http.StatusNotModified {
		out.Body = nil
	}
	if len(out.Body) > 0 && out.Headers["Content-Type"] == "" {
		out.Headers["Content-Type"] = "application/json; charset=utf-8"
	}
	isGet := normalizeMethod(in.Method) == http.MethodGet
	success := status >= 200 && status < 300
	if out.Headers["Cache-Control"] == "" {
		if route.Cache != nil && isGet && success {
			out.Headers["Cache-Control"] = cacheControlFor(route.Cache)
		} else {
			out.Headers["Cache-Control"] = "no-store"
		}
	}
	if isGet && status == http.StatusOK && len(out.Body) > 0 {
		if out.Headers["ETag"] == "" {
			out.Headers["ETag"] = weakETag(out.Body)
		}
		if etagMatches(in.Headers.Get("If-None-Match"), out.Headers["ETag"]) {
			kept := map[string]string{}
			for _, h := range []string{"Cache-Control", "ETag", "Vary"} {
				if v := out.Headers[h]; v != "" {
					kept[h] = v
				}
			}
			return Response{Status: http.StatusNotModified, Headers: kept}, nil
		}
	}
	return out, nil
}
