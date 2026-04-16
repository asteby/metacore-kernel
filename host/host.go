// Package host is the single object a consuming app (ops, link) mounts to
// get the whole kernel wired together: lifecycles, interceptors, installer,
// navigation, and on-boot service injection.
package host

import (
	"github.com/asteby/metacore-kernel/installer"
	"github.com/asteby/metacore-kernel/lifecycle"
	"github.com/asteby/metacore-sdk/pkg/manifest"
	"github.com/asteby/metacore-kernel/navigation"
	"github.com/asteby/metacore-kernel/runtime/wasm"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// Host is the kernel facade. Build it once at startup, pass it to handlers.
type Host struct {
	Installer     *installer.Installer
	Lifecycles    *lifecycle.Registry
	Interceptors  *lifecycle.InterceptorRegistry
	KernelVersion string

	// WASM is the optional in-process addon runtime. Populated by EnableWASM;
	// consumers can check for nil to keep v2 (webhook-only) deployments working.
	WASM *wasm.Host

	db       *gorm.DB
	services map[string]any
}

// Config configures a new Host.
type Config struct {
	DB            *gorm.DB
	KernelVersion string
	// Services are injected into addon Boot() calls (e.g. "eventbus", "fiscal").
	Services map[string]any
}

// New builds a Host and runs AutoMigrate for kernel-owned tables.
func New(cfg Config) (*Host, error) {
	inst := installer.New(cfg.DB, cfg.KernelVersion)
	if err := cfg.DB.AutoMigrate(&installer.Installation{}); err != nil {
		return nil, err
	}
	return &Host{
		Installer:     inst,
		Lifecycles:    inst.Lifecycles,
		Interceptors:  inst.Interceptors,
		KernelVersion: cfg.KernelVersion,
		db:            cfg.DB,
		services:      cfg.Services,
	}, nil
}

// RegisterCompiled registers a compiled-in addon before Boot. Typically called
// from an init() in the host binary so it runs before New returns.
func (h *Host) RegisterCompiled(key string, a lifecycle.Addon) {
	h.Lifecycles.Register(key, a)
}

// Boot calls every registered addon's Boot hook with the shared services.
// Call once after all services (DB, eventbus, etc.) are initialized.
func (h *Host) Boot() error {
	ctx := &lifecycle.BootContext{DB: h.db, Services: h.services}
	for _, a := range h.Lifecycles.All() {
		if b, ok := a.(lifecycle.Bootstrapper); ok {
			if err := b.Boot(ctx); err != nil {
				return err
			}
		}
	}
	return nil
}

// Navigation merges core sidebar groups with contributions from every addon
// enabled in the given organization.
func (h *Host) Navigation(orgID uuid.UUID, core []navigation.Group) ([]navigation.Group, error) {
	var installs []installer.Installation
	if err := h.db.
		Where("organization_id = ? AND status = ?", orgID, "enabled").
		Find(&installs).Error; err != nil {
		return nil, err
	}
	var contribs []navigation.Contribution
	for _, inst := range installs {
		lc, ok := h.Lifecycles.Get(inst.AddonKey)
		if !ok {
			continue
		}
		contribs = append(contribs, navigation.Contribution{
			AddonKey: inst.AddonKey,
			Groups:   lc.Manifest().Navigation,
		})
	}
	return navigation.Build(core, contribs), nil
}

// InstalledManifests returns manifests of every enabled addon for an org —
// used by the frontend to drive federation loading and slot registration.
func (h *Host) InstalledManifests(orgID uuid.UUID) ([]manifest.Manifest, error) {
	var installs []installer.Installation
	if err := h.db.
		Where("organization_id = ? AND status = ?", orgID, "enabled").
		Find(&installs).Error; err != nil {
		return nil, err
	}
	out := make([]manifest.Manifest, 0, len(installs))
	for _, inst := range installs {
		lc, ok := h.Lifecycles.Get(inst.AddonKey)
		if !ok {
			continue
		}
		out = append(out, lc.Manifest())
	}
	return out, nil
}
