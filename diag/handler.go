package diag

import (
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"

	"github.com/asteby/metacore-kernel/httpx"
)

// PermView es el permiso que gobierna toda la API: platform.diagnostics.view.
// El host lo exige con su propio middleware (por ejemplo
// permission.Gate(svc, lookup, permission.Cap("platform.diagnostics", "view"))
// o, de respaldo, su RequireAdmin), pasado a Mount.
const PermView = "platform.diagnostics.view"

// PermManage es el permiso que gobierna resolver y silenciar incidencias:
// platform.diagnostics.manage. Ver con PermView no basta para cambiar su estado.
const PermManage = "platform.diagnostics.manage"

// HandlerOptions de la API.
type HandlerOptions struct {
	// Manage son los middlewares ADICIONALES (tras los de Mount) que protegen
	// POST /issues/:id/resolve y /mute, p. ej.
	// permission.Gate(svc, lookup, permission.Cap("platform.diagnostics", "manage")).
	// Falla cerrado: sin ninguno, esas dos rutas no se montan y la API es de
	// solo lectura.
	Manage []fiber.Handler
	// IsSuperAdmin decide si la peticion puede mirar otra organizacion con
	// ?org_id=. Nil = nadie (solo la organizacion propia).
	IsSuperAdmin func(c fiber.Ctx) bool
	// OrgFrom resuelve la organizacion de la peticion. Nil = el local
	// "organization_id" que ponen los middlewares de auth del kernel.
	OrgFrom func(c fiber.Ctx) (uuid.UUID, error)
	// Versions lista addon_key -> version instalada para el support bundle.
	Versions func(c fiber.Ctx) map[string]string
	// KernelVersion se incluye en el support bundle.
	KernelVersion string
	Now           func() time.Time
}

// Handler expone la API de diagnosticos.
type Handler struct {
	store *Store
	opt   HandlerOptions
}

// NewHandler crea el Handler.
func NewHandler(store *Store, opt HandlerOptions) *Handler {
	if opt.Now == nil {
		opt.Now = time.Now
	}
	return &Handler{store: store, opt: opt}
}

// Mount registra las rutas relativas a r (el host elige el prefijo, p.ej.
// app.Group("/api/diagnostics")):
//
//	GET  /issues?addon=&code=&severity=&status=&since=
//	GET  /issues/:id                       (con muestras)
//	GET  /events?correlation_id= | subject_type=&subject_id= | diag_ref=
//	POST /issues/:id/resolve               (solo con HandlerOptions.Manage)
//	POST /issues/:id/mute                  (solo con HandlerOptions.Manage)
//	GET  /export                           (support bundle, sin ajustes)
//
// mw (PermView) se aplica a TODAS las rutas y es obligatorio: sin al menos un middleware
// de acceso Mount devuelve error (falla cerrado) en vez de exponer diagnosticos.
func (h *Handler) Mount(r fiber.Router, mw ...fiber.Handler) error {
	var chain []any
	for _, m := range mw {
		if m != nil {
			chain = append(chain, m)
		}
	}
	if len(chain) == 0 {
		return errors.New("diag: Mount requires an access middleware (" + PermView + " or admin)")
	}
	reg := func(add func(string, any, ...any) fiber.Router, path string, final fiber.Handler) {
		hs := append(append([]any{}, chain[1:]...), final)
		add(path, chain[0], hs...)
	}
	reg(r.Get, "/issues", h.list)
	reg(r.Get, "/issues/:id", h.get)
	reg(r.Get, "/events", h.events)
	if manage := compact(h.opt.Manage); len(manage) > 0 {
		// Cambiar el estado exige PermManage ademas de PermView.
		regm := func(path string, final fiber.Handler) {
			hs := append([]any{}, chain[1:]...)
			for _, m := range manage {
				hs = append(hs, m)
			}
			add := r.Post
			add(path, chain[0], append(hs, final)...)
		}
		regm("/issues/:id/resolve", h.setStatus(StatusResolved))
		regm("/issues/:id/mute", h.setStatus(StatusMuted))
	}
	reg(r.Get, "/export", h.export)
	return nil
}

func compact(in []fiber.Handler) []fiber.Handler {
	var out []fiber.Handler
	for _, m := range in {
		if m != nil {
			out = append(out, m)
		}
	}
	return out
}

type fiberLocals struct{ c fiber.Ctx }

func (l fiberLocals) Locals(key string) any { return l.c.Locals(key) }

func (h *Handler) org(c fiber.Ctx) (uuid.UUID, error) {
	var own uuid.UUID
	var err error
	if h.opt.OrgFrom != nil {
		own, err = h.opt.OrgFrom(c)
	} else {
		own, err = httpx.ExtractOrgID(fiberLocals{c})
	}
	if err != nil || own == uuid.Nil {
		return uuid.Nil, fiber.NewError(fiber.StatusUnauthorized, "organization required")
	}
	if q := c.Query("org_id"); q != "" {
		want, perr := uuid.Parse(q)
		if perr != nil {
			return uuid.Nil, fiber.NewError(fiber.StatusBadRequest, "invalid org_id")
		}
		if want != own {
			if h.opt.IsSuperAdmin == nil || !h.opt.IsSuperAdmin(c) {
				return uuid.Nil, fiber.NewError(fiber.StatusForbidden, "cross-organization access requires superadmin")
			}
			return want, nil
		}
	}
	return own, nil
}

