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
	"strings"
	"time"

	"github.com/asteby/metacore-kernel/auth"
	"github.com/asteby/metacore-kernel/bundle"
	"github.com/asteby/metacore-kernel/installer"
	"github.com/asteby/metacore-kernel/manifest"
	v3 "github.com/asteby/metacore-kernel/manifest/v3"
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

	// Requires is the list of OTHER addon keys this installation declares
	// under `compatibility.requires[]` (excluding the reserved "kernel" key).
	// It is captured at install time from the bundle manifest so the uninstall
	// path can answer "who depends on X?" with a single indexed query instead
	// of walking every other installation's manifest blob. Empty for lite-mode
	// installs (no bundle parsed) and for legacy rows that predate the field.
	Requires AddonKeyList `gorm:"serializer:json;type:jsonb;column:requires" json:"requires,omitempty"`
}

// AddonKeyList is the typed alias used for the JSON-serialised Requires column
// so the gorm serializer tag resolves cleanly under sqlite and postgres alike.
type AddonKeyList []string

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
	// Capture the addon's declared addon-level dependencies (excluding the
	// reserved "kernel" key) so the uninstall handler can compute the reverse
	// dep graph without re-reading bundles. extractRequires reads the bundle's
	// preserved RawManifest (the only place the v3 compatibility.requires list
	// survives — manifest.FromV3 only forwards the kernel range). Empty for
	// legacy v2 bundles or v3 bundles that declare no addon-level deps.
	row.Requires = extractRequires(b)
	_ = h.db.Save(&row).Error

	return c.Status(fiber.StatusCreated).JSON(fiber.Map{
		"success":      true,
		"data":         row,
		"installation": inst,
	})
}

