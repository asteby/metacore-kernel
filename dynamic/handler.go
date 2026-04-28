package dynamic

import (
	"strconv"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"

	"github.com/asteby/metacore-kernel/modelbase"
	"github.com/asteby/metacore-kernel/query"
)

// UserResolver extracts the authenticated user from the Fiber context.
// Apps wire this to their auth middleware (e.g. using auth.GetClaims).
type UserResolver func(c *fiber.Ctx) modelbase.AuthUser

// Handler exposes dynamic CRUD over Fiber.
type Handler struct {
	service  *Service
	resolver UserResolver
}

// NewHandler constructs a Handler. If resolver is nil, the handler returns 401
// for every request.
func NewHandler(service *Service, resolver UserResolver) *Handler {
	return &Handler{service: service, resolver: resolver}
}

// Mount attaches the CRUD routes under the given router.
//
//	GET    /dynamic/:model           List (paginated + filtered)
//	POST   /dynamic/:model           Create
//	GET    /dynamic/:model/:id       Get
//	PUT    /dynamic/:model/:id       Update
//	DELETE /dynamic/:model/:id       Delete
func (h *Handler) Mount(r fiber.Router, middleware ...fiber.Handler) {
	h.MountWith(MountOpts{Middleware: middleware})(r)
}

// MountOpts customises route registration.
type MountOpts struct {
	// Middleware runs on every CRUD route (auth, request logging, …).
	Middleware []fiber.Handler

	// MutationMiddleware runs only on routes that change DB state — POST
	// /:model (create), POST /:model/import. Apps drop the kernel's
	// `idempotency.Middleware` here so retries replay the original
	// response instead of duplicating writes. The validate endpoint
	// (POST /:model/import/validate) does not mutate state and is
	// excluded by design.
	MutationMiddleware []fiber.Handler
}

// MountWith returns a function that registers all CRUD + import routes
// against `r`, layering the configured middleware. Useful when the app
// wants to wire route-specific middleware (e.g. idempotency) without
// touching every endpoint by hand.
func (h *Handler) MountWith(opts MountOpts) func(r fiber.Router) {
	return func(r fiber.Router) {
		g := r.Group("/dynamic", opts.Middleware...)

		// Read paths — no mutation middleware.
		g.Get("/:model", h.list)
		g.Get("/:model/export", h.exportData)
		g.Get("/:model/export/template", h.exportTemplate)
		g.Post("/:model/import/validate", h.importValidate)

		// Mutation paths — receive the extra middleware chain.
		mut := opts.MutationMiddleware
		g.Post("/:model", chain(mut, h.create)...)
		g.Post("/:model/import", chain(mut, h.importData)...)

		// Read paths after dynamic ones (matters for Fiber router order).
		g.Get("/:model/:id", h.get)
		g.Put("/:model/:id", h.update)
		g.Delete("/:model/:id", h.delete)
	}
}

// chain prepends the middleware chain to the final handler, returning the
// slice fiber wants for `g.Post(path, handler...)`.
func chain(mw []fiber.Handler, final fiber.Handler) []fiber.Handler {
	if len(mw) == 0 {
		return []fiber.Handler{final}
	}
	out := make([]fiber.Handler, 0, len(mw)+1)
	out = append(out, mw...)
	out = append(out, final)
	return out
}

// MountOptions attaches options + search lookups. These are mounted outside
// the /dynamic prefix because existing apps expose them at /api/options/:model
// and /api/search/:model, and preserving those paths avoids a frontend change.
//
//	GET    /options/:model    Options (by ?field=...)
//	GET    /search/:model     Search  (by ?q=... or ?search=...)
func (h *Handler) MountOptions(r fiber.Router, middleware ...fiber.Handler) {
	r.Get("/options/:model", append(middleware, h.options)...)
	r.Get("/search/:model", append(middleware, h.search)...)
}

func (h *Handler) user(c *fiber.Ctx) modelbase.AuthUser {
	if h.resolver == nil {
		return nil
	}
	return h.resolver(c)
}

func (h *Handler) list(c *fiber.Ctx) error {
	u := h.user(c)
	if u == nil {
		return respondErr(c, fiber.StatusUnauthorized, "not authenticated")
	}
	params, err := query.ParseFiber(c)
	if err != nil {
		return respondErr(c, fiber.StatusBadRequest, err.Error())
	}
	items, meta, err := h.service.List(c.Context(), c.Params("model"), u, params)
	if err != nil {
		return h.handleError(c, err)
	}
	return c.JSON(fiber.Map{"success": true, "data": items, "meta": meta})
}

