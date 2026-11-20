package automation

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
)

// IdentityResolver extracts the authenticated identity from a request (the
// host's auth middleware already ran).
type IdentityResolver func(c fiber.Ctx) (Identity, error)

// Handler exposes the automation API. Mount it under the host's authenticated
// group, e.g. api.Group("/automations").
//
//	GET    /                 list (own; ?all=1 for managers, ?kind=, ?status=)
//	GET    /catalog          tools the person may compose + JSON Schema of the spec
//	POST   /                 create {kind, spec, propose}
//	GET    /:id              detail
//	GET    /:id/runs         latest runs
//	PATCH  /:id              update {spec, version}
//	POST   /:id/confirm      confirm a proposal
//	POST   /:id/pause
//	POST   /:id/resume
//	POST   /:id/run          {dry_run}
//	DELETE /:id              archive
type Handler struct {
	svc      *Service
	identity IdentityResolver
}

// NewHandler builds the HTTP handler.
func NewHandler(svc *Service, identity IdentityResolver) *Handler {
	return &Handler{svc: svc, identity: identity}
}

// Mount installs the routes.
func (h *Handler) Mount(r fiber.Router) {
	r.Get("/", h.list)
	r.Get("/catalog", h.catalog)
	r.Post("/", h.create)
	r.Get("/:id", h.get)
	r.Get("/:id/runs", h.runs)
	r.Patch("/:id", h.update)
	r.Post("/:id/confirm", h.confirm)
	r.Post("/:id/pause", h.pause)
	r.Post("/:id/resume", h.resume)
	r.Post("/:id/run", h.runNow)
	r.Delete("/:id", h.archive)
}

func (h *Handler) ident(c fiber.Ctx) (Identity, error) {
	if h.identity == nil {
		return Identity{}, ErrForbidden
	}
	id, err := h.identity(c)
	if err != nil || id.OrganizationID == uuid.Nil || id.UserID == uuid.Nil {
		return Identity{}, ErrForbidden
	}
	return id, nil
}

func (h *Handler) list(c fiber.Ctx) error {
	id, err := h.ident(c)
	if err != nil {
		return fail(c, err)
	}
	list, err := h.svc.List(c, id, ListFilter{Kind: c.Query("kind"), Status: c.Query("status"), All: c.Query("all") == "1"})
	if err != nil {
		return fail(c, err)
	}
	return ok(c, h.svc.Views(c, list))
}

func (h *Handler) catalog(c fiber.Ctx) error {
	id, err := h.ident(c)
	if err != nil {
		return fail(c, err)
	}
	p := h.svc.Policy()
	return ok(c, fiber.Map{
		"tools":         h.svc.CatalogFor(c, id),
		"schema":        json.RawMessage(SchemaJSON()),
		"channels":      p.Channels,
		"timezone":      p.DefaultTimezone,
		"min_interval":  p.MinInterval.String(),
		"max_per_user":  p.MaxPerUser,
		"llm_available": h.svc.summarizer != nil && p.DailyBudgetUSD > 0,
	})
}

type createBody struct {
	Kind    string          `json:"kind"`
	Spec    json.RawMessage `json:"spec"`
	Propose bool            `json:"propose"`
}

func (h *Handler) create(c fiber.Ctx) error {
	id, err := h.ident(c)
	if err != nil {
		return fail(c, err)
	}
	var b createBody
	if err := json.Unmarshal(c.Body(), &b); err != nil {
		return fail(c, invalid("invalid body"))
	}
	a, err := h.svc.Create(c, id, CreateInput{Kind: b.Kind, Spec: b.Spec, Origin: OriginUI, Propose: b.Propose})
	if err != nil {
		return fail(c, err)
	}
	return c.Status(fiber.StatusCreated).JSON(fiber.Map{"success": true, "data": h.svc.ViewOf(c, a)})
}

func (h *Handler) withID(c fiber.Ctx) (Identity, uuid.UUID, error) {
	id, err := h.ident(c)
	if err != nil {
		return id, uuid.Nil, err
	}
	autoID, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return id, uuid.Nil, ErrNotFound
	}
	return id, autoID, nil
}

