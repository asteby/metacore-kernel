package flow

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
)

// OrgResolver extracts the organization ID from a request. Hosts wire this
// to whatever auth layer they use (JWT claims, header, session, etc.). When
// the resolver returns an error the handler responds 400.
type OrgResolver func(c fiber.Ctx) (uuid.UUID, error)

// FlowSource is the read-side dependency the HTTP layer needs: load a flow by
// ID for the editor "Test" and execute endpoints. It is intentionally narrow
// (one method) so app stores satisfy it with no extra work.
type FlowSource interface {
	GetFlow(ctx context.Context, orgID, flowID uuid.UUID) (*Flow, error)
}

// FlowSourceFunc is a tiny adapter so app code can pass a function literal
// where a FlowSource is expected.
type FlowSourceFunc func(ctx context.Context, orgID, flowID uuid.UUID) (*Flow, error)

// GetFlow satisfies FlowSource.
func (f FlowSourceFunc) GetFlow(ctx context.Context, orgID, flowID uuid.UUID) (*Flow, error) {
	return f(ctx, orgID, flowID)
}

// HandlerConfig wires the runtime endpoints together. Engine and FlowSource
// are required; OrgResolver is optional — when nil the handler reads the
// organization ID off of the loaded flow and the caller is trusted.
type HandlerConfig struct {
	Engine      *Engine
	FlowSource  FlowSource
	OrgResolver OrgResolver
}

// Handler exposes the flow runtime over Fiber. It is intentionally minimal —
// flow CRUD (Create / Update / Delete) is left to the host because flows live
// in the host's database with host-specific columns (status, version log,
// statistics). The kernel only exposes the runtime endpoints:
//
//	POST /:id/run    asynchronous execution → returns execution ID
//	POST /:id/test   synchronous "Test" run → returns inline TestResult
//	POST /:id/cancel cancel a running execution (id = execution ID here)
//
// Apps mount this alongside their own CRUD routes:
//
//	flowRuntime := flow.NewHandler(flow.HandlerConfig{
//	    Engine:      engine,
//	    FlowSource:  myStore,
//	    OrgResolver: myAuth.OrgID,
//	})
//	flowRuntime.Mount(api.Group("/flows"))
type Handler struct {
	cfg HandlerConfig
}

// NewHandler builds a handler. Returns nil when Engine or FlowSource are nil
// — both are mandatory and there is no useful default the kernel can supply.
func NewHandler(cfg HandlerConfig) *Handler {
	if cfg.Engine == nil || cfg.FlowSource == nil {
		return nil
	}
	return &Handler{cfg: cfg}
}

// Mount installs the runtime endpoints on the given router. Apps typically
// call this from their flow CRUD module after registering create/update/delete.
func (h *Handler) Mount(r fiber.Router) {
	r.Post("/:id/run", h.run)
	r.Post("/:id/test", h.test)
	r.Post("/:id/cancel", h.cancel)
}

// run kicks off an asynchronous flow execution and returns the execution ID.
// Body schema:
//
//	{ "trigger_data": { ... }, "app_context": { ... } }
func (h *Handler) run(c fiber.Ctx) error {
	flowID, err := parseUUIDParam(c, "id")
	if err != nil {
		return fail(c, fiber.StatusBadRequest, err.Error())
	}
	body, err := parseRunBody(c)
	if err != nil {
		return fail(c, fiber.StatusBadRequest, err.Error())
	}

	orgID, err := h.resolveOrg(c)
	if err != nil {
		return fail(c, fiber.StatusBadRequest, err.Error())
	}

	f, err := h.cfg.FlowSource.GetFlow(c, orgID, flowID)
	if err != nil {
		return fail(c, fiber.StatusNotFound, err.Error())
	}
	if f == nil {
		return fail(c, fiber.StatusNotFound, "flow not found")
	}

	triggerType := f.TriggerType
	if triggerType == "" {
		triggerType = TriggerManual
	}

	exec, err := h.cfg.Engine.ExecuteFlow(f, triggerType, body.TriggerData, body.AppContext)
	if err != nil {
		return fail(c, fiber.StatusBadRequest, err.Error())
	}
	return ok(c, fiber.Map{
		"execution_id": exec.ID.String(),
		"status":       string(exec.Status),
		"started_at":   exec.StartedAt,
	})
}

// test runs a flow synchronously without persisting executions — the editor
// "Test" button. Body schema mirrors run.
func (h *Handler) test(c fiber.Ctx) error {
	flowID, err := parseUUIDParam(c, "id")
	if err != nil {
		return fail(c, fiber.StatusBadRequest, err.Error())
	}
	body, err := parseRunBody(c)
	if err != nil {
		return fail(c, fiber.StatusBadRequest, err.Error())
	}

	orgID, err := h.resolveOrg(c)
	if err != nil {
		return fail(c, fiber.StatusBadRequest, err.Error())
	}

	f, err := h.cfg.FlowSource.GetFlow(c, orgID, flowID)
	if err != nil {
		return fail(c, fiber.StatusNotFound, err.Error())
	}
	if f == nil {
		return fail(c, fiber.StatusNotFound, "flow not found")
	}

	result, err := h.cfg.Engine.TestFlowInline(f, body.TriggerData, body.AppContext)
	if err != nil {
		return fail(c, fiber.StatusInternalServerError, err.Error())
	}
	return ok(c, result)
}

// cancel aborts an in-flight execution. Note `:id` is the execution ID here,
// not the flow ID — the engine tracks active executions by execution ID.
func (h *Handler) cancel(c fiber.Ctx) error {
	execID, err := parseUUIDParam(c, "id")
	if err != nil {
		return fail(c, fiber.StatusBadRequest, err.Error())
	}
	if err := h.cfg.Engine.CancelExecution(execID); err != nil {
		return fail(c, fiber.StatusConflict, err.Error())
	}
	return ok(c, fiber.Map{"cancelled": true})
}

// resolveOrg returns the org ID for the request, falling back to a zero UUID
// when no resolver was configured.
func (h *Handler) resolveOrg(c fiber.Ctx) (uuid.UUID, error) {
	if h.cfg.OrgResolver == nil {
		return uuid.Nil, nil
	}
	return h.cfg.OrgResolver(c)
}

// runBody is the JSON shape POST /:id/run and POST /:id/test accept. Both
// fields are optional; absent fields default to empty maps.
type runBody struct {
	TriggerData map[string]interface{} `json:"trigger_data"`
	AppContext  map[string]interface{} `json:"app_context"`
}

func parseRunBody(c fiber.Ctx) (*runBody, error) {
	raw := c.Body()
	body := &runBody{}
	if len(raw) == 0 {
		return body, nil
	}
	if err := json.Unmarshal(raw, body); err != nil {
		return nil, fmt.Errorf("invalid JSON body: %w", err)
	}
	return body, nil
}

func parseUUIDParam(c fiber.Ctx, name string) (uuid.UUID, error) {
	raw := c.Params(name)
	id, err := uuid.Parse(raw)
	if err != nil {
		return uuid.Nil, fmt.Errorf("invalid %s: %w", name, err)
	}
	return id, nil
}

func ok(c fiber.Ctx, data any) error {
	return c.JSON(fiber.Map{"success": true, "data": data})
}

func fail(c fiber.Ctx, status int, msg string) error {
	return c.Status(status).JSON(fiber.Map{"success": false, "message": msg})
}
