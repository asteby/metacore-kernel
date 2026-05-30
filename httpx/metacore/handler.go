// Package metacore exposes a thin HTTP surface (Fiber routes) over the
// metacore kernel that runs alongside (not instead of) any pre-existing
// addon endpoints a host app already serves. The frontend opts into these
// routes to exercise schema-per-addon, HMAC signing and capabilities
// without disturbing existing /api/addons/* contracts.
//
// Routes:
//
//	GET  /api/metacore/manifests                    → ListManifests
//	GET  /api/metacore/navigation                   → Navigation
//	GET  /api/metacore/catalog                      → Catalog
//	POST /api/metacore/installations/:key           → Install
//	PUT  /api/metacore/installations/:key/version   → Upgrade
//	GET  /api/metacore/addons/:key/frontend/*path   → ServeAddonFrontend
//	GET  /api/metacore/tools[?addon_key=X]          → ListTools
//	POST /api/metacore/tools/execute                → ExecuteTool
//
// The Handler does not import host model packages — it consumes the
// bridge.Bridge and a small set of host-supplied collaborators
// (ToolStore, ActionsBridge) injected via Deps.
package metacore

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/asteby/metacore-kernel/bridge"
	"github.com/asteby/metacore-kernel/bundle"
	"github.com/asteby/metacore-kernel/httpx"
	"github.com/asteby/metacore-kernel/installer"
	"github.com/asteby/metacore-kernel/manifest"
	"github.com/asteby/metacore-kernel/navigation"
	kerneltool "github.com/asteby/metacore-kernel/tool"
	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
)

// Deps bundles the collaborators a host wires into the HTTP handler.
// Fields tagged optional may be left nil — the routes that need them will
// degrade or refuse with a clear message instead of panicking.
type Deps struct {
	// Bridge is the kernel-side glue. Required.
	Bridge *bridge.Bridge

	// ActionsBridge projects manifest.Actions into the host's interceptor
	// registry on install. Optional: if nil, action wiring is skipped on
	// Install (the installation still succeeds).
	ActionsBridge *bridge.ActionsBridge

	// ToolStore is the host's agent-tool table. Optional: if nil, the
	// host-specific tool-row projection is skipped on Install.
	ToolStore bridge.ToolStore

	// ToolRegistry is the process-global kernel tool registry. Defaults to
	// kerneltool.GlobalRegistry() when nil.
	ToolRegistry *kerneltool.Registry

	// CoreNavigation is the host's baked-in sidebar groups merged with
	// addon contributions. Required for Navigation; if nil the route
	// returns just the addon-contributed groups.
	CoreNavigation []navigation.Group

	// FrontendBasePath is the on-disk root from which ServeAddonFrontend
	// reads materialized federation artifacts. Empty disables that route.
	FrontendBasePath string

	// CatalogDir is the directory ListCatalog scans for .tar.gz bundles.
	// Defaults to METACORE_CATALOG_DIR env var when empty.
	CatalogDir string
}

// Handler holds the kernel collaborators needed by the kernel-scoped
// routes. Build it once at startup and wire its methods to your Fiber
// router.
type Handler struct {
	deps Deps
}

// NewHandler validates the supplied deps and returns a ready-to-mount
// Handler. Returns an error if Bridge is nil — every route depends on it.
func NewHandler(deps Deps) (*Handler, error) {
	if deps.Bridge == nil {
		return nil, fmt.Errorf("metacore handler: Deps.Bridge is required")
	}
	if deps.ToolRegistry == nil {
		deps.ToolRegistry = kerneltool.GlobalRegistry()
	}
	return &Handler{deps: deps}, nil
}

// orgIDFromCtx extracts the authenticated organization id from the Fiber
// context. We use kernel/httpx's well-known key so any host that drops
// "organization_id" (uuid.UUID) into Fiber locals via auth middleware
// works out of the box.
func orgIDFromCtx(c fiber.Ctx) (uuid.UUID, bool) {
	id, err := httpx.ExtractOrgID(fiberLookup{c})
	if err != nil {
		return uuid.UUID{}, false
	}
	return id, id != uuid.Nil
}

// fiberLookup adapts fiber.Ctx to httpx.ContextLookup.
type fiberLookup struct{ c fiber.Ctx }

func (f fiberLookup) Locals(key string) any { return f.c.Locals(key) }

