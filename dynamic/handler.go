package dynamic

import (
	"strconv"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"

	"github.com/asteby/metacore-kernel/modelbase"
	"github.com/asteby/metacore-kernel/query"
)

// UserResolver extracts the authenticated user from the Fiber context.
// Apps wire this to their auth middleware (e.g. using auth.GetClaims).
type UserResolver func(c fiber.Ctx) modelbase.AuthUser

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
		mws := handlersToAny(opts.Middleware)
		g := r.Group("/dynamic", mws...)

		// Read paths — no mutation middleware.
		g.Get("/:model", h.list)
		g.Get("/:model/export", h.exportData)
		g.Get("/:model/export/template", h.exportTemplate)
		g.Post("/:model/import/validate", h.importValidate)

		// Mutation paths — receive the extra middleware chain. Fiber v3
		// expects (handler, ...rest) where the first arg runs first; we
		// front-load any mutation middleware before the final handler.
		registerMut(g.Post, "/:model", opts.MutationMiddleware, h.create)
		registerMut(g.Post, "/:model/import", opts.MutationMiddleware, h.importData)

		// Read paths after dynamic ones (matters for Fiber router order).
		g.Get("/:model/:id", h.get)
		g.Put("/:model/:id", h.update)
		g.Delete("/:model/:id", h.delete)
	}
}

// registerMut wires a mutation route through fiber v3's
// `Method(path, handler any, handlers ...any)` signature so that any provided
// middleware runs before `final`. When there is no middleware we register the
// final handler directly.
func registerMut(register func(string, any, ...any) fiber.Router, path string, mw []fiber.Handler, final fiber.Handler) {
	if len(mw) == 0 {
		register(path, final)
		return
	}
	rest := make([]any, 0, len(mw))
	for _, h := range mw[1:] {
		if h != nil {
			rest = append(rest, h)
		}
	}
	rest = append(rest, final)
	register(path, mw[0], rest...)
}

// handlersToAny converts a typed []fiber.Handler slice to []any so it can be
// spread into fiber v3 router methods (which take `any` for middleware).
func handlersToAny(in []fiber.Handler) []any {
	if len(in) == 0 {
		return nil
	}
	out := make([]any, 0, len(in))
	for _, h := range in {
		if h != nil {
			out = append(out, h)
		}
	}
	return out
}

// MountOptions attaches options + search lookups. These are mounted outside
// the /dynamic prefix because existing apps expose them at /api/options/:model
// and /api/search/:model, and preserving those paths avoids a frontend change.
//
//	GET    /options/:model    Options (by ?field=...)
//	GET    /search/:model     Search  (by ?q=... or ?search=...)
func (h *Handler) MountOptions(r fiber.Router, middleware ...fiber.Handler) {
	registerMut(r.Get, "/options/:model", middleware, h.options)
	registerMut(r.Get, "/search/:model", middleware, h.search)
}

func (h *Handler) user(c fiber.Ctx) modelbase.AuthUser {
	if h.resolver == nil {
		return nil
	}
	return h.resolver(c)
}

func (h *Handler) list(c fiber.Ctx) error {
	u := h.user(c)
	if u == nil {
		return respondErr(c, fiber.StatusUnauthorized, "not authenticated")
	}
	params, err := query.ParseFiber(c)
	if err != nil {
		return respondErr(c, fiber.StatusBadRequest, err.Error())
	}
	items, meta, err := h.service.List(c, c.Params("model"), u, params)
	if err != nil {
		return h.handleError(c, err)
	}
	return c.JSON(fiber.Map{"success": true, "data": items, "meta": meta})
}

func (h *Handler) get(c fiber.Ctx) error {
	u := h.user(c)
	if u == nil {
		return respondErr(c, fiber.StatusUnauthorized, "not authenticated")
	}
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return respondErr(c, fiber.StatusBadRequest, ErrInvalidID.Error())
	}
	record, err := h.service.Get(c, c.Params("model"), u, id)
	if err != nil {
		return h.handleError(c, err)
	}
	return c.JSON(fiber.Map{"success": true, "data": record})
}

func (h *Handler) create(c fiber.Ctx) error {
	u := h.user(c)
	if u == nil {
		return respondErr(c, fiber.StatusUnauthorized, "not authenticated")
	}
	var input map[string]any
	if err := c.Bind().Body(&input); err != nil {
		return respondErr(c, fiber.StatusBadRequest, "invalid body")
	}
	record, err := h.service.Create(c, c.Params("model"), u, input)
	if err != nil {
		return h.handleError(c, err)
	}
	return c.Status(fiber.StatusCreated).JSON(fiber.Map{"success": true, "data": record})
}

func (h *Handler) update(c fiber.Ctx) error {
	u := h.user(c)
	if u == nil {
		return respondErr(c, fiber.StatusUnauthorized, "not authenticated")
	}
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return respondErr(c, fiber.StatusBadRequest, ErrInvalidID.Error())
	}
	var input map[string]any
	if err := c.Bind().Body(&input); err != nil {
		return respondErr(c, fiber.StatusBadRequest, "invalid body")
	}
	record, err := h.service.Update(c, c.Params("model"), u, id, input)
	if err != nil {
		return h.handleError(c, err)
	}
	return c.JSON(fiber.Map{"success": true, "data": record})
}

func (h *Handler) delete(c fiber.Ctx) error {
	u := h.user(c)
	if u == nil {
		return respondErr(c, fiber.StatusUnauthorized, "not authenticated")
	}
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return respondErr(c, fiber.StatusBadRequest, ErrInvalidID.Error())
	}
	if err := h.service.Delete(c, c.Params("model"), u, id); err != nil {
		return h.handleError(c, err)
	}
	return c.JSON(fiber.Map{"success": true})
}

func (h *Handler) options(c fiber.Ctx) error {
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
	res, err := h.service.Options(c, u, q)
	if err != nil {
		return h.handleError(c, err)
	}
	return c.JSON(fiber.Map{"success": true, "data": res.Options, "type": res.Type})
}

func (h *Handler) search(c fiber.Ctx) error {
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
	hits, err := h.service.Search(c, u, q)
	if err != nil {
		return h.handleError(c, err)
	}
	return c.JSON(fiber.Map{"success": true, "data": hits})
}

func (h *Handler) handleError(c fiber.Ctx, err error) error {
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

func respondErr(c fiber.Ctx, status int, msg string) error {
	return c.Status(status).JSON(fiber.Map{"success": false, "message": msg})
}
