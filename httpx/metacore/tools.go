// tools.go exposes the kernel's process-global tool.Registry over HTTP so
// the host frontend (and any other in-process caller) can list registered
// tools and dispatch one by (addon_key, tool_id) without duplicating the
// HMAC/validation pipeline.
//
// Routes:
//
//	GET  /api/metacore/tools                    → list all registered tools
//	GET  /api/metacore/tools?addon_key=foo      → filter by addon
//	POST /api/metacore/tools/execute            → run one tool by id
//
// Registration lives in Handler.Install — see handler.go — so this file
// only reads from the registry; it never mutates it.
package metacore

import (
	"strings"

	"github.com/asteby/metacore-kernel/installer"
	"github.com/asteby/metacore-kernel/manifest"
	kerneltool "github.com/asteby/metacore-kernel/tool"
	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
)

// toolListItem is the wire shape returned by ListTools. We serialize the
// manifest.ToolDef inline so @asteby/metacore-tools' HTTPToolClient.list()
// (which expects `ToolDef[]`) can consume the body directly — the extra
// addon_key field is optional in the SDK type and is ignored cleanly by
// callers that only care about the def. The full envelope shape is kept
// flat (no `{success,items,total}` wrapper) for that same reason.
type toolListItem struct {
	manifest.ToolDef
	// AddonKey scopes the tool. Duplicated from the registry key so the
	// SDK client can round-trip it into ToolExecutionRequest without a
	// second call.
	AddonKey string `json:"addon_key"`
}

// ListTools returns every tool currently registered in the kernel's
// process-global registry. When ?addon_key is supplied, only tools
// belonging to that addon are returned. The response is a bare JSON array
// of ToolDef-shaped objects so SDK clients consume it as-is.
//
// GET /api/metacore/tools[?addon_key=X]
func (h *Handler) ListTools(c *fiber.Ctx) error {
	registry := h.deps.ToolRegistry

	filter := strings.TrimSpace(c.Query("addon_key"))
	var tools []kerneltool.Tool
	if filter != "" {
		tools = registry.ByAddon(filter)
	} else {
		tools = registry.All()
	}

	items := make([]toolListItem, 0, len(tools))
	for _, t := range tools {
		items = append(items, toolListItem{
			ToolDef:  t.Def(),
			AddonKey: t.AddonKey(),
		})
	}
	return c.JSON(items)
}

// executeToolRequest is the POST body accepted by ExecuteTool.
type executeToolRequest struct {
	AddonKey       string         `json:"addon_key"`
	ToolID         string         `json:"tool_id"`
	InstallationID string         `json:"installation_id"`
	Parameters     map[string]any `json:"parameters"`
}

// ExecuteTool dispatches a single registered tool. The InstallationID in
// the body is currently informational — the dispatcher already carries
// the installation context captured at Install time. We validate it
// belongs to the caller's org when provided so frontends can't accidentally
// dispatch a tool against a different tenant's installation.
//
// POST /api/metacore/tools/execute
func (h *Handler) ExecuteTool(c *fiber.Ctx) error {
	orgID, ok := orgIDFromCtx(c)
	if !ok {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{
			"success": false,
			"message": "organization context required",
		})
	}

	var req executeToolRequest
	if err := c.BodyParser(&req); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"success": false,
			"message": "invalid JSON body: " + err.Error(),
		})
	}
	if req.AddonKey == "" || req.ToolID == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"success": false,
			"message": "addon_key and tool_id are required",
		})
	}

	// Cross-tenant guard: when installation_id is supplied, make sure it
	// actually belongs to the caller's org for this addon.
	if req.InstallationID != "" {
		instUUID, err := uuid.Parse(req.InstallationID)
		if err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
				"success": false,
				"message": "invalid installation_id",
			})
		}
		var inst installer.Installation
		err = h.deps.Bridge.DB().Where(
			"id = ? AND organization_id = ? AND addon_key = ?",
			instUUID, orgID, req.AddonKey,
		).First(&inst).Error
		if err != nil {
			return c.Status(fiber.StatusForbidden).JSON(fiber.Map{
				"success": false,
				"message": "installation not found for this organization/addon",
			})
		}
	}

	tool, ok := h.deps.ToolRegistry.ByID(req.AddonKey, req.ToolID)
	if !ok {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{
			"success": false,
			"message": "tool not registered",
		})
	}

	result, err := tool.Execute(c.Context(), req.Parameters)
	if err != nil {
		// Shape-compatible with ToolExecutionResponse — the SDK client
		// returns the JSON body verbatim as the response envelope.
		return c.Status(fiber.StatusBadGateway).JSON(fiber.Map{
			"success": false,
			"error":   err.Error(),
		})
	}
	// kernel/tool.Result already matches ToolExecutionResponse:
	// {success, data, error, metadata}. Return it verbatim.
	return c.JSON(result)
}