// ListManifests returns the manifests of every addon currently enabled
// for the caller's organization.
//
// GET /api/metacore/manifests
//
// Returns the empty array when the request has no organization context
// (anonymous / unauthenticated). The SDK frontend boots before auth has
// hydrated and calls this endpoint; returning [] keeps the bootstrap clean
// instead of forcing every host to wrap the route in an auth middleware
// just for the empty case.
func (h *Handler) ListManifests(c fiber.Ctx) error {
	c.Set("X-Metacore-Kernel-Version", h.deps.Bridge.KernelVersion())
	orgID, ok := orgIDFromCtx(c)
	if !ok {
		return c.JSON([]manifest.Manifest{})
	}
	manifests, err := h.deps.Bridge.Host().InstalledManifests(orgID)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"success": false,
			"message": err.Error(),
		})
	}
	if manifests == nil {
		manifests = []manifest.Manifest{}
	}
	return c.JSON(manifests)
}

// Navigation returns the merged sidebar for the caller's organization.
// Core groups come from Deps.CoreNavigation; addon groups come from the
// kernel's registry.
//
// GET /api/metacore/navigation
//
// Returns the empty array when the request has no organization context
// (anonymous / unauthenticated). Mirrors ListManifests — both endpoints
// are safe to expose without auth and the SDK bootstrap fires before
// auth state has hydrated.
func (h *Handler) Navigation(c fiber.Ctx) error {
	orgID, ok := orgIDFromCtx(c)
	if !ok {
		return c.JSON([]navigation.Group{})
	}
	groups, err := h.deps.Bridge.Navigation(orgID, h.deps.CoreNavigation)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"success": false,
			"message": err.Error(),
		})
	}
	if groups == nil {
		groups = []navigation.Group{}
	}
	return c.JSON(groups)
}

// Install installs (or re-installs) an addon for the caller's organization.
//
// Two invocation styles are accepted:
//
//  1. Bundle upload — multipart/form-data with a "bundle" file containing
//     a .tar.gz produced by the SDK. The kernel parses, validates and runs
//     the full install pipeline. The :key URL param must match the
//     bundle's manifest key (safety check against wrong-URL uploads).
//
//  2. Registered manifest — no body. The kernel invokes install on an
//     addon already present in its lifecycle registry (e.g. imported from
//     the host's legacy system by the bridge). Useful for promoting an
//     already-registered declarative addon into an enabled installation
//     for a new org.
//
// POST /api/metacore/installations/:key
func (h *Handler) Install(c fiber.Ctx) error {
	orgID, ok := orgIDFromCtx(c)
	if !ok {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{
			"success": false,
			"message": "organization context required",
		})
	}
	key := c.Params("key")
	if key == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"success": false,
			"message": "addon key required",
		})
	}

	if file, err := c.FormFile("bundle"); err == nil && file != nil {
		src, err := file.Open()
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
				"success": false,
				"message": "failed to read uploaded bundle",
			})
		}
		defer src.Close()

		b, err := bundle.Read(src, 64<<20)
		if err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
				"success": false,
				"message": fmt.Sprintf("invalid bundle: %v", err),
			})
		}
		if b.Manifest.Key != key {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
				"success": false,
				"message": fmt.Sprintf("bundle key %q does not match URL key %q", b.Manifest.Key, key),
			})
		}
		inst, secret, err := h.deps.Bridge.Host().Installer.Install(orgID, b)
		if err != nil {
			return c.Status(fiber.StatusUnprocessableEntity).JSON(fiber.Map{
				"success": false,
				"message": err.Error(),
			})
		}
		// Host-specific tool projection (each host maps tools into its own
		// agent-tool table). Non-fatal — the installation already succeeded.
		if h.deps.ToolStore != nil {
			_ = bridge.SyncAddonTools(h.deps.ToolStore, orgID, b.Manifest)
		}
		// Process-global kernel tool registry hydration so /tools/execute
		// can resolve without re-walking the manifest on every call.
		if inst != nil {
			_, _ = kerneltool.SyncFromManifest(b.Manifest, *inst, h.deps.Bridge.WebhookDispatcher(), h.deps.ToolRegistry)
		}
		// Project UI-facing Actions into the host's interceptor registry.
		if h.deps.ActionsBridge != nil {
			if err := h.deps.ActionsBridge.SyncAddonActions(b.Manifest); err != nil {
				return c.JSON(fiber.Map{
					"success":         true,
					"source":          "bundle",
					"installation":    inst,
					"install_secret":  string(secret),
					"manifest":        b.Manifest,
					"actions_warning": "install succeeded but action bridge failed: " + err.Error(),
				})
			}
		}
		return c.JSON(fiber.Map{
			"success":        true,
			"source":         "bundle",
			"installation":   inst,
			"install_secret": string(secret),
			"manifest":       b.Manifest,
		})
	}

	// Fallback: install from an already-registered manifest (no bundle body).
	lc, exists := h.deps.Bridge.Host().Lifecycles.Get(key)
	if !exists {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{
			"success": false,
			"message": fmt.Sprintf("addon %q is not registered in the kernel; upload a bundle or register it first", key),
		})
	}
	b := &bundle.Bundle{Manifest: lc.Manifest()}
	inst, secret, err := h.deps.Bridge.Host().Installer.Install(orgID, b)
	if err != nil {
		return c.Status(fiber.StatusUnprocessableEntity).JSON(fiber.Map{
			"success": false,
			"message": err.Error(),
		})
	}
	if inst != nil {
		_, _ = kerneltool.SyncFromManifest(lc.Manifest(), *inst, h.deps.Bridge.WebhookDispatcher(), h.deps.ToolRegistry)
	}
	return c.JSON(fiber.Map{
		"success":        true,
		"source":         "registered",
		"installation":   inst,
		"install_secret": string(secret),
		"manifest":       lc.Manifest(),
	})
}

