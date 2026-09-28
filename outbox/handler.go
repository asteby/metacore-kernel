package outbox

import (
	"errors"
	"strconv"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
)

// ScopeResolver returns the organization the caller may see. A nil org
// with a nil error means "every organization" (platform operators); an
// error answers 403. Mount the handler behind your own auth/permission
// middleware — the scope only narrows what an allowed caller sees.
type ScopeResolver func(c fiber.Ctx) (*uuid.UUID, error)

// Handler is the optional admin HTTP surface over a Service. The service
// works without it (Law 3).
type Handler struct {
	svc   *Service
	scope ScopeResolver
}

// NewHandler builds the handler. A nil scope sees every organization.
func NewHandler(svc *Service, scope ScopeResolver) *Handler {
	if scope == nil {
		scope = func(fiber.Ctx) (*uuid.UUID, error) { return nil, nil }
	}
	return &Handler{svc: svc, scope: scope}
}

// Mount registers the routes on r:
//
//	GET  /jobs             ?status=&kind=&limit=&offset=  → {data: [...], meta: {total, limit, offset}}
//	GET  /jobs/:id
//	POST /jobs/:id/retry
//
// Example:
//
//	outbox.NewHandler(app.Outbox, func(c fiber.Ctx) (*uuid.UUID, error) {
//	    org := auth.GetOrganizationID(c)
//	    return &org, nil
//	}).Mount(authed.Group("/admin/outbox", requireAdmin))
func (h *Handler) Mount(r fiber.Router) {
	r.Get("/jobs", h.list)
	r.Get("/jobs/:id", h.get)
	r.Post("/jobs/:id/retry", h.retry)
}

func (h *Handler) list(c fiber.Ctx) error {
	org, err := h.scope(c)
	if err != nil {
		return fail(c, fiber.StatusForbidden, err.Error())
	}
	f := Filter{OrgID: org, Kind: c.Query("kind"), Status: Status(c.Query("status"))}
	if f.Status != "" && !f.Status.Valid() {
		return fail(c, fiber.StatusBadRequest, "invalid status")
	}
	f.Limit, _ = strconv.Atoi(c.Query("limit"))
	f.Offset, _ = strconv.Atoi(c.Query("offset"))
	jobs, total, err := h.svc.List(c.Context(), f)
	if err != nil {
		return fail(c, fiber.StatusInternalServerError, err.Error())
	}
	return c.JSON(fiber.Map{
		"success": true,
		"data":    jobs,
		"meta":    fiber.Map{"total": total, "limit": f.Limit, "offset": f.Offset},
	})
}

func (h *Handler) get(c fiber.Ctx) error {
	job, status, msg := h.load(c)
	if status != 0 {
		return fail(c, status, msg)
	}
	return c.JSON(fiber.Map{"success": true, "data": job})
}

func (h *Handler) retry(c fiber.Ctx) error {
	job, status, msg := h.load(c)
	if status != 0 {
		return fail(c, status, msg)
	}
	job, err := h.svc.Retry(c.Context(), job.ID)
	switch {
	case errors.Is(err, ErrNotFound):
		return fail(c, fiber.StatusNotFound, err.Error())
	case errors.Is(err, ErrNotRetryable), errors.Is(err, ErrDuplicate):
		return fail(c, fiber.StatusConflict, err.Error())
	case err != nil:
		return fail(c, fiber.StatusInternalServerError, err.Error())
	}
	return c.JSON(fiber.Map{"success": true, "data": job})
}

// load resolves :id within the caller's scope. A job of another org reads
// as 404 so ids do not leak across tenants.
func (h *Handler) load(c fiber.Ctx) (Job, int, string) {
	org, err := h.scope(c)
	if err != nil {
		return Job{}, fiber.StatusForbidden, err.Error()
	}
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return Job{}, fiber.StatusBadRequest, "invalid id"
	}
	job, err := h.svc.Get(c.Context(), id)
	if errors.Is(err, ErrNotFound) || (err == nil && org != nil && (job.OrgID == nil || *job.OrgID != *org)) {
		return Job{}, fiber.StatusNotFound, ErrNotFound.Error()
	}
	if err != nil {
		return Job{}, fiber.StatusInternalServerError, err.Error()
	}
	return job, 0, ""
}

func fail(c fiber.Ctx, status int, msg string) error {
	return c.Status(status).JSON(fiber.Map{"success": false, "message": msg})
}
