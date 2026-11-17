package webhooks

import (
	"encoding/json"
	"errors"
	"strconv"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
)

// OwnerResolver extracts (ownerType, ownerID) from the request context.
// Apps wire their own: e.g. one host may pull device_id from URL params,
// another may pull organization_id from JWT claims.
type OwnerResolver func(c fiber.Ctx) (string, uuid.UUID, error)

// Handler exposes the webhook CRUD over Fiber. EVERY route is scoped to the
// owner the resolver returns: a webhook or delivery of another owner is a 404,
// indistinguishable from a missing one.
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
//	POST   /:id/enable     Enable a declarative subscription (releases retained events)
//	POST   /:id/disable    Disable it (new events are retained)
//	GET    /:id/logs       Logs (paginated)
//	POST   /logs/:id/replay Replay a prior delivery
//	POST   /logs/:id/requeue Put a dead-lettered delivery back in the queue
func (h *Handler) Mount(r fiber.Router) {
	r.Get("/", h.list)
	r.Post("/", h.create)
	r.Get("/:id", h.get)
	r.Put("/:id", h.update)
	r.Delete("/:id", h.delete)
	r.Post("/:id/test", h.test)
	r.Post("/:id/enable", h.setEnabled(true))
	r.Post("/:id/disable", h.setEnabled(false))
	r.Get("/:id/logs", h.logs)
	r.Post("/logs/:id/replay", h.replay)
	r.Post("/logs/:id/requeue", h.requeue)
}

// webhookInput is the ONLY shape a client may write: an allow-list. Owner,
// organization, id, secret, counters, health and timestamps are never taken
// from the body. Declarative is kept raw so "absent" and an explicit null can be
// told apart.
type webhookInput struct {
	Name        *string         `json:"name"`
	URL         *string         `json:"url"`
	Events      *StringSlice    `json:"events"`
	Active      *bool           `json:"active"`
	RetryMax    *int            `json:"retry_max"`
	TimeoutSec  *int            `json:"timeout_sec"`
	Declarative json.RawMessage `json:"declarative"`
}

// apply copies the allowed fields onto w.
func (in *webhookInput) apply(w *Webhook) error {
	if in.Name != nil {
		w.Name = *in.Name
	}
	if in.URL != nil {
		w.URL = *in.URL
	}
	if in.Events != nil {
		w.Events = *in.Events
	}
	if in.Active != nil {
		w.Active = *in.Active
	}
	if in.RetryMax != nil {
		w.RetryMax = *in.RetryMax
	}
	if in.TimeoutSec != nil {
		w.TimeoutSec = *in.TimeoutSec
	}
	if len(in.Declarative) > 0 {
		if string(in.Declarative) == "null" {
			if w.Declarative != nil {
				return errors.New("a declarative subscription cannot become a classic webhook")
			}
			return nil
		}
		var d Declarative
		if w.Declarative != nil {
			d = *w.Declarative // partial update: absent fields keep their value
		}
		if err := json.Unmarshal(in.Declarative, &d); err != nil {
			return errors.New("invalid declarative configuration")
		}
		w.Declarative = &d
	}
	return nil
}

// owned resolves the owner and loads the webhook only if it is theirs.
func (h *Handler) owned(c fiber.Ctx) (*Webhook, error) {
	ot, oid, err := h.resolver(c)
	if err != nil {
		return nil, fiber.NewError(fiber.StatusBadRequest, err.Error())
	}
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return nil, fiber.NewError(fiber.StatusBadRequest, "invalid id")
	}
	w, err := h.service.GetOwned(c, ot, oid, id)
	if err == ErrWebhookNotFound {
		return nil, fiber.NewError(fiber.StatusNotFound, err.Error())
	}
	if err != nil {
		return nil, fiber.NewError(fiber.StatusInternalServerError, err.Error())
	}
	return w, nil
}

// ownedDelivery resolves the owner and loads the delivery only if its webhook is theirs.
func (h *Handler) ownedDelivery(c fiber.Ctx) (*WebhookDelivery, error) {
	ot, oid, err := h.resolver(c)
	if err != nil {
		return nil, fiber.NewError(fiber.StatusBadRequest, err.Error())
	}
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return nil, fiber.NewError(fiber.StatusBadRequest, "invalid id")
	}
	d, err := h.service.DeliveryOwned(c, ot, oid, id)
	if err == ErrDeliveryNotFound {
		return nil, fiber.NewError(fiber.StatusNotFound, err.Error())
	}
	if err != nil {
		return nil, fiber.NewError(fiber.StatusInternalServerError, err.Error())
	}
	return d, nil
}

