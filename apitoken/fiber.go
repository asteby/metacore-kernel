package apitoken

import (
	"errors"
	"math"
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"

	"github.com/asteby/metacore-kernel/runtime/apiroute"
)

// localPrincipal is the fiber.Ctx Locals key Middleware stores the principal
// under.
const localPrincipal = "apitoken.principal"

// ResolverConfig tunes NewPrincipalResolver / Middleware. The zero value
// works: in-memory limiter, DefaultRateLimitPerMinute, anonymous fallback.
type ResolverConfig struct {
	// Limiter enforces APIToken.RateLimitPerMinute per token. nil uses a
	// process-local MemoryLimiter.
	Limiter Limiter
	// DefaultPerMinute applies to tokens with RateLimitPerMinute 0. Zero
	// uses DefaultRateLimitPerMinute; negative disables the default limit.
	DefaultPerMinute int
	// Fallback resolves requests that carry no API token (no Authorization
	// header, or a bearer that is not pk_/sk_/pt_ — e.g. a staff JWT or a
	// customer session). nil yields an anonymous principal (kind none, no
	// organization: the gateway resolves it by domain).
	Fallback apiroute.PrincipalResolver
}

// BearerToken extracts the token of an "Authorization: Bearer <token>"
// header (scheme case-insensitive), or "".
func BearerToken(header string) string {
	scheme, tok, ok := strings.Cut(strings.TrimSpace(header), " ")
	if !ok || !strings.EqualFold(scheme, "bearer") {
		return ""
	}
	return strings.TrimSpace(tok)
}

// looksLikeAPIToken reports whether s carries one of the token prefixes.
func looksLikeAPIToken(s string) bool {
	return strings.HasPrefix(s, "pk_") || strings.HasPrefix(s, "sk_") || strings.HasPrefix(s, "pt_")
}

// PrincipalOf converts an authenticated token into the gateway principal:
// kind token, the token's scopes and id, and its organization (uuid.Nil for a
// platform token — the host resolves it by domain).
func PrincipalOf(t *APIToken) apiroute.Principal {
	return apiroute.Principal{
		OrgID:   t.Org(),
		Kind:    apiroute.AuthToken,
		Scopes:  append([]string(nil), t.Scopes...),
		TokenID: t.ID,
	}
}

// NewPrincipalResolver returns an apiroute.PrincipalResolver that
// authenticates "Authorization: Bearer pk_|sk_|pt_..." tokens and enforces
// their rate limit. Invalid, revoked or expired tokens yield
// apiroute.ErrUnauthorized; an exhausted budget apiroute.RateLimited(n).
func NewPrincipalResolver(svc *Service, cfg ResolverConfig) apiroute.PrincipalResolver {
	limiter := cfg.Limiter
	if limiter == nil {
		limiter = NewMemoryLimiter(nil)
	}
	def := cfg.DefaultPerMinute
	if def == 0 {
		def = DefaultRateLimitPerMinute
	}
	return func(c fiber.Ctx) (apiroute.Principal, error) {
		raw := BearerToken(c.Get(fiber.HeaderAuthorization))
		if !looksLikeAPIToken(raw) {
			if cfg.Fallback != nil {
				return cfg.Fallback(c)
			}
			return apiroute.Principal{Kind: apiroute.AuthNone}, nil
		}
		t, err := svc.Authenticate(c.Context(), raw)
		if err != nil {
			if errors.Is(err, ErrInvalidToken) || errors.Is(err, ErrRevoked) || errors.Is(err, ErrExpired) {
				return apiroute.Principal{}, apiroute.ErrUnauthorized
			}
			return apiroute.Principal{}, err
		}
		limit := t.RateLimitPerMinute
		if limit == 0 {
			limit = def
		}
		if ok, wait := limiter.Allow(t.ID.String(), limit); !ok {
			return apiroute.Principal{}, apiroute.RateLimited(int(math.Ceil(wait.Seconds())))
		}
		return PrincipalOf(t), nil
	}
}

