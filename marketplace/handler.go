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
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"

	"github.com/asteby/metacore-kernel/auth"
	"github.com/asteby/metacore-kernel/bundle"
	"github.com/asteby/metacore-kernel/installer"
	"github.com/gofiber/fiber/v3"
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
	// Name is the display title the Hub already localised to the user's
	// language at install time. Sidebar / dashboard use it instead of the
	// raw addon_key so users see "Activos Fijos" not "assets". Empty when
	// the Hub didn't supply one (legacy installs).
	Name           string    `gorm:"size:200" json:"name,omitempty"`
	// Category is the Hub-supplied taxonomy bucket ("operations",
	// "productivity", …) — useful for grouping installed addons in the
	// sidebar.
	Category       string    `gorm:"size:60" json:"category,omitempty"`
	Version        string    `gorm:"size:40;not null" json:"version"`
	BundleURL      string    `gorm:"size:512" json:"bundle_url,omitempty"`
	// Status: requested → downloading → installing → installed | failed
	//      |  installed   → upgrading → installed | failed
	//      |  installed   → uninstalling → uninstalled
	Status         string    `gorm:"size:20;not null;default:'requested'" json:"status"`
	ErrorMessage   string    `gorm:"size:1024" json:"error_message,omitempty"`
	RequestedByID  uuid.UUID `gorm:"type:uuid;not null;index" json:"requested_by_id"`
	RequestedAt    time.Time `gorm:"autoCreateTime" json:"requested_at"`
	CompletedAt    *time.Time `json:"completed_at,omitempty"`

	// PreviousVersion is the version this installation was on before the
	// most recent successful upgrade. Empty for fresh installs that never
	// upgraded. Rollback uses it (and PreviousVersions) to choose a target.
	PreviousVersion string `gorm:"size:40;column:previous_version" json:"previous_version,omitempty"`

	// PreviousVersions tracks the rolling history of versions this
	// installation has been on, oldest first. The kernel caps it at
	// maxRollbackHistory entries — rollback can only target the last 3
	// versions. Stored as a JSON-encoded []string so sqlite and postgres
	// agree on round-tripping.
	PreviousVersions PreviousVersionList `gorm:"serializer:json;type:jsonb;column:previous_versions" json:"previous_versions,omitempty"`

	// UpgradedAt is the timestamp of the most recent successful upgrade.
	UpgradedAt *time.Time `gorm:"column:upgraded_at" json:"upgraded_at,omitempty"`

	// UninstalledAt is the timestamp when the row last transitioned to
	// `uninstalled`. Drives discovery: rows with UninstalledAt != nil are
	// hidden from GET /api/addons by default.
	UninstalledAt *time.Time `gorm:"column:uninstalled_at" json:"uninstalled_at,omitempty"`

	// PurgeAt is when a background sweeper may drop the addon's schema for
	// retain_30d-policy uninstalls. nil for drop_now (already dropped) and
	// retain_forever (never drop).
	PurgeAt *time.Time `gorm:"column:purge_at" json:"purge_at,omitempty"`

	// DataPolicy records the uninstall-time choice so re-install / audit
	// can see whether storage was reclaimed. One of "drop_now",
	// "retain_30d", "retain_forever" (or empty for never-uninstalled rows).
	DataPolicy string `gorm:"size:20;column:data_policy" json:"data_policy,omitempty"`
}

// PreviousVersionList is a typed alias used so the JSON-serializer gorm tag
// resolves cleanly. The slice always stores versions oldest-first, capped at
// maxRollbackHistory entries.
type PreviousVersionList []string

// maxRollbackHistory caps how many past versions Rollback can target. Three
// is the operator-friendly default: enough to roll forward after a bad
// release plus one or two safety nets, small enough that history doesn't
// inflate the row indefinitely.
const maxRollbackHistory = 3

// Data-policy enum values for the Uninstall request body. The kernel does
// not validate the schema migrations themselves — addon-author concern —
// but it does enforce that only known policies pass through.
const (
	DataPolicyDropNow       = "drop_now"
	DataPolicyRetain30d     = "retain_30d"
	DataPolicyRetainForever = "retain_forever"
)