func fail(c fiber.Ctx, err error) error {
	var fe *fiber.Error
	status := fiber.StatusInternalServerError
	msg := "internal error"
	switch {
	case errors.As(err, &fe):
		status, msg = fe.Code, fe.Message
	case errors.Is(err, ErrNotFound):
		status, msg = fiber.StatusNotFound, "not found"
	case errors.Is(err, ErrBadStatus):
		status, msg = fiber.StatusBadRequest, "invalid status"
	case errors.Is(err, ErrOrgRequired):
		status, msg = fiber.StatusUnauthorized, "organization required"
	}
	return c.Status(status).JSON(fiber.Map{"success": false, "error": fiber.Map{"code": errCode(status), "message": msg}})
}

func errCode(status int) string {
	switch status {
	case 400:
		return "bad_request"
	case 401:
		return "unauthorized"
	case 403:
		return "forbidden"
	case 404:
		return "not_found"
	}
	return "internal_error"
}

func ok(c fiber.Ctx, data any, meta fiber.Map) error {
	return c.JSON(fiber.Map{"success": true, "data": data, "meta": meta})
}

func (h *Handler) list(c fiber.Ctx) error {
	org, err := h.org(c)
	if err != nil {
		return fail(c, err)
	}
	f := Filter{Org: org, Addon: c.Query("addon"), Code: c.Query("code"), Severity: c.Query("severity"), Status: c.Query("status")}
	if s := c.Query("since"); s != "" {
		t, perr := time.Parse(time.RFC3339, s)
		if perr != nil {
			return fail(c, fiber.NewError(fiber.StatusBadRequest, "since must be RFC3339"))
		}
		f.Since = t
	}
	f.Limit, _ = strconv.Atoi(c.Query("limit"))
	f.Offset, _ = strconv.Atoi(c.Query("offset"))
	rows, total, err := h.store.List(c.Context(), f)
	if err != nil {
		return fail(c, err)
	}
	if rows == nil {
		rows = []Group{}
	}
	return ok(c, rows, fiber.Map{"total": total})
}

func (h *Handler) get(c fiber.Ctx) error {
	org, err := h.org(c)
	if err != nil {
		return fail(c, err)
	}
	id, perr := uuid.Parse(c.Params("id"))
	if perr != nil {
		return fail(c, fiber.NewError(fiber.StatusBadRequest, "invalid id"))
	}
	g, evs, err := h.store.Get(c.Context(), org, id)
	if err != nil {
		return fail(c, err)
	}
	return ok(c, fiber.Map{"issue": g, "events": evs}, nil)
}

func (h *Handler) events(c fiber.Ctx) error {
	org, err := h.org(c)
	if err != nil {
		return fail(c, err)
	}
	q := EventQuery{Org: org, CorrelationID: c.Query("correlation_id"), SubjectType: c.Query("subject_type"),
		SubjectID: c.Query("subject_id"), DiagRef: c.Query("diag_ref")}
	q.Limit, _ = strconv.Atoi(c.Query("limit"))
	evs, err := h.store.Events(c.Context(), q)
	if err != nil {
		if errors.Is(err, ErrEventsNeedFilter) {
			return fail(c, fiber.NewError(fiber.StatusBadRequest, "correlation_id, diag_ref or subject_type+subject_id is required"))
		}
		return fail(c, err) // cualquier otro fallo es del servidor: 500 generico, sin el texto interno
	}
	return ok(c, evs, fiber.Map{"count": len(evs)})
}

func (h *Handler) setStatus(status string) fiber.Handler {
	return func(c fiber.Ctx) error {
		org, err := h.org(c)
		if err != nil {
			return fail(c, err)
		}
		id, perr := uuid.Parse(c.Params("id"))
		if perr != nil {
			return fail(c, fiber.NewError(fiber.StatusBadRequest, "invalid id"))
		}
		if err := h.store.SetStatus(c.Context(), org, id, status); err != nil {
			return fail(c, err)
		}
		g, _, err := h.store.Get(c.Context(), org, id)
		if err != nil {
			return fail(c, err)
		}
		return ok(c, g, nil)
	}
}

// Bundle es el support bundle: todo re-saneado, con versiones y SIN ajustes.
type Bundle struct {
	GeneratedAt   time.Time         `json:"generated_at"`
	KernelVersion string            `json:"kernel_version,omitempty"`
	Org           uuid.UUID         `json:"org_id"`
	Addons        map[string]string `json:"addons,omitempty"`
	Issues        []Group           `json:"issues"`
	Events        []Event           `json:"events"`
}

func (h *Handler) export(c fiber.Ctx) error {
	org, err := h.org(c)
	if err != nil {
		return fail(c, err)
	}
	b, err := h.Bundle(c.Context(), org, h.opt.Now().Add(-EventRetention))
	if err != nil {
		return fail(c, err)
	}
	if h.opt.Versions != nil {
		b.Addons = h.opt.Versions(c)
	}
	c.Set("Content-Disposition", fmt.Sprintf(`attachment; filename="diagnostics-%s.json"`, h.opt.Now().UTC().Format("20060102T150405Z")))
	return c.JSON(b)
}
