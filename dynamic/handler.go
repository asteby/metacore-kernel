package dynamic

import (
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
	g := r.Group("/dynamic", middleware...)
	g.Get("/:model", h.list)
	g.Post("/:model", h.create)
	g.Get("/:model/:id", h.get)
	g.Put("/:model/:id", h.update)
	g.Delete("/:model/:id", h.delete)
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

func (h *Handler) handleError(c *fiber.Ctx, err error) error {
	switch err {
	case ErrModelNotFound:
		return respondErr(c, fiber.StatusNotFound, err.Error())
	case ErrRecordNotFound:
		return respondErr(c, fiber.StatusNotFound, err.Error())
	case ErrForbidden:
		return respondErr(c, fiber.StatusForbidden, err.Error())
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
