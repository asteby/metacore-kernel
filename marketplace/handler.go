// Package marketplace exposes a thin HTTP surface for the embedded Hub
// install flow: when a user clicks "Instalar" inside a host app's
// marketplace iframe, the host (via metacore-app-providers/MetacoreAppShell)
// posts the addon key + version + bundle URL to this handler.
//
// Two modes:
//
//   - Lite (default, when no Installer is wired): records the request
//     in `marketplace_installations` with status `requested` and returns
//     201. Apps without the full bundle pipeline get a working "Instalar"
//     button that at least persists the intent for later automation.
//
//   - Full (Installer wired via WithInstaller): downloads the bundle from
//     the supplied URL, validates it through `bundle.Read`, runs the
//     kernel's `installer.Install(orgID, bundle)` pipeline (migrations,
//     lifecycle hooks, secret minting, frontend write, etc) and flips
//     the row status to `installed` on success or `failed` on error.
package marketplace

import (
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/asteby/metacore-kernel/auth"
	"github.com/asteby/metacore-kernel/bundle"
	"github.com/asteby/metacore-kernel/installer"
	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// maxBundleBytes caps remote bundle downloads. Mirrors the limit used by
// httpx/metacore.Install for multipart uploads.
const maxBundleBytes int64 = 64 << 20

// Installation is the persisted row. Hosts that wire the full
// installer.Install pipeline can extend this with foreign keys.
type Installation struct {
	ID             uuid.UUID `gorm:"type:uuid;primaryKey;default:gen_random_uuid()" json:"id"`
	OrganizationID uuid.UUID `gorm:"type:uuid;not null;index" json:"organization_id"`
	AddonKey       string    `gorm:"size:120;not null;index:idx_org_addon,priority:2" json:"addon_key"`
	Version        string    `gorm:"size:40;not null" json:"version"`
	BundleURL      string    `gorm:"size:512" json:"bundle_url,omitempty"`
	// Status: requested → downloading → installing → installed | failed
	Status         string    `gorm:"size:20;not null;default:'requested'" json:"status"`
	ErrorMessage   string    `gorm:"size:1024" json:"error_message,omitempty"`
	RequestedByID  uuid.UUID `gorm:"type:uuid;not null;index" json:"requested_by_id"`
	RequestedAt    time.Time `gorm:"autoCreateTime" json:"requested_at"`
	CompletedAt    *time.Time `json:"completed_at,omitempty"`
}

func (Installation) TableName() string { return "marketplace_installations" }

// Handler wires the HTTP routes on top of a *gorm.DB.
type Handler struct {
	db        *gorm.DB
	installer *installer.Installer
	httpc     *http.Client
}

// HandlerOption customises the handler.
type HandlerOption func(*Handler)

// WithInstaller wires the kernel installer so POST /install runs the full
// download + verify + install pipeline. Without this, the handler operates
// in lite mode (records the request, lets a worker pick it up).
func WithInstaller(inst *installer.Installer) HandlerOption {
	return func(h *Handler) { h.installer = inst }
}

// WithHTTPClient overrides the bundle-download HTTP client (default: 30s
// timeout). Useful for tests or for hosts with custom retry/cert config.
func WithHTTPClient(c *http.Client) HandlerOption {
	return func(h *Handler) { h.httpc = c }
}

// NewHandler builds a Handler. AutoMigrates the Installation table on first
// call so hosts that wire the marketplace endpoint don't have to add a
// migration step.
func NewHandler(db *gorm.DB, opts ...HandlerOption) (*Handler, error) {
	if db == nil {
		panic("marketplace: NewHandler requires a *gorm.DB")
	}
	if err := db.AutoMigrate(&Installation{}); err != nil {
		return nil, err
	}
	h := &Handler{
		db:    db,
		httpc: &http.Client{Timeout: 30 * time.Second},
	}
	for _, opt := range opts {
		opt(h)
	}
	return h, nil
}

// Mount registers the marketplace endpoints under `/marketplace`:
//
//	POST /marketplace/install   {addonKey, version, bundleURL}
//	GET  /marketplace/installs                  → org's installations list
//
// `middleware` is layered first (typically the host's auth middleware).
func (h *Handler) Mount(r fiber.Router, middleware ...fiber.Handler) {
	g := r.Group("/marketplace", middleware...)
	g.Post("/install", h.install)
	g.Get("/installs", h.list)
}

type installRequest struct {
	AddonKey  string `json:"addonKey"`
	Version   string `json:"version"`
	BundleURL string `json:"bundleURL"`
}

func (h *Handler) install(c *fiber.Ctx) error {
	orgID := auth.GetOrganizationID(c)
	userID := auth.GetUserID(c)
	if orgID == uuid.Nil || userID == uuid.Nil {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{
			"success": false,
			"message": "authenticated organization required",
		})
	}

	var req installRequest
	if err := c.BodyParser(&req); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"success": false,
			"message": "invalid body: " + err.Error(),
		})
	}
	if req.AddonKey == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"success": false,
			"message": "addonKey is required",
		})
	}
	if req.Version == "" {
		req.Version = "latest"
	}

	row := Installation{
		OrganizationID: orgID,
		AddonKey:       req.AddonKey,
		Version:        req.Version,
		BundleURL:      req.BundleURL,
		Status:         "requested",
		RequestedByID:  userID,
	}
	if err := h.db.WithContext(c.Context()).Create(&row).Error; err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"success": false,
			"message": "failed to record installation: " + err.Error(),
		})
	}

	// Lite mode — no installer wired or no bundle URL to fetch. The row
	// stays in `requested` for a worker / cron to pick up.
	if h.installer == nil || req.BundleURL == "" {
		return c.Status(fiber.StatusCreated).JSON(fiber.Map{
			"success": true,
			"data":    row,
		})
	}

	// Full pipeline — download, parse, install in a single request.
	// Hosts that need long-running installs should swap this for an
	// async worker (record the row, return 202, run the pipeline off the
	// request goroutine). 30s is enough for kernel-shipped addons.
	h.markStatus(&row, "downloading", "")

	b, err := h.fetchBundle(req.BundleURL, req.AddonKey)
	if err != nil {
		h.markFailed(&row, err)
		return c.Status(fiber.StatusUnprocessableEntity).JSON(fiber.Map{
			"success": false,
			"message": err.Error(),
			"data":    row,
		})
	}

	h.markStatus(&row, "installing", "")
	inst, _, err := h.installer.Install(orgID, b)
	if err != nil {
		h.markFailed(&row, err)
		return c.Status(fiber.StatusUnprocessableEntity).JSON(fiber.Map{
			"success": false,
			"message": err.Error(),
			"data":    row,
		})
	}

	now := time.Now()
	row.Status = "installed"
	row.CompletedAt = &now
	row.ErrorMessage = ""
	_ = h.db.Save(&row).Error

	return c.Status(fiber.StatusCreated).JSON(fiber.Map{
		"success":      true,
		"data":         row,
		"installation": inst,
	})
}