// retain30dDuration is the wall-clock window before a `retain_30d` policy
// hands the row off to the (eventual) purge sweeper. Exposed as a variable
// rather than a const so tests can shorten it; production keeps the default.
var retain30dDuration = 30 * 24 * time.Hour

func (Installation) TableName() string { return "marketplace_installations" }

// Handler wires the HTTP routes on top of a *gorm.DB.
type Handler struct {
	db            *gorm.DB
	installer     *installer.Installer
	httpc         *http.Client
	updateChecker UpdateChecker
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
//	POST /marketplace/install                  {addonKey, version, bundleURL}
//	POST /marketplace/uninstall                {addonKey, orgID?, dataPolicy?}
//	POST /marketplace/upgrade                  {addonKey, targetVersion, bundleURL, signature?}
//	POST /marketplace/rollback/:installationID
//	GET  /marketplace/installs                 → org's installations list
//
// `middleware` is layered first (typically the host's auth middleware).
func (h *Handler) Mount(r fiber.Router, middleware ...fiber.Handler) {
	mws := make([]any, 0, len(middleware))
	for _, mw := range middleware {
		if mw != nil {
			mws = append(mws, mw)
		}
	}
	g := r.Group("/marketplace", mws...)
	g.Post("/install", h.install)
	g.Post("/uninstall", h.uninstall)
	g.Post("/upgrade", h.upgrade)
	g.Post("/rollback/:installationID", h.rollback)
	g.Get("/installs", h.list)
}

// MountDiscovery wires the catalog/discovery endpoint under `/api`. Kept
// separate from Mount so hosts that want to expose the marketplace control
// plane on a different prefix from the public discovery list can opt in
// independently (e.g. `/marketplace` behind admin auth, `/api/addons`
// behind the regular org session).
//
//	GET /api/addons → installed addons for the current org, with has_update
//
// `middleware` is layered first; pass the host's auth middleware to scope
// the result to the caller's org.
func (h *Handler) MountDiscovery(r fiber.Router, middleware ...fiber.Handler) {
	mws := make([]any, 0, len(middleware))
	for _, mw := range middleware {
		if mw != nil {
			mws = append(mws, mw)
		}
	}
	g := r.Group("/api", mws...)
	g.Get("/addons", h.discoverAddons)
}

type installRequest struct {
	AddonKey  string `json:"addonKey"`
	Version   string `json:"version"`
	BundleURL string `json:"bundleURL"`
	// Optional metadata the iframe already has from the Hub catalog —
	// stored verbatim so the sidebar can render the addon by display
	// name instead of falling back to the raw key.
	Name      string `json:"name,omitempty"`
	Category  string `json:"category,omitempty"`
}

func (h *Handler) install(c fiber.Ctx) error {
	orgID := auth.GetOrganizationID(c)
	userID := auth.GetUserID(c)
	// Verbose diagnostic — temporary while we hunt the 401 in production.
	// Logs the raw locals types + claim dump so we can tell apart "no
	// auth middleware ran" from "auth ran but JWT lacked OrgID" from
	// "Locals key collision".
	claims := auth.GetClaims(c)
	claimsDump := "<nil>"
	if claims != nil {
		claimsDump = fmt.Sprintf("user=%s org=%s role=%s email=%s",
			claims.UserID, claims.OrganizationID, claims.Role, claims.Email)
	}
	rawOrg := c.Locals(auth.LocalOrganizationID)
	rawUser := c.Locals(auth.LocalUserID)
	log.Printf("[kernel-install] enter orgID=%s userID=%s rawOrgType=%T rawUserType=%T claims={%s} authHeader=%v",
		orgID, userID, rawOrg, rawUser, claimsDump, c.Get(fiber.HeaderAuthorization) != "")
	if orgID == uuid.Nil || userID == uuid.Nil {
		log.Printf("[kernel-install] AUTH FAIL → 401: orgID=%s userID=%s claims={%s}",
			orgID, userID, claimsDump)
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{
			"success": false,
			"message": "authenticated organization required",
		})
	}

	var req installRequest
	if err := c.Bind().Body(&req); err != nil {
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
		Name:           req.Name,
		Category:       req.Category,
		Version:        req.Version,
		BundleURL:      req.BundleURL,
		Status:         "requested",
		RequestedByID:  userID,
	}
	if err := h.db.WithContext(c).Create(&row).Error; err != nil {
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

// uninstallRequest accepts an optional orgID override for service-to-service
// calls; web sessions ignore it and use the authenticated org. DataPolicy
// defaults to retain_30d so a user clicking "Uninstall" without specifying a
// policy still gets a 30-day grace window before storage is reclaimed.
type uninstallRequest struct {
	AddonKey   string `json:"addonKey"`
	OrgID      string `json:"orgID,omitempty"`
	DataPolicy string `json:"dataPolicy,omitempty"`
}

func (h *Handler) uninstall(c fiber.Ctx) error {
	orgID := auth.GetOrganizationID(c)
	if orgID == uuid.Nil {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{
			"success": false,
			"message": "authenticated organization required",
		})
	}
	var req uninstallRequest
	if err := c.Bind().Body(&req); err != nil {
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
	policy, err := normaliseDataPolicy(req.DataPolicy)
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"success": false,
			"message": err.Error(),
		})
	}
	// Locate the marketplace row first — the kernel installer drives the
	// destructive work but we need the row to track state transitions and
	// to compute the purge_at deadline.
	var row Installation
	if err := h.db.WithContext(c).
		Where("organization_id = ? AND addon_key = ?", orgID, req.AddonKey).
		Order("requested_at DESC").
		Take(&row).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			return c.Status(fiber.StatusNotFound).JSON(fiber.Map{
				"success": false,
				"message": "no installation for addon " + req.AddonKey,
			})
		}
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"success": false,
			"message": err.Error(),
		})
	}

	// Mark uninstalling BEFORE we touch the installer so a concurrent
	// request sees the in-flight state instead of racing two destructive
	// flows against each other.
	h.markStatus(&row, "uninstalling", "")
	row.DataPolicy = policy

	// Drive the installer lifecycle hooks + (when drop_now) schema
	// destruction. Hosts that haven't wired an installer just persist the
	// row transition so a worker / cron can finish the job out-of-band.
	if h.installer != nil {
		dropNow := policy == DataPolicyDropNow
		if err := h.installer.Uninstall(orgID, req.AddonKey, dropNow); err != nil {
			h.markFailed(&row, fmt.Errorf("uninstall: %w", err))
			return c.Status(fiber.StatusUnprocessableEntity).JSON(fiber.Map{
				"success": false,
				"message": err.Error(),
				"data":    row,
			})
		}
	}

	now := time.Now()
	row.Status = "uninstalled"
	row.UninstalledAt = &now
	row.CompletedAt = &now
	row.ErrorMessage = ""
	switch policy {
	case DataPolicyRetain30d:
		purge := now.Add(retain30dDuration)
		row.PurgeAt = &purge
	case DataPolicyRetainForever:
		row.PurgeAt = nil
	case DataPolicyDropNow:
		// Schema already dropped by installer.Uninstall(dropNow=true).
		row.PurgeAt = nil
	}
	if err := h.db.Save(&row).Error; err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"success": false,
			"message": "persist uninstall: " + err.Error(),
		})
	}
	return c.JSON(fiber.Map{
		"success": true,
		"data":    row,
	})
}

