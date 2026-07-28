package dynamic

import (
	"errors"
	"strconv"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"

	"github.com/asteby/metacore-kernel/auth/adapters"
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
//	GET    /dynamic/:model                       List (paginated + filtered)
//	POST   /dynamic/:model                       Create
//	GET    /dynamic/:model/:id                   Get
//	PUT    /dynamic/:model/:id                   Update
//	DELETE /dynamic/:model/:id                   Delete
//	POST   /dynamic/:model/:id/action/:key       Dispatch a per-row action
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
		g.Get("/:model/aggregate", h.aggregate)
		g.Get("/:model/facets", h.facets)
		g.Get("/:model/export", h.exportData)
		g.Get("/:model/export/template", h.exportTemplate)
		g.Post("/:model/import/validate", h.importValidate)

		// Mutation paths — receive the extra middleware chain. Fiber v3
		// expects (handler, ...rest) where the first arg runs first; we
		// front-load any mutation middleware before the final handler.
		registerMut(g.Post, "/:model", opts.MutationMiddleware, h.create)
		registerMut(g.Post, "/:model/import", opts.MutationMiddleware, h.importData)
		registerMut(g.Post, "/:model/:id/action/:key", opts.MutationMiddleware, h.action)

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
	// Legacy path: app supplied a UserResolver that returns the full
	// modelbase.AuthUser. Honoured first so existing wiring keeps working
	// unchanged.
	if h.resolver != nil {
		if u := h.resolver(c); u != nil {
			return u
		}
	}

	// Fallback path: Service.AuthExtractor is configured. Apps with a custom
	// principal shape (raw UUIDs in Fiber locals, JWT-claims-only auth, …)
	// wire an extractor instead of a resolver and the handler bridges back to
	// modelbase.AuthUser via adapters.WrapAsModelbase.
	extractor := h.service.AuthExtractor()
	if extractor == nil {
		return nil
	}
	ctx := adapters.WithFiberCtx(c, c)
	provider, err := extractor(ctx)
	if err != nil || provider == nil {
		return nil
	}
	return adapters.WrapAsModelbase(provider)
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

// aggregate returns the per-column SUM over the FILTERED set for every column
// whose metadata opts in via StyleConfig["aggregate"] == "sum" (mapped by the
// kernel from a manifest's display_config.aggregate). It parses the same query
// params as list — so the footer carries the identical f_*, search and
// org/branch scope — but runs a single aggregate query with no sort/pagination.
// The response envelope mirrors get: {success, data:{<col>:<number>}}.
func (h *Handler) aggregate(c fiber.Ctx) error {
	u := h.user(c)
	if u == nil {
		return respondErr(c, fiber.StatusUnauthorized, "not authenticated")
	}
	model := c.Params("model")
	params, err := query.ParseFiber(c)
	if err != nil {
		return respondErr(c, fiber.StatusBadRequest, err.Error())
	}

	// Pick the aggregate-flagged columns from the model's metadata. A column
	// opts in when StyleConfig["aggregate"] is a non-empty string (e.g. "sum").
	meta, err := h.service.TableMetadata(c, model)
	if err != nil {
		return h.handleError(c, err)
	}
	columns := aggregateColumns(meta)
	if len(columns) == 0 {
		// No column opts in — return an empty totals map rather than running a
		// pointless query. The footer simply renders nothing.
		return c.JSON(fiber.Map{"success": true, "data": fiber.Map{}})
	}

	totals, err := h.service.Aggregate(c, model, u, params, columns)
	if err != nil {
		return h.handleError(c, err)
	}
	return c.JSON(fiber.Map{"success": true, "data": totals})
}

// aggregateColumns returns the column keys whose metadata declares
// StyleConfig["aggregate"] as a non-empty string. nil-safe.
func aggregateColumns(meta *modelbase.TableMetadata) []string {
	if meta == nil {
		return nil
	}
	var cols []string
	for _, col := range meta.Columns {
		if col.StyleConfig == nil {
			continue
		}
		if v, ok := col.StyleConfig["aggregate"].(string); ok && v != "" {
			cols = append(cols, col.Key)
		}
	}
	return cols
}

// facets returns the distinct values (with counts) of a column, org/branch
// scoped, so a text-column filter can offer the values that actually exist in
// the table instead of only a free-text "contains…" match. The envelope mirrors
// the other dynamic handlers: {success, data:[{value,label,count}], meta}.
//
//	GET /:model/facets?field=<col>&q=<text>&limit=<n>
func (h *Handler) facets(c fiber.Ctx) error {
	u := h.user(c)
	if u == nil {
		return respondErr(c, fiber.StatusUnauthorized, "not authenticated")
	}
	q := FacetsQuery{
		Model: c.Params("model"),
		Field: c.Query("field"),
		Q:     c.Query("q"),
	}
	if v := c.Query("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			q.Limit = n
		}
	}
	buckets, err := h.service.Facets(c, u, q)
	if err != nil {
		return h.handleError(c, err)
	}
	return c.JSON(fiber.Map{
		"success": true,
		"data":    buckets,
		"meta": fiber.Map{
			"count": len(buckets),
			"field": q.Field,
		},
	})
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

func (h *Handler) action(c fiber.Ctx) error {
	u := h.user(c)
	if u == nil {
		return respondErr(c, fiber.StatusUnauthorized, "not authenticated")
	}
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return respondErr(c, fiber.StatusBadRequest, ErrInvalidID.Error())
	}
	var payload map[string]any
	if len(c.Body()) > 0 {
		if err := c.Bind().Body(&payload); err != nil {
			return respondErr(c, fiber.StatusBadRequest, "invalid body")
		}
	}
	if payload == nil {
		payload = map[string]any{}
	}
	res, err := h.service.ExecAction(c, c.Params("model"), u, id, c.Params("key"), payload)
	if err != nil {
		return h.handleError(c, err)
	}
	body := fiber.Map{"success": res.Success, "meta": res.Meta}
	if res.Success {
		body["data"] = res.Data
	} else if res.Error != nil {
		body["error"] = res.Error
	}
	status := res.HTTPStatus
	if status == 0 {
		status = fiber.StatusOK
	}
	return c.Status(status).JSON(body)
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
	// v0.9.0: normalize the options envelope to {success, data, meta} so the
	// endpoint matches every other dynamic handler. The previous shape
	// {success, data, type} surfaced `type` as a sibling of `data` which
	// forced consumers to special-case this route. The discriminator is now
	// nested under `meta.type` alongside the count, mirroring how list
	// endpoints carry pagination meta. BREAKING — bumped in CHANGELOG.
	return c.JSON(fiber.Map{
		"success": true,
		"data":    res.Options,
		"meta": fiber.Map{
			"type":  res.Type,
			"count": len(res.Options),
		},
	})
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
	// Structured field-validation failure: 422 with the per-column code map the
	// SDK localizes. Must precede the flat respondErr cases so the field map is
	// preserved instead of being flattened to a single message.
	var ve *ValidationError
	if errors.As(err, &ve) {
		return c.Status(fiber.StatusUnprocessableEntity).JSON(fiber.Map{
			"success": false,
			"message": "validation failed",
			"errors":  ve.Fields,
		})
	}
	if errors.Is(err, ErrUnsupportedTriggerType) {
		return respondErr(c, fiber.StatusNotImplemented, err.Error())
	}
	if errors.Is(err, ErrInvalidState) {
		return respondErr(c, fiber.StatusConflict, err.Error())
	}
	if errors.Is(err, ErrInvalidTransition) {
		return respondErr(c, fiber.StatusUnprocessableEntity, err.Error())
	}
	if errors.Is(err, ErrConstraintViolation) {
		return respondErr(c, fiber.StatusUnprocessableEntity, err.Error())
	}
	// errors.Is BEFORE the identity switch: a wrapped ErrForbidden (e.g.
	// ErrPermissionServiceMissing) must still answer 403, not 500. A denial
	// reported as a server error reads as "our bug" instead of "not allowed".
	if errors.Is(err, ErrForbidden) {
		return respondErr(c, fiber.StatusForbidden, err.Error())
	}
	switch err {
	case ErrModelNotFound, ErrRecordNotFound, ErrSourceModelNotFound, ErrOptionsFieldNotFound, ErrActionNotFound:
		return respondErr(c, fiber.StatusNotFound, err.Error())
	case ErrForbidden:
		return respondErr(c, fiber.StatusForbidden, err.Error())
	case ErrFieldRequired, ErrInvalidInput:
		return respondErr(c, fiber.StatusBadRequest, err.Error())
	case ErrNoOptionsConfig, ErrNoSearchConfig, ErrNoActionResolver:
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