func (h *Handler) get(c *fiber.Ctx) error {
	u := h.user(c)
	if u == nil {
		return respondErr(c, fiber.StatusUnauthorized, "not authenticated")
	}
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return respondErr(c, fiber.StatusBadRequest, ErrInvalidID.Error())
	}
	record, err := h.service.Get(c.Context(), c.Params("model"), u, id)
	if err != nil {
		return h.handleError(c, err)
	}
	return c.JSON(fiber.Map{"success": true, "data": record})
}

func (h *Handler) create(c *fiber.Ctx) error {
	u := h.user(c)
	if u == nil {
		return respondErr(c, fiber.StatusUnauthorized, "not authenticated")
	}
	var input map[string]any
	if err := c.BodyParser(&input); err != nil {
		return respondErr(c, fiber.StatusBadRequest, "invalid body")
	}
	record, err := h.service.Create(c.Context(), c.Params("model"), u, input)
	if err != nil {
		return h.handleError(c, err)
	}
	return c.Status(fiber.StatusCreated).JSON(fiber.Map{"success": true, "data": record})
}

func (h *Handler) update(c *fiber.Ctx) error {
	u := h.user(c)
	if u == nil {
		return respondErr(c, fiber.StatusUnauthorized, "not authenticated")
	}
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return respondErr(c, fiber.StatusBadRequest, ErrInvalidID.Error())
	}
	var input map[string]any
	if err := c.BodyParser(&input); err != nil {
		return respondErr(c, fiber.StatusBadRequest, "invalid body")
	}
	record, err := h.service.Update(c.Context(), c.Params("model"), u, id, input)
	if err != nil {
		return h.handleError(c, err)
	}
	return c.JSON(fiber.Map{"success": true, "data": record})
}

func (h *Handler) delete(c *fiber.Ctx) error {
	u := h.user(c)
	if u == nil {
		return respondErr(c, fiber.StatusUnauthorized, "not authenticated")
	}
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return respondErr(c, fiber.StatusBadRequest, ErrInvalidID.Error())
	}
	if err := h.service.Delete(c.Context(), c.Params("model"), u, id); err != nil {
		return h.handleError(c, err)
	}
	return c.JSON(fiber.Map{"success": true})
}

func (h *Handler) options(c *fiber.Ctx) error {
	u := h.user(c)
	q := OptionsQuery{
		Model:       c.Params("model"),
		Field:       c.Query("field"),
		Q:           c.Query("q"),
		FilterValue: c.Query("filter_value"),
	}
	if v := c.Query("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			q.Limit = n
		}
	}
	if v := c.Query("offset"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			q.Offset = n
		}
	}
	res, err := h.service.Options(c.Context(), u, q)
	if err != nil {
		return h.handleError(c, err)
	}
	return c.JSON(fiber.Map{"success": true, "data": res.Options, "type": res.Type})
}

func (h *Handler) search(c *fiber.Ctx) error {
	u := h.user(c)
	q := SearchQuery{
		Model: c.Params("model"),
		Q:     c.Query("q"),
	}
	if q.Q == "" {
		q.Q = c.Query("search")
	}
	if v := c.Query("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			q.Limit = n
		}
	}
	hits, err := h.service.Search(c.Context(), u, q)
	if err != nil {
		return h.handleError(c, err)
	}
	return c.JSON(fiber.Map{"success": true, "data": hits})
}

func (h *Handler) handleError(c *fiber.Ctx, err error) error {
	switch err {
	case ErrModelNotFound, ErrRecordNotFound, ErrSourceModelNotFound, ErrOptionsFieldNotFound:
		return respondErr(c, fiber.StatusNotFound, err.Error())
	case ErrForbidden:
		return respondErr(c, fiber.StatusForbidden, err.Error())
	case ErrFieldRequired, ErrInvalidInput:
		return respondErr(c, fiber.StatusBadRequest, err.Error())
	case ErrNoOptionsConfig, ErrNoSearchConfig:
		return respondErr(c, fiber.StatusNotImplemented, err.Error())
	default:
		if err.Error() == "permission denied" {
			return respondErr(c, fiber.StatusForbidden, err.Error())
		}
		return respondErr(c, fiber.StatusInternalServerError, err.Error())
	}
}

func respondErr(c *fiber.Ctx, status int, msg string) error {
	return c.Status(status).JSON(fiber.Map{"success": false, "message": msg})
}