// normaliseDataPolicy validates the policy enum and applies the default
// when empty. Anything outside the closed set is rejected so a typo cannot
// silently coerce to retain_30d.
func normaliseDataPolicy(p string) (string, error) {
	switch p {
	case "":
		return DataPolicyRetain30d, nil
	case DataPolicyDropNow, DataPolicyRetain30d, DataPolicyRetainForever:
		return p, nil
	default:
		return "", fmt.Errorf("dataPolicy must be one of %q, %q, %q (got %q)",
			DataPolicyDropNow, DataPolicyRetain30d, DataPolicyRetainForever, p)
	}
}

// upgradeRequest carries everything the kernel needs to swing an
// installation to a newer version. Signature is currently advisory — the
// installer re-verifies the bundle signature itself during Upgrade.
type upgradeRequest struct {
	AddonKey      string `json:"addonKey"`
	OrgID         string `json:"orgID,omitempty"`
	TargetVersion string `json:"targetVersion"`
	BundleURL     string `json:"bundleURL"`
	Signature     string `json:"signature,omitempty"`
}

func (h *Handler) upgrade(c fiber.Ctx) error {
	orgID := auth.GetOrganizationID(c)
	if orgID == uuid.Nil {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{
			"success": false,
			"message": "authenticated organization required",
		})
	}
	var req upgradeRequest
	if err := c.Bind().Body(&req); err != nil {
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
	if req.TargetVersion == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"success": false,
			"message": "targetVersion is required",
		})
	}
	if req.BundleURL == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"success": false,
			"message": "bundleURL is required",
		})
	}
	// Look up the marketplace row so we can record the version transition
	// and detect "addon not installed" before paying the bundle download.
	var row Installation
	if err := h.db.WithContext(c).
		Where("organization_id = ? AND addon_key = ?", orgID, req.AddonKey).
		Order("requested_at DESC").
		Take(&row).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			return c.Status(fiber.StatusNotFound).JSON(fiber.Map{
				"success": false,
				"message": "no installation for addon " + req.AddonKey,
			})
		}
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"success": false,
			"message": err.Error(),
		})
	}
	if row.Status == "uninstalled" {
		return c.Status(fiber.StatusConflict).JSON(fiber.Map{
			"success": false,
			"message": "cannot upgrade an uninstalled addon (reinstall instead)",
			"data":    row,
		})
	}

	previousVersion := row.Version
	previousBundleURL := row.BundleURL
	h.markStatus(&row, "upgrading", "")

	// Lite mode — no installer, just record the upgrade intent so a worker
	// can pick it up. Keeps the public contract usable for hosts that drive
	// installs out-of-band.
	if h.installer == nil {
		now := time.Now()
		row.PreviousVersion = previousVersion
		row.PreviousVersions = appendCappedHistory(row.PreviousVersions, previousVersion)
		row.Version = req.TargetVersion
		row.BundleURL = req.BundleURL
		row.UpgradedAt = &now
		row.CompletedAt = &now
		row.Status = "installed"
		row.ErrorMessage = ""
		if err := h.db.Save(&row).Error; err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
				"success": false,
				"message": "persist upgrade: " + err.Error(),
			})
		}
		return c.JSON(fiber.Map{
			"success": true,
			"data":    row,
		})
	}

	// Full pipeline: download new bundle, hand it to installer.Upgrade.
	// installer.Upgrade verifies signature + kernel range + runs forward
	// migrations + dispatches lifecycle.upgrade hooks; a failure leaves the
	// installer's metacore_installations row untouched, so we rollback the
	// marketplace row to the previous status.
	b, err := h.fetchBundle(req.BundleURL, req.AddonKey)
	if err != nil {
		h.markFailed(&row, err)
		// Restore the prior installed version on the marketplace row — the
		// installer never committed the upgrade so the addon is still on
		// previousVersion as far as everything downstream is concerned.
		row.Version = previousVersion
		row.BundleURL = previousBundleURL
		row.Status = "installed"
		row.ErrorMessage = err.Error()
		_ = h.db.Save(&row).Error
		return c.Status(fiber.StatusUnprocessableEntity).JSON(fiber.Map{
			"success": false,
			"message": err.Error(),
			"data":    row,
		})
	}
	if req.TargetVersion != "" && b.Manifest.Version != req.TargetVersion {
		err := fmt.Errorf("bundle version %q does not match targetVersion %q",
			b.Manifest.Version, req.TargetVersion)
		h.markFailed(&row, err)
		row.Version = previousVersion
		row.BundleURL = previousBundleURL
		row.Status = "installed"
		row.ErrorMessage = err.Error()
		_ = h.db.Save(&row).Error
		return c.Status(fiber.StatusUnprocessableEntity).JSON(fiber.Map{
			"success": false,
			"message": err.Error(),
			"data":    row,
		})
	}

	if _, err := h.installer.Upgrade(c, orgID, b); err != nil {
		h.markFailed(&row, err)
		row.Version = previousVersion
		row.BundleURL = previousBundleURL
		row.Status = "installed"
		row.ErrorMessage = err.Error()
		_ = h.db.Save(&row).Error
		status := fiber.StatusUnprocessableEntity
		return c.Status(status).JSON(fiber.Map{
			"success": false,
			"message": err.Error(),
			"data":    row,
		})
	}

	now := time.Now()
	row.PreviousVersion = previousVersion
	row.PreviousVersions = appendCappedHistory(row.PreviousVersions, previousVersion)
	row.Version = req.TargetVersion
	row.BundleURL = req.BundleURL
	row.UpgradedAt = &now
	row.CompletedAt = &now
	row.Status = "installed"
	row.ErrorMessage = ""
	if err := h.db.Save(&row).Error; err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"success": false,
			"message": "persist upgrade: " + err.Error(),
		})
	}
	return c.JSON(fiber.Map{
		"success": true,
		"data":    row,
	})
}