func (h *Handler) get(c fiber.Ctx) error {
	id, autoID, err := h.withID(c)
	if err != nil {
		return fail(c, err)
	}
	a, err := h.svc.Get(c, id, autoID)
	if err != nil {
		return fail(c, err)
	}
	return ok(c, h.svc.ViewOf(c, a))
}

func (h *Handler) runs(c fiber.Ctx) error {
	id, autoID, err := h.withID(c)
	if err != nil {
		return fail(c, err)
	}
	runs, err := h.svc.Runs(c, id, autoID, fiber.Query[int](c, "limit", 30))
	if err != nil {
		return fail(c, err)
	}
	return ok(c, RunViews(runs))
}

type updateBody struct {
	Spec    json.RawMessage `json:"spec"`
	Version int             `json:"version"`
}

func (h *Handler) update(c fiber.Ctx) error {
	id, autoID, err := h.withID(c)
	if err != nil {
		return fail(c, err)
	}
	var b updateBody
	if err := json.Unmarshal(c.Body(), &b); err != nil {
		return fail(c, invalid("invalid body"))
	}
	a, err := h.svc.Update(c, id, autoID, UpdateInput{Spec: b.Spec, ExpectedVersion: b.Version})
	if err != nil {
		return fail(c, err)
	}
	return ok(c, h.svc.ViewOf(c, a))
}

func (h *Handler) confirm(c fiber.Ctx) error { return h.act(c, h.svc.Confirm) }
func (h *Handler) pause(c fiber.Ctx) error   { return h.act(c, h.svc.Pause) }
func (h *Handler) resume(c fiber.Ctx) error  { return h.act(c, h.svc.Resume) }
func (h *Handler) archive(c fiber.Ctx) error { return h.act(c, h.svc.Archive) }

func (h *Handler) act(c fiber.Ctx, fn func(ctx context.Context, id Identity, autoID uuid.UUID) (*Automation, error)) error {
	id, autoID, err := h.withID(c)
	if err != nil {
		return fail(c, err)
	}
	a, err := fn(c, id, autoID)
	if err != nil {
		return fail(c, err)
	}
	return ok(c, h.svc.ViewOf(c, a))
}

func (h *Handler) runNow(c fiber.Ctx) error {
	id, autoID, err := h.withID(c)
	if err != nil {
		return fail(c, err)
	}
	var b struct {
		DryRun *bool `json:"dry_run"`
	}
	_ = json.Unmarshal(c.Body(), &b)
	dry := b.DryRun == nil || *b.DryRun
	run, out, err := h.svc.RunNow(c, id, autoID, dry)
	if run == nil && err != nil {
		return fail(c, err)
	}
	resp := fiber.Map{"run": RunViews([]Run{*run})[0], "output": out}
	if err != nil {
		resp["error"] = err.Error()
	}
	return ok(c, resp)
}

func ok(c fiber.Ctx, data any) error {
	return c.JSON(fiber.Map{"success": true, "data": data})
}

func fail(c fiber.Ctx, err error) error {
	status := fiber.StatusInternalServerError
	msg := "error interno"
	switch {
	case errors.Is(err, ErrInvalid):
		status, msg = fiber.StatusUnprocessableEntity, err.Error()
	case errors.Is(err, ErrNotFound):
		status, msg = fiber.StatusNotFound, ErrNotFound.Error()
	case errors.Is(err, ErrForbidden):
		status, msg = fiber.StatusForbidden, ErrForbidden.Error()
	case errors.Is(err, ErrLimit):
		status, msg = fiber.StatusTooManyRequests, err.Error()
	case errors.Is(err, ErrConflict), errors.Is(err, ErrBadState):
		status, msg = fiber.StatusConflict, err.Error()
	case errors.Is(err, ErrDisabled):
		status, msg = fiber.StatusServiceUnavailable, err.Error()
	}
	return c.Status(status).JSON(fiber.Map{"success": false, "message": msg})
}