// Upgrade applies a newer bundle on top of an already-installed addon for
// the caller's organization. Required because Install treats every call as
// a fresh first-install and never fires the `upgrade` lifecycle event;
// hosts that need to bump a tenant from 1.0 → 1.1 use this route so addon
// authors can react to the version change via their declared
// `lifecycle_hooks.upgrade` handlers.
//
// Request body: multipart/form-data with a `bundle` file containing the new
// .tar.gz. The :key URL param MUST match the bundle's manifest key — safety
// check against wrong-URL uploads.
//
// Possible status codes:
//
//	200   upgrade applied; body contains the updated installation row
//	400   bundle missing, malformed, or key mismatch
//	404   addon is not installed for the caller's org (installer.ErrNotInstalled)
//	409   cannot downgrade / same-version upgrade
//	422   signature rejected or schema apply failed
//
// PUT /api/metacore/installations/:key/version
func (h *Handler) Upgrade(c fiber.Ctx) error {
	orgID, ok := orgIDFromCtx(c)
	if !ok {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{
			"success": false,
			"message": "organization context required",
		})
	}
	key := c.Params("key")
	if key == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"success": false,
			"message": "addon key required",
		})
	}

	file, err := c.FormFile("bundle")
	if err != nil || file == nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"success": false,
			"message": "multipart form-data with 'bundle' file is required",
		})
	}
	src, err := file.Open()
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"success": false,
			"message": "failed to read uploaded bundle",
		})
	}
	defer src.Close()

	b, err := bundle.Read(src, 64<<20)
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"success": false,
			"message": fmt.Sprintf("invalid bundle: %v", err),
		})
	}
	if b.Manifest.Key != key {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"success": false,
			"message": fmt.Sprintf("bundle key %q does not match URL key %q", b.Manifest.Key, key),
		})
	}

	inst, err := h.deps.Bridge.Host().Installer.Upgrade(c.Context(), orgID, b)
	if err != nil {
		// Map sentinel errors to specific HTTP codes so frontends can render
		// a precise message ("not installed → install first", "same version
		// → already up to date", etc.).
		switch {
		case errors.Is(err, installer.ErrNotInstalled):
			return c.Status(fiber.StatusNotFound).JSON(fiber.Map{
				"success": false,
				"code":    "not_installed",
				"message": err.Error(),
			})
		case errors.Is(err, installer.ErrCannotDowngrade):
			return c.Status(fiber.StatusConflict).JSON(fiber.Map{
				"success": false,
				"code":    "cannot_downgrade",
				"message": err.Error(),
			})
		case errors.Is(err, installer.ErrSameVersionUpgrade):
			return c.Status(fiber.StatusConflict).JSON(fiber.Map{
				"success": false,
				"code":    "same_version_upgrade",
				"message": err.Error(),
			})
		}
		return c.Status(fiber.StatusUnprocessableEntity).JSON(fiber.Map{
			"success": false,
			"message": err.Error(),
		})
	}

	// Re-project host-specific side effects exactly like Install does so
	// the host sees a consistent post-state regardless of whether the
	// version arrived via Install or Upgrade.
	if h.deps.ToolStore != nil {
		_ = bridge.SyncAddonTools(h.deps.ToolStore, orgID, b.Manifest)
	}
	if inst != nil {
		_, _ = kerneltool.SyncFromManifest(b.Manifest, *inst, h.deps.Bridge.WebhookDispatcher(), h.deps.ToolRegistry)
	}
	if h.deps.ActionsBridge != nil {
		_ = h.deps.ActionsBridge.SyncAddonActions(b.Manifest)
	}

	return c.JSON(fiber.Map{
		"success":      true,
		"installation": inst,
		"manifest":     b.Manifest,
	})
}