// appendCappedHistory appends `v` to the version history, ignoring empty
// versions and capping the slice at maxRollbackHistory entries by dropping
// the oldest. Returns a new slice so the caller can assign it directly.
func appendCappedHistory(prev PreviousVersionList, v string) PreviousVersionList {
	if v == "" {
		return prev
	}
	out := make(PreviousVersionList, 0, len(prev)+1)
	out = append(out, prev...)
	out = append(out, v)
	if len(out) > maxRollbackHistory {
		out = out[len(out)-maxRollbackHistory:]
	}
	return out
}

func (h *Handler) rollback(c fiber.Ctx) error {
	orgID := auth.GetOrganizationID(c)
	if orgID == uuid.Nil {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{
			"success": false,
			"message": "authenticated organization required",
		})
	}
	idParam := c.Params("installationID")
	id, err := uuid.Parse(idParam)
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"success": false,
			"message": "invalid installationID: " + err.Error(),
		})
	}
	var row Installation
	if err := h.db.WithContext(c).
		Where("id = ? AND organization_id = ?", id, orgID).
		Take(&row).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			return c.Status(fiber.StatusNotFound).JSON(fiber.Map{
				"success": false,
				"message": "installation not found",
			})
		}
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"success": false,
			"message": err.Error(),
		})
	}
	if len(row.PreviousVersions) == 0 && row.PreviousVersion == "" {
		return c.Status(fiber.StatusConflict).JSON(fiber.Map{
			"success": false,
			"message": "no previous version to rollback to",
			"data":    row,
		})
	}
	// Pick the most recent entry from the history. Cap at maxRollbackHistory
	// is already enforced on write; we re-check on read so a manually
	// edited row can't bypass the limit.
	history := row.PreviousVersions
	if len(history) > maxRollbackHistory {
		history = history[len(history)-maxRollbackHistory:]
	}
	var target string
	if len(history) > 0 {
		target = history[len(history)-1]
	} else {
		target = row.PreviousVersion
	}
	if target == "" {
		return c.Status(fiber.StatusConflict).JSON(fiber.Map{
			"success": false,
			"message": "no previous version recorded",
			"data":    row,
		})
	}
	if target == row.Version {
		return c.Status(fiber.StatusConflict).JSON(fiber.Map{
			"success": false,
			"message": "rollback target matches current version",
			"data":    row,
		})
	}

	currentVersion := row.Version
	h.markStatus(&row, "rolling_back", "")

	// Without an installer we run lite mode — flip the row to the previous
	// version so the operator can see the intent recorded. Hosts wiring
	// the full installer should re-fetch the previous bundle (BundleURL is
	// preserved alongside the version history in real deployments).
	now := time.Now()
	// Trim the consumed entry from history so subsequent rollbacks step
	// further back instead of looping on the same target.
	if len(history) > 0 {
		row.PreviousVersions = PreviousVersionList(history[:len(history)-1])
	}
	row.PreviousVersion = currentVersion
	row.Version = target
	row.UpgradedAt = &now
	row.CompletedAt = &now
	row.Status = "installed"
	row.ErrorMessage = ""
	if err := h.db.Save(&row).Error; err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"success": false,
			"message": "persist rollback: " + err.Error(),
		})
	}
	return c.JSON(fiber.Map{
		"success": true,
		"data":    row,
	})
}