// extractRequires reads the bundle's preserved v3 RawManifest and returns the
// addon-level deps declared under compatibility.requires[] (the reserved
// "kernel" key is filtered — it is a runtime range, not a dep). Returns nil
// for legacy v2 bundles or v3 manifests that declare no requires. The bundle
// is read defensively: any parse error degrades to nil so a malformed manifest
// that nonetheless installed (the security gate is upstream) cannot crash the
// dep-graph capture.
func extractRequires(b *bundle.Bundle) AddonKeyList {
	if b == nil || len(b.RawManifest) == 0 {
		return nil
	}
	m, err := v3.Parse(b.RawManifest)
	if err != nil || m == nil {
		return nil
	}
	if len(m.Compatibility.Requires) == 0 {
		return nil
	}
	out := make(AddonKeyList, 0, len(m.Compatibility.Requires))
	for _, r := range m.Compatibility.Requires {
		key := strings.TrimSpace(r.Key)
		if key == "" || key == "kernel" {
			continue
		}
		out = append(out, key)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// fetchBundle downloads the .tar.gz from the Hub and parses it through
// kernel/bundle.Read. The addon key is checked against the bundle manifest
// to catch URL/key mismatches early.
//
// Central trust anchor wiring: the hub counter-signs every served bundle
// with its central Ed25519 key (Let's Encrypt-style) and ships the hex
// signature in the X-Asteby-Marketplace-Signature header (see
// installer.HeaderMarketplaceSignature). The publisher's own embedded
// manifest.Signature (if any) is kept; otherwise the header signature is
// injected here so installer/security.VerifyBundle can verify it against
// the trusted MARKETPLACE_PUBKEY set the installer auto-fetches at boot.
// Without this injection the bundle reaches the security gate as unsigned
// and the install fails with "security: bundle has no signature" even
// though the marketplace signed it correctly.
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
	injectCentralSignature(b, resp.Header)
	return b, nil
}

// injectCentralSignature copies the marketplace counter-signature from the
// HTTP response headers into the in-memory manifest.Signature so the
// security gate sees a signature to verify. Publisher-embedded signatures
// take precedence — multi-key trust means either may verify. Split out so
// the upgrade flow (which also calls fetchBundle's HTTP path) can share it.
func injectCentralSignature(b *bundle.Bundle, headers http.Header) {
	if b == nil {
		return
	}
	if b.Manifest.Signature != nil && strings.TrimSpace(b.Manifest.Signature.Value) != "" {
		return
	}
	sig := strings.TrimSpace(headers.Get(installer.HeaderMarketplaceSignature))
	if sig == "" {
		return
	}
	b.Manifest.Signature = &manifest.Signature{
		Algorithm: "ed25519",
		Value:     sig,
		Digest:    strings.TrimSpace(headers.Get(installer.HeaderBundleChecksum)),
		Verified:  true,
	}
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
//
// Force/Cascade are mutually compatible escape hatches around the dep-block
// gate the handler runs by default (refuses to uninstall an addon some other
// installed row declares under compatibility.requires[]):
//
//	force=true   — skip the dep-block check entirely. The dependent will be
//	               left broken; only an operator who knows what they are
//	               doing should reach for this. Surfaced as the "admin
//	               override" path in the UI.
//	cascade=true — walk the reverse dep graph and uninstall every dependent
//	               leaf-first before the requested addon. Surfaced from the
//	               409 dialog as "Desinstalar X y sus N dependientes".
//
// When both are set cascade wins (a cascade implicitly satisfies the dep-block
// without skipping it).
type uninstallRequest struct {
	AddonKey   string `json:"addonKey"`
	OrgID      string `json:"orgID,omitempty"`
	DataPolicy string `json:"dataPolicy,omitempty"`
	Force      bool   `json:"force,omitempty"`
	Cascade    bool   `json:"cascade,omitempty"`
}

// dependent is the projection returned in the 409 body so the UI can list the
// addons blocking the uninstall by display name. Kept tiny on purpose — the
// dialog only renders {key, name}, anything richer (version, category) would
// invite scope creep on the API contract.
type dependent struct {
	Key  string `json:"key"`
	Name string `json:"name,omitempty"`
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

	// Dep-block gate. Refuse to uninstall an addon some OTHER active
	// installation declares under compatibility.requires[]; the dependent
	// would silently break otherwise. Two escape hatches:
	//
	//   force=true   → skip the check (admin override).
	//   cascade=true → uninstall every dependent leaf-first before the
	//                  requested addon (the "yes, really gone" hatch).
	//
	// When the gate fires without either flag we return 409 + the dependents
	// list so the UI can render the "primero desinstalá X" dialog with the
	// fallback cascade button.
	if !req.Force && !req.Cascade {
		deps, err := h.findDependents(c, orgID, req.AddonKey)
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
				"success": false,
				"message": "dependents lookup: " + err.Error(),
			})
		}
		if len(deps) > 0 {
			return c.Status(fiber.StatusConflict).JSON(fiber.Map{
				"success": false,
				"message": fmt.Sprintf("cannot uninstall %s: %d installed addon(s) depend on it", req.AddonKey, len(deps)),
				"data": fiber.Map{
					"dependents": deps,
				},
			})
		}
	}

	// Cascade: walk the reverse dep graph and uninstall every dependent
	// leaf-first before the requested addon. Failures inside the cascade
	// abort the whole operation — the requested addon stays installed so
	// the UI doesn't end up with a half-torn-down dep tree it can't reason
	// about. Hosts that want partial-progress semantics can drive the
	// individual uninstalls themselves.
	if req.Cascade {
		victims, err := h.reverseDepOrder(c, orgID, req.AddonKey)
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
				"success": false,
				"message": "cascade plan: " + err.Error(),
			})
		}
		uninstalled := make([]Installation, 0, len(victims))
		for _, v := range victims {
			done, err := h.runUninstall(c, orgID, v, policy)
			if err != nil {
				return c.Status(fiber.StatusUnprocessableEntity).JSON(fiber.Map{
					"success": false,
					"message": fmt.Sprintf("cascade uninstall of %s: %s", v, err.Error()),
					"data": fiber.Map{
						"uninstalled": uninstalled,
					},
				})
			}
			uninstalled = append(uninstalled, done)
		}
		return c.JSON(fiber.Map{
			"success": true,
			"data": fiber.Map{
				"uninstalled": uninstalled,
				// The requested addon is the last entry — surface it
				// explicitly so the UI can flip the "Desinstalado" toast
				// without recomputing the order client-side.
				"primary": uninstalled[len(uninstalled)-1],
			},
		})
	}

	row, err := h.runUninstall(c, orgID, req.AddonKey, policy)
	if err != nil {
		// runUninstall returns ErrUninstallNotFound for missing rows so the
		// handler can map it to a 404 without leaking internals. Everything
		// else is a 422 with the persisted-failed row attached.
		if err == errUninstallNotFound {
			return c.Status(fiber.StatusNotFound).JSON(fiber.Map{
				"success": false,
				"message": "no installation for addon " + req.AddonKey,
			})
		}
		return c.Status(fiber.StatusUnprocessableEntity).JSON(fiber.Map{
			"success": false,
			"message": err.Error(),
			"data":    row,
		})
	}
	return c.JSON(fiber.Map{
		"success": true,
		"data":    row,
	})
}