// mimeForExt returns a correct Content-Type for the small set of
// extensions federation bundles emit. Everything else falls back to
// octet-stream so the browser never tries to sniff-execute arbitrary
// static content.
func mimeForExt(ext string) string {
	switch strings.ToLower(ext) {
	case ".js", ".mjs":
		return "application/javascript"
	case ".css":
		return "text/css"
	case ".map":
		return "application/json"
	case ".html":
		return "text/html; charset=utf-8"
	case ".json":
		return "application/json"
	case ".svg":
		return "image/svg+xml"
	case ".woff":
		return "font/woff"
	case ".woff2":
		return "font/woff2"
	default:
		return "application/octet-stream"
	}
}

// ServeAddonFrontend serves a static file from the addon's materialized
// frontend directory. It is scoped to installations enabled for the
// caller's organization — an un-installed addon's remoteEntry.js is not
// discoverable via this route. Path traversal is rejected by verifying
// the resolved path stays under FrontendBasePath/<key>.
//
// GET /api/metacore/addons/:key/frontend/*path
func (h *Handler) ServeAddonFrontend(c fiber.Ctx) error {
	if h.deps.FrontendBasePath == "" {
		return c.Status(fiber.StatusNotFound).SendString("frontend serving disabled")
	}
	addonKey := c.Params("key")
	subpath := strings.TrimPrefix(c.Params("*"), "/")
	if addonKey == "" || subpath == "" {
		return c.Status(fiber.StatusBadRequest).SendString("key and path required")
	}

	// Federation bundles are PUBLIC static frontend assets. The browser's
	// module loader fetches remoteEntry.js — and every lazy chunk it imports —
	// via plain <script>/import() with no way to attach the host's bearer
	// token, so gating this route on auth makes federated addon UIs impossible
	// to load (the request 401s and the browser refuses the JSON error as a
	// module). The bundle is non-sensitive client code; the data layer stays
	// behind the authenticated API. So: when org context IS present we still
	// scope the serve to addons enabled for that org (defence in depth for
	// authenticated callers); when it is absent — the normal script-load case —
	// we serve the static asset anyway. Hosts mount this route without their
	// auth middleware to make it publicly reachable.
	if orgID, ok := orgIDFromCtx(c); ok {
		var count int64
		if err := h.deps.Bridge.Host().Installer.DB.
			Model(&installer.Installation{}).
			Where("organization_id = ? AND addon_key = ? AND status = ?", orgID, addonKey, "enabled").
			Count(&count).Error; err != nil {
			return c.Status(fiber.StatusInternalServerError).SendString(err.Error())
		}
		if count == 0 {
			return c.Status(fiber.StatusNotFound).SendString("addon not installed for organization")
		}
	}

	dir := installer.FrontendDir(h.deps.FrontendBasePath, addonKey)
	clean := filepath.Clean(subpath)
	filePath := filepath.Join(dir, clean)
	absDir, _ := filepath.Abs(dir)
	absFile, _ := filepath.Abs(filePath)
	if !strings.HasPrefix(absFile, absDir+string(filepath.Separator)) && absFile != absDir {
		return c.Status(fiber.StatusBadRequest).SendString("invalid path")
	}

	if info, err := os.Stat(filePath); err != nil || info.IsDir() {
		return c.Status(fiber.StatusNotFound).SendString("file not found")
	}

	c.Set(fiber.HeaderContentType, mimeForExt(filepath.Ext(filePath)))
	c.Set(fiber.HeaderCacheControl, "public, max-age=31536000, immutable")
	return c.SendFile(filePath)
}

// guard: ensure errors stays referenced if future refactors switch
// fiber.Map to a typed envelope. (No call.)
var _ = errors.New