// Middleware is a Fiber middleware for ordinary (non-gateway) routes that
// require an API token. It runs NewPrincipalResolver (without Fallback),
// answers 401/429 with the apiroute error body, and stores the principal for
// PrincipalFrom.
func Middleware(svc *Service, cfg ResolverConfig) fiber.Handler {
	cfg.Fallback = nil
	resolve := NewPrincipalResolver(svc, cfg)
	return func(c fiber.Ctx) error {
		p, err := resolve(c)
		if err == nil && p.Kind != apiroute.AuthToken {
			err = apiroute.ErrUnauthorized
		}
		if err != nil {
			return apiroute.WriteResponse(c, apiroute.ErrorResponse(err))
		}
		c.Locals(localPrincipal, p)
		return c.Next()
	}
}

// PrincipalFrom returns the principal Middleware stored on the request.
func PrincipalFrom(c fiber.Ctx) (apiroute.Principal, bool) {
	p, ok := c.Locals(localPrincipal).(apiroute.Principal)
	return p, ok
}

// --- staff CRUD ---------------------------------------------------------------

// StaffResolver extracts the organization and acting user from an
// authenticated staff request (typically the JWT claims).
type StaffResolver func(c fiber.Ctx) (org uuid.UUID, user uuid.UUID, err error)

// Handler exposes token management for an organization's staff over Fiber.
// Platform tokens are not managed here (issue them from platform tooling
// with Service.Issue).
type Handler struct {
	svc      *Service
	resolver StaffResolver
}

// NewHandler builds the handler.
func NewHandler(svc *Service, resolver StaffResolver) *Handler {
	return &Handler{svc: svc, resolver: resolver}
}

// Mount registers the routes on r (mount it behind the staff auth and
// permission middleware, e.g. api.Group("/api-tokens")).
//
//	GET    /             List the organization's tokens
//	POST   /             Issue {name, kind, scopes, expires_at?, rate_limit_per_minute?}
//	POST   /:id/rotate   Rotate {grace_seconds?} (default 0: old token revoked)
//	PATCH  /:id/scopes   Replace the scopes {scopes} (same secret, no rotation)
//	POST   /:id/revoke   Revoke
//	DELETE /:id          Revoke
func (h *Handler) Mount(r fiber.Router) {
	r.Get("/", h.list)
	r.Post("/", h.issue)
	r.Post("/:id/rotate", h.rotate)
	r.Patch("/:id/scopes", h.setScopes)
	r.Post("/:id/revoke", h.revoke)
	r.Delete("/:id", h.revoke)
}

func (h *Handler) staff(c fiber.Ctx) (uuid.UUID, uuid.UUID, error) {
	if h.resolver == nil {
		return uuid.Nil, uuid.Nil, errors.New("no staff resolver")
	}
	org, user, err := h.resolver(c)
	if err == nil && org == uuid.Nil {
		err = errors.New("no organization")
	}
	return org, user, err
}

func (h *Handler) list(c fiber.Ctx) error {
	org, _, err := h.staff(c)
	if err != nil {
		return fail(c, fiber.StatusUnauthorized, err.Error())
	}
	items, err := h.svc.List(c.Context(), org)
	if err != nil {
		return fail(c, fiber.StatusInternalServerError, "could not list tokens")
	}
	return ok(c, items)
}

type issueBody struct {
	Name               string     `json:"name"`
	Kind               Kind       `json:"kind"`
	Scopes             []string   `json:"scopes"`
	ExpiresAt          *time.Time `json:"expires_at"`
	RateLimitPerMinute int        `json:"rate_limit_per_minute"`
}