// errUninstallNotFound is the sentinel runUninstall returns when the addon row
// for the org does not exist. The handler maps it to 404; callers driving the
// helper directly (cascade) can ignore it (a planned victim that vanished
// between plan and execute is treated as "already gone" and dropped silently).
var errUninstallNotFound = fmt.Errorf("marketplace: no installation for addon")

// runUninstall is the shared per-addon uninstall step used by both the direct
// path and the cascade walker. It re-runs the row lookup, marks the row
// uninstalling, drives the installer lifecycle hooks (when wired), and
// persists the final state with the correct PurgeAt window for the policy.
// Returns the persisted row (success or failure) so the handler can attach it
// to the response body.
func (h *Handler) runUninstall(c fiber.Ctx, orgID uuid.UUID, addonKey, policy string) (Installation, error) {
	var row Installation
	if err := h.db.WithContext(c).
		Where("organization_id = ? AND addon_key = ?", orgID, addonKey).
		Order("requested_at DESC").
		Take(&row).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			return Installation{}, errUninstallNotFound
		}
		return Installation{}, err
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
		if err := h.installer.Uninstall(orgID, addonKey, dropNow); err != nil {
			wrapped := fmt.Errorf("uninstall: %w", err)
			h.markFailed(&row, wrapped)
			return row, wrapped
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
		return row, fmt.Errorf("persist uninstall: %w", err)
	}
	return row, nil
}

// findDependents returns the OTHER active installations in the same org whose
// Requires list contains addonKey. The query uses the json serializer column
// rendered to string (sqlite + postgres both round-trip a JSON array as
// `["a","b"]`); we lift the rows in Go and match exactly so a substring
// collision ("customers" vs "customers_pro") cannot fire a false positive.
//
// Returns dependents oldest-first so the UI lists them in install order — the
// dep that landed first is usually the one the user remembers installing.
func (h *Handler) findDependents(c fiber.Ctx, orgID uuid.UUID, addonKey string) ([]dependent, error) {
	var rows []Installation
	if err := h.db.WithContext(c).
		Where("organization_id = ? AND status = ? AND addon_key <> ?", orgID, "installed", addonKey).
		Order("requested_at ASC").
		Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]dependent, 0)
	for _, r := range rows {
		for _, dep := range r.Requires {
			if dep == addonKey {
				name := r.Name
				if name == "" {
					name = r.AddonKey
				}
				out = append(out, dependent{Key: r.AddonKey, Name: name})
				break
			}
		}
	}
	return out, nil
}

// reverseDepOrder builds the leaf-first uninstall plan for a cascade. Starting
// from the requested addon it walks the reverse-dep tree (who depends on me?)
// breadth-first, then reverses the visit order so leaves (no further
// dependents) come out first. The requested addon is always the last entry.
//
// Cycles are tolerated (visited set), and missing rows are skipped silently —
// a planned victim that was uninstalled out-of-band between plan and execute
// is treated as already gone. Returns a slice of addon keys.
func (h *Handler) reverseDepOrder(c fiber.Ctx, orgID uuid.UUID, root string) ([]string, error) {
	visited := map[string]bool{root: true}
	order := []string{root}
	queue := []string{root}
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		deps, err := h.findDependents(c, orgID, current)
		if err != nil {
			return nil, err
		}
		for _, d := range deps {
			if visited[d.Key] {
				continue
			}
			visited[d.Key] = true
			order = append(order, d.Key)
			queue = append(queue, d.Key)
		}
	}
	// Reverse: BFS visits root → direct dependents → transitive dependents,
	// so the slice above is root-first. The uninstall plan needs leaf-first
	// (uninstall things that depend on X before X), so flip it.
	out := make([]string, len(order))
	for i, k := range order {
		out[len(order)-1-i] = k
	}
	return out, nil
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