// addonDiscovery is the shape returned by GET /api/addons. It is a
// pared-down projection of Installation — the discovery list is meant to
// drive a sidebar / launcher, not surface bundle URLs or secret material.
type addonDiscovery struct {
	AddonKey    string     `json:"addon_key"`
	Name        string     `json:"name,omitempty"`
	Version     string     `json:"version"`
	Status      string     `json:"status"`
	InstalledAt time.Time  `json:"installed_at"`
	HasUpdate   bool       `json:"has_update"`
	// LatestVersion is populated when an UpdateChecker reports a newer
	// version is available. nil when the checker is not wired.
	LatestVersion string `json:"latest_version,omitempty"`
}

// UpdateChecker reports whether a newer version exists for an addon. Hosts
// wire a real implementation (Hub API client, manifest-cache reader, …) via
// WithUpdateChecker. A nil checker means GET /api/addons always returns
// has_update=false — discovery still works, it just can't tell the UI a
// shiny new release is available.
type UpdateChecker interface {
	LatestVersion(ctx context.Context, addonKey string) (string, error)
}

// UpdateCheckerFunc adapts a function to UpdateChecker for hosts that don't
// need a struct (one-liner Hub API call, in-memory stub for tests).
type UpdateCheckerFunc func(ctx context.Context, addonKey string) (string, error)