// fetchBundle downloads the .tar.gz from the Hub and parses it through
// kernel/bundle.Read. The addon key is checked against the bundle manifest
// to catch URL/key mismatches early.
func (h *Handler) fetchBundle(url, expectedKey string) (*bundle.Bundle, error) {
	resp, err := h.httpc.Get(url)
	if err != nil {
		return nil, fmt.Errorf("download bundle: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("bundle URL returned %d: %s", resp.StatusCode, string(body))
	}
	b, err := bundle.Read(resp.Body, maxBundleBytes)
	if err != nil {
		return nil, fmt.Errorf("parse bundle: %w", err)
	}
	if b.Manifest.Key != expectedKey {
		return nil, fmt.Errorf("bundle key %q does not match request %q", b.Manifest.Key, expectedKey)
	}
	return b, nil
}

func (h *Handler) markStatus(row *Installation, status, errMsg string) {
	row.Status = status
	row.ErrorMessage = errMsg
	_ = h.db.Save(row).Error
}

func (h *Handler) markFailed(row *Installation, err error) {
	now := time.Now()
	row.Status = "failed"
	row.ErrorMessage = truncateError(err.Error(), 1024)
	row.CompletedAt = &now
	_ = h.db.Save(row).Error
}

func truncateError(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max-1] + "…"
}

func (h *Handler) list(c *fiber.Ctx) error {
	orgID := auth.GetOrganizationID(c)
	if orgID == uuid.Nil {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{
			"success": false,
			"message": "authenticated organization required",
		})
	}
	var rows []Installation
	if err := h.db.WithContext(c.Context()).
		Where("organization_id = ?", orgID).
		Order("requested_at DESC").
		Limit(200).
		Find(&rows).Error; err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"success": false,
			"message": err.Error(),
		})
	}
	return c.JSON(fiber.Map{
		"success": true,
		"data":    rows,
	})
}
