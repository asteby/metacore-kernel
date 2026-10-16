package apiroute

import (
	"slices"
	"strings"

	"github.com/google/uuid"

	v3 "github.com/asteby/metacore-kernel/manifest/v3"
)

// AuthKind is the kind of principal a route requires (Route.Auth) or a
// request carries (Principal.Kind).
type AuthKind string

const (
	// AuthNone — public route / anonymous caller. The organization is still
	// resolved by the host (e.g. from the request's domain).
	AuthNone AuthKind = "none"
	// AuthToken — a scoped API token (see package apitoken).
	AuthToken AuthKind = "token"
	// AuthCustomer — an end-customer session.
	AuthCustomer AuthKind = "customer"
	// AuthStaff — a back-office user.
	AuthStaff AuthKind = "staff"
)

// Cache is the HTTP caching policy of a GET route (mirrors v3.APICache).
type Cache struct {
	// MaxAge is the freshness lifetime in seconds.
	MaxAge int
	// SWR is the stale-while-revalidate window in seconds.
	SWR int
	// Private marks responses as caller-specific ("private, no-store").
	Private bool
}

// Route is a resolved API route (mirrors v3.APIRoute). Addon is filled by
// Router.Register.
type Route struct {
	// Addon is the key of the addon that owns the route.
	Addon string
	// Key identifies the route within the addon.
	Key string
	// Method is GET | POST | PUT | PATCH | DELETE.
	Method string
	// Path is the pattern ("/products/:handle", "/files/*rest").
	Path string
	// Auth is the principal kind the route requires.
	Auth AuthKind
	// Scopes the principal must all hold.
	Scopes []string
	// Cache is the GET caching policy; nil = not cacheable.
	Cache *Cache
	// Do is the handler reference ("wasm:<export>" | "compiled:<fn>").
	Do string
}

// RoutesFromManifest converts the manifest projection (manifest.Manifest.
// APIRoutes or v3 contributions.api) into router routes.
func RoutesFromManifest(api []v3.APIRoute) []Route {
	if len(api) == 0 {
		return nil
	}
	out := make([]Route, 0, len(api))
	for _, r := range api {
		rt := Route{
			Key:    r.Key,
			Method: r.Method,
			Path:   r.Path,
			Auth:   AuthKind(r.Auth),
			Scopes: slices.Clone(r.Scopes),
			Do:     r.Do,
		}
		if r.Cache != nil {
			rt.Cache = &Cache{MaxAge: r.Cache.MaxAge, SWR: r.Cache.SWR, Private: r.Cache.Private}
		}
		out = append(out, rt)
	}
	return out
}

// toManifest converts back so registration applies v3.ValidateAPIRoute — the
// exact rules the manifest validator enforces.
func (r Route) toManifest() v3.APIRoute {
	out := v3.APIRoute{
		Key:    r.Key,
		Method: r.Method,
		Path:   r.Path,
		Auth:   string(r.Auth),
		Scopes: r.Scopes,
		Do:     r.Do,
	}
	if r.Cache != nil {
		out.Cache = &v3.APICache{MaxAge: r.Cache.MaxAge, SWR: r.Cache.SWR, Private: r.Cache.Private}
	}
	return out
}

// Principal is the already-authenticated caller of a gateway request. The
// host resolves it (API token, customer session, staff JWT, or anonymous with
// the organization taken from the domain) before calling Gateway.Handle.
type Principal struct {
	// OrgID is the organization the request acts on. Required for every
	// route; uuid.Nil yields 404 on "none" routes and 401 otherwise.
	OrgID uuid.UUID
	// Kind is the principal kind; "" is treated as AuthNone.
	Kind AuthKind
	// Scopes granted to the principal. "*" grants everything and
	// "<area>:*" every verb of an area.
	Scopes []string
	// TokenID is the API token that authenticated the request (Kind token).
	TokenID uuid.UUID
	// UserID is the staff user (Kind staff).
	UserID uuid.UUID
	// CustomerID is the end customer (Kind customer). Opaque to the kernel.
	CustomerID string
}

// HasScope reports whether the principal holds scope, honouring the "*" and
// "<area>:*" wildcards.
func (p Principal) HasScope(scope string) bool {
	area, _, _ := strings.Cut(scope, ":")
	for _, s := range p.Scopes {
		if s == scope || s == "*" || s == area+":*" {
			return true
		}
	}
	return false
}

// kind normalizes an empty Kind to AuthNone.
func (p Principal) kind() AuthKind {
	if p.Kind == "" {
		return AuthNone
	}
	return p.Kind
}
