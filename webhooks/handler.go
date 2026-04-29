package webhooks

import (
	"strconv"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
)

// OwnerResolver extracts (ownerType, ownerID) from the request context.
// Apps wire their own: e.g. one host may pull device_id from URL params,
// another may pull organization_id from JWT claims.
type OwnerResolver func(c fiber.Ctx) (string, uuid.UUID, error)

// Handler exposes the webhook CRUD over Fiber.
type Handler struct {
	service  *Service
	resolver OwnerResolver
}

func NewHandler(service *Service, resolver OwnerResolver) *Handler {
	return &Handler{service: service, resolver: resolver}
}

// Mount registers the CRUD routes on the given router.
//
//	GET    /               List
//	POST   /               Create
//	GET    /:id            Get
//	PUT    /:id            Update
//	DELETE /:id            Delete
//	POST   /:id/test       Test (synchronous single delivery)
//	GET    /:id/logs       Logs (paginated)
//	POST   /logs/:id/replay Replay a prior delivery
func (h *Handler) Mount(r fiber.Router) {
	r.Get("/", h.list)
	r.Post("/", h.create)
	r.Get("/:id", h.get)
	r.Put("/:id", h.update)
	r.Delete("/:id", h.delete)
	r.Post("/:id/test", h.test)
	r.Get("/:id/logs", h.logs)
	r.Post("/logs/:id/replay", h.replay)
}

func (h *Handler) list(c fiber.Ctx) error {
	ot, oid, err := h.resolver(c)
	if err != nil {
		return fail(c, fiber.StatusBadRequest, err.Error())
	}
	items, err := h.service.List(c, ot, oid)
	if err != nil {
		return fail(c, fiber.StatusInternalServerError, err.Error())
	}
	return ok(c, items)
}

func (h *Handler) create(c fiber.Ctx) error {
	ot, oid, err := h.resolver(c)
	if err != nil {
		return fail(c, fiber.StatusBadRequest, err.Error())
	}
	var w Webhook
	if err := c.Bind().Body(&w); err != nil {
		return fail(c, fiber.StatusBadRequest, "invalid body")
	}
	w.OwnerType = ot
	w.OwnerID = oid
	if err := h.service.Create(c, &w); err != nil {
		return fail(c, fiber.StatusBadRequest, err.Error())
	}
	return ok(c, w)
}

func (h *Handler) get(c fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return fail(c, fiber.StatusBadRequest, "invalid id")
	}
	w, err := h.service.Get(c, id)
	if err == ErrWebhookNotFound {
		return fail(c, fiber.StatusNotFound, err.Error())
	}
	if err != nil {
		return fail(c, fiber.StatusInternalServerError, err.Error())
	}
	return ok(c, w)
}

func (h *Handler) update(c fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return fail(c, fiber.StatusBadRequest, "invalid id")
	}
	existing, err := h.service.Get(c, id)
	if err == ErrWebhookNotFound {
		return fail(c, fiber.StatusNotFound, err.Error())
	}
	if err != nil {
		return fail(c, fiber.StatusInternalServerError, err.Error())
	}
	if err := c.Bind().Body(existing); err != nil {
		return fail(c, fiber.StatusBadRequest, "invalid body")
	}
	existing.ID = id
	if err := h.service.Update(c, existing); err != nil {
		return fail(c, fiber.StatusBadRequest, err.Error())
	}
	return ok(c, existing)
}

func (h *Handler) delete(c fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return fail(c, fiber.StatusBadRequest, "invalid id")
	}
	if err := h.service.Delete(c, id); err != nil {
		return fail(c, fiber.StatusInternalServerError, err.Error())
	}
	return ok(c, nil)
}

func (h *Handler) test(c fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return fail(c, fiber.StatusBadRequest, "invalid id")
	}
	d, err := h.service.Test(c, id)
	if err == ErrWebhookNotFound {
		return fail(c, fiber.StatusNotFound, err.Error())
	}
	if err != nil {
		return fail(c, fiber.StatusInternalServerError, err.Error())
	}
	return ok(c, d)
}

func (h *Handler) logs(c fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return fail(c, fiber.StatusBadRequest, "invalid id")
	}
	page, _ := strconv.Atoi(c.Query("page", "1"))
	perPage, _ := strconv.Atoi(c.Query("per_page", "50"))
	params := LogsParams{Page: page, PerPage: perPage, Event: c.Query("event")}
	items, total, err := h.service.Logs(c, id, params)
	if err != nil {
		return fail(c, fiber.StatusInternalServerError, err.Error())
	}
	return c.JSON(fiber.Map{
		"success": true,
		"data":    items,
		"meta":    fiber.Map{"total": total, "page": page, "per_page": perPage},
	})
}

func (h *Handler) replay(c fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return fail(c, fiber.StatusBadRequest, "invalid id")
	}
	d, err := h.service.Replay(c, id)
	if err == ErrDeliveryNotFound {
		return fail(c, fiber.StatusNotFound, err.Error())
	}
	if err != nil {
		return fail(c, fiber.StatusInternalServerError, err.Error())
	}
	return ok(c, d)
}

func ok(c fiber.Ctx, data any) error {
	return c.JSON(fiber.Map{"success": true, "data": data})
}

func fail(c fiber.Ctx, status int, msg string) error {
	return c.Status(status).JSON(fiber.Map{"success": false, "message": msg})
}