func replyErr(c fiber.Ctx, err error) error {
	var fe *fiber.Error
	if errors.As(err, &fe) {
		return fail(c, fe.Code, fe.Message)
	}
	return fail(c, fiber.StatusInternalServerError, err.Error())
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
	var in webhookInput
	if err := c.Bind().Body(&in); err != nil {
		return fail(c, fiber.StatusBadRequest, "invalid body")
	}
	w := Webhook{Active: true}
	if err := in.apply(&w); err != nil {
		return fail(c, fiber.StatusBadRequest, err.Error())
	}
	w.OwnerType = ot
	w.OwnerID = oid
	if ot == "organization" {
		w.OrganizationID = oid
	}
	if err := h.service.Create(c, &w); err != nil {
		return fail(c, fiber.StatusBadRequest, err.Error())
	}
	return ok(c, w)
}

func (h *Handler) get(c fiber.Ctx) error {
	w, err := h.owned(c)
	if err != nil {
		return replyErr(c, err)
	}
	return ok(c, w)
}

func (h *Handler) update(c fiber.Ctx) error {
	existing, err := h.owned(c)
	if err != nil {
		return replyErr(c, err)
	}
	var in webhookInput
	if err := c.Bind().Body(&in); err != nil {
		return fail(c, fiber.StatusBadRequest, "invalid body")
	}
	if err := in.apply(existing); err != nil {
		return fail(c, fiber.StatusBadRequest, err.Error())
	}
	if err := h.service.Update(c, existing); err != nil {
		return fail(c, fiber.StatusBadRequest, err.Error())
	}
	fresh, err := h.service.GetOwned(c, existing.OwnerType, existing.OwnerID, existing.ID)
	if err != nil {
		return fail(c, fiber.StatusInternalServerError, err.Error())
	}
	return ok(c, fresh)
}

func (h *Handler) delete(c fiber.Ctx) error {
	w, err := h.owned(c)
	if err != nil {
		return replyErr(c, err)
	}
	if err := h.service.Delete(c, w.ID); err != nil {
		return fail(c, fiber.StatusInternalServerError, err.Error())
	}
	return ok(c, nil)
}

func (h *Handler) test(c fiber.Ctx) error {
	w, err := h.owned(c)
	if err != nil {
		return replyErr(c, err)
	}
	d, err := h.service.Test(c, w.ID)
	if err == ErrWebhookNotFound {
		return fail(c, fiber.StatusNotFound, err.Error())
	}
	if err != nil {
		return fail(c, fiber.StatusInternalServerError, err.Error())
	}
	return ok(c, d)
}

func (h *Handler) logs(c fiber.Ctx) error {
	w, err := h.owned(c)
	if err != nil {
		return replyErr(c, err)
	}
	page, _ := strconv.Atoi(c.Query("page", "1"))
	perPage, _ := strconv.Atoi(c.Query("per_page", "50"))
	params := LogsParams{Page: page, PerPage: perPage, Event: c.Query("event")}
	items, total, err := h.service.Logs(c, w.ID, params)
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
	od, err := h.ownedDelivery(c)
	if err != nil {
		return replyErr(c, err)
	}
	d, err := h.service.Replay(c, od.ID)
	switch {
	case err == nil:
		return ok(c, d)
	case err == ErrDeliveryNotFound || err == ErrWebhookNotFound:
		return fail(c, fiber.StatusNotFound, err.Error())
	case errors.Is(err, ErrPayloadPurged):
		return fail(c, fiber.StatusConflict, err.Error())
	}
	return fail(c, fiber.StatusInternalServerError, err.Error())
}

// setEnabled toggles a declarative subscription. Enabling releases the events
// that were retained while it was off and clears an auto-disable.
func (h *Handler) setEnabled(enabled bool) fiber.Handler {
	return func(c fiber.Ctx) error {
		w, err := h.owned(c)
		if err != nil {
			return replyErr(c, err)
		}
		released, err := h.service.SetEnabled(c, w.ID, enabled)
		switch err {
		case nil:
			return ok(c, fiber.Map{"enabled": enabled, "released": released})
		case ErrWebhookNotFound:
			return fail(c, fiber.StatusNotFound, err.Error())
		case ErrNotDeclarative:
			return fail(c, fiber.StatusBadRequest, err.Error())
		}
		return fail(c, fiber.StatusInternalServerError, err.Error())
	}
}

// requeue puts a dead-lettered delivery back in the queue (manual repeat).
func (h *Handler) requeue(c fiber.Ctx) error {
	od, err := h.ownedDelivery(c)
	if err != nil {
		return replyErr(c, err)
	}
	switch err := h.service.Requeue(c, od.ID); {
	case err == nil:
		return ok(c, nil)
	case err == ErrDeliveryNotFound:
		return fail(c, fiber.StatusNotFound, err.Error())
	case errors.Is(err, ErrNotDead), errors.Is(err, ErrPayloadPurged):
		return fail(c, fiber.StatusConflict, err.Error())
	default:
		return fail(c, fiber.StatusInternalServerError, err.Error())
	}
}

func ok(c fiber.Ctx, data any) error {
	return c.JSON(fiber.Map{"success": true, "data": data})
}

func fail(c fiber.Ctx, status int, msg string) error {
	return c.Status(status).JSON(fiber.Map{"success": false, "message": msg})
}
