// Package marketplace exposes a thin HTTP surface for the embedded Hub
// install flow: when a user clicks "Instalar" inside a host app's
// marketplace iframe, the host (via metacore-app-providers/MetacoreAppShell)
// posts the addon key + version + bundle URL to this handler. It records
// the request in `marketplace_installations` and returns 201.
//
// The full install pipeline (download bundle, verify Ed25519, run
// installer.Install) lives in the kernel/installer package and is wired
// separately by hosts that need it. This handler is the lightweight
// "register the intent" entry point that every metacore app gets for free.
package marketplace

import (
	"time"

	"github.com/asteby/metacore-kernel/auth"
	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// Installation is the persisted row. Hosts that wire the full
// installer.Install pipeline can extend this with foreign keys.
type Installation struct {
	ID             uuid.UUID `gorm:"type:uuid;primaryKey;default:gen_random_uuid()" json:"id"`
	OrganizationID uuid.UUID `gorm:"type:uuid;not null;index" json:"organization_id"`
	AddonKey       string    `gorm:"size:120;not null;index:idx_org_addon,priority:2" json:"addon_key"`
	Version        string    `gorm:"size:40;not null" json:"version"`
	BundleURL      string    `gorm:"size:512" json:"bundle_url,omitempty"`
	Status         string    `gorm:"size:20;not null;default:'requested'" json:"status"`
	RequestedByID  uuid.UUID `gorm:"type:uuid;not null;index" json:"requested_by_id"`
	RequestedAt    time.Time `gorm:"autoCreateTime" json:"requested_at"`
}

func (Installation) TableName() string { return "marketplace_installations" }

// Handler wires the HTTP routes on top of a *gorm.DB. Mount via Mount(r).
type Handler struct {
	db *gorm.DB
}

// NewHandler builds a Handler. AutoMigrates the Installation table on first
// call so hosts that wire the marketplace endpoint don't have to add a
// migration step.
func NewHandler(db *gorm.DB) (*Handler, error) {
	if db == nil {
		panic("marketplace: NewHandler requires a *gorm.DB")
	}
	if err := db.AutoMigrate(&Installation{}); err != nil {
		return nil, err
	}
	return &Handler{db: db}, nil
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
	return c.Status(fiber.StatusCreated).JSON(fiber.Map{
		"success": true,
		"data":    row,
	})
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