// LatestVersion satisfies UpdateChecker.
func (f UpdateCheckerFunc) LatestVersion(ctx context.Context, addonKey string) (string, error) {
	return f(ctx, addonKey)
}

// WithUpdateChecker wires the optional has_update probe used by GET
// /api/addons. Without it, has_update is always false (still useful — the
// sidebar can render the list immediately and surface updates later).
func WithUpdateChecker(u UpdateChecker) HandlerOption {
	return func(h *Handler) { h.updateChecker = u }
}

func (h *Handler) discoverAddons(c fiber.Ctx) error {
	orgID := auth.GetOrganizationID(c)
	if orgID == uuid.Nil {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{
			"success": false,
			"message": "authenticated organization required",
		})
	}
	var rows []Installation
	// Active installations only: exclude rows that were uninstalled. The
	// sidebar should not surface an addon that was removed; rollback /
	// reinstall flows live on the dedicated marketplace surfaces.
	if err := h.db.WithContext(c).
		Where("organization_id = ? AND (status = ? OR status IS NULL)", orgID, "installed").
		Order("requested_at DESC").
		Find(&rows).Error; err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"success": false,
			"message": err.Error(),
		})
	}
	out := make([]addonDiscovery, 0, len(rows))
	for _, r := range rows {
		d := addonDiscovery{
			AddonKey:    r.AddonKey,
			Name:        r.Name,
			Version:     r.Version,
			Status:      r.Status,
			InstalledAt: r.RequestedAt,
		}
		if h.updateChecker != nil {
			latest, err := h.updateChecker.LatestVersion(c, r.AddonKey)
			if err == nil && latest != "" && latest != r.Version {
				d.HasUpdate = true
				d.LatestVersion = latest
			}
		}
		out = append(out, d)
	}
	return c.JSON(fiber.Map{
		"success": true,
		"data":    out,
	})
}

func (h *Handler) list(c fiber.Ctx) error {
	orgID := auth.GetOrganizationID(c)
	if orgID == uuid.Nil {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{
			"success": false,
			"message": "authenticated organization required",
		})
	}
	var rows []Installation
	if err := h.db.WithContext(c).
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