func (h *Handler) issue(c fiber.Ctx) error {
	org, user, err := h.staff(c)
	if err != nil {
		return fail(c, fiber.StatusUnauthorized, err.Error())
	}
	var b issueBody
	if err := c.Bind().Body(&b); err != nil {
		return fail(c, fiber.StatusBadRequest, "invalid body")
	}
	if b.Kind == KindPlatform {
		return fail(c, fiber.StatusForbidden, "platform tokens cannot be issued by an organization")
	}
	plain, t, err := h.svc.Issue(c.Context(), org, b.Name, b.Kind, b.Scopes, b.ExpiresAt,
		WithRateLimit(b.RateLimitPerMinute), WithCreatedBy(user))
	if err != nil {
		return failErr(c, err)
	}
	c.Set(fiber.HeaderCacheControl, "no-store")
	return c.Status(fiber.StatusCreated).JSON(fiber.Map{"success": true, "data": fiber.Map{"token": plain, "api_token": t}})
}

type rotateBody struct {
	GraceSeconds int64 `json:"grace_seconds"`
}

func (h *Handler) rotate(c fiber.Ctx) error {
	t, err := h.owned(c)
	if err != nil {
		return failErr(c, err)
	}
	var b rotateBody
	if len(c.Body()) > 0 {
		if err := c.Bind().Body(&b); err != nil {
			return fail(c, fiber.StatusBadRequest, "invalid body")
		}
	}
	plain, nt, err := h.svc.Rotate(c.Context(), t.ID, time.Duration(b.GraceSeconds)*time.Second)
	if err != nil {
		return failErr(c, err)
	}
	c.Set(fiber.HeaderCacheControl, "no-store")
	return ok(c, fiber.Map{"token": plain, "api_token": nt})
}

type scopesBody struct {
	Scopes []string `json:"scopes"`
}

func (h *Handler) setScopes(c fiber.Ctx) error {
	t, err := h.owned(c)
	if err != nil {
		return failErr(c, err)
	}
	var b scopesBody
	if err := c.Bind().Body(&b); err != nil {
		return fail(c, fiber.StatusBadRequest, "invalid body")
	}
	nt, err := h.svc.SetScopes(c.Context(), t.ID, b.Scopes)
	if err != nil {
		return failErr(c, err)
	}
	return ok(c, fiber.Map{"api_token": nt})
}

func (h *Handler) revoke(c fiber.Ctx) error {
	t, err := h.owned(c)
	if err != nil {
		return failErr(c, err)
	}
	if err := h.svc.Revoke(c.Context(), t.ID); err != nil {
		return failErr(c, err)
	}
	return ok(c, nil)
}

// owned loads the :id token and checks it belongs to the staff's
// organization (a foreign token is reported as not found).
func (h *Handler) owned(c fiber.Ctx) (*APIToken, error) {
	org, _, err := h.staff(c)
	if err != nil {
		return nil, errUnauthorizedStaff
	}
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return nil, ErrNotFound
	}
	t, err := h.svc.Get(c.Context(), id)
	if err != nil {
		return nil, err
	}
	if t.Org() != org {
		return nil, ErrNotFound
	}
	return t, nil
}

var errUnauthorizedStaff = errors.New("unauthorized")

func failErr(c fiber.Ctx, err error) error {
	switch {
	case errors.Is(err, errUnauthorizedStaff):
		return fail(c, fiber.StatusUnauthorized, err.Error())
	case errors.Is(err, ErrNotFound):
		return fail(c, fiber.StatusNotFound, ErrNotFound.Error())
	case errors.Is(err, ErrInvalidKind), errors.Is(err, ErrInvalidScope), errors.Is(err, ErrInvalidInput):
		return fail(c, fiber.StatusBadRequest, err.Error())
	case errors.Is(err, ErrRevoked), errors.Is(err, ErrExpired):
		return fail(c, fiber.StatusConflict, err.Error())
	}
	return fail(c, fiber.StatusInternalServerError, "internal error")
}

func ok(c fiber.Ctx, data any) error {
	return c.JSON(fiber.Map{"success": true, "data": data})
}

func fail(c fiber.Ctx, status int, msg string) error {
	return c.Status(status).JSON(fiber.Map{"success": false, "message": msg})
}
