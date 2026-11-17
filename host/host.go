// Package host is the single object a consuming app mounts to get the whole
// kernel wired together: lifecycles, interceptors, installer, navigation,
// and on-boot service injection.
package host

import (
	"log/slog"
	"sync"

	"github.com/asteby/metacore-kernel/dynamic"
	"github.com/asteby/metacore-kernel/installer"
	"github.com/asteby/metacore-kernel/lifecycle"
	"github.com/asteby/metacore-kernel/manifest"
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

	// driftWarned rate-limits InstalledManifests warnings.
	driftWarned sync.Map
}

// Config configures a new Host.
type Config struct {
	DB            *gorm.DB
	KernelVersion string
	// Services are injected into addon Boot() calls (e.g. "eventbus", "fiscal").
	Services map[string]any

	// HookRunner, when set, is wired into installer.Installer so
	// manifest.LifecycleHooks fire on Install/Enable/Disable/Uninstall.
	// Hosts typically construct it via lifecycle.NewHookRunner, register
	// the wasm + webhook dispatchers, and pass it through here. Leaving
	// it nil keeps the pre-implementation behaviour (declared hooks are
	// validated but never dispatched).
	HookRunner *lifecycle.HookRunner

	// DynamicHooks is the dynamic.HookRegistry the installer projects
	// manifest before_*/after_* CRUD hooks into. Must be the same
	// instance that was passed to dynamic.New through dynamic.Config.Hooks
	// (typically host.App.DynamicHooks when both host.App and host.Host
	// are wired into the same app).
	DynamicHooks *dynamic.HookRegistry
}

// New builds a Host and runs AutoMigrate for kernel-owned tables.
func New(cfg Config) (*Host, error) {
	inst := installer.New(cfg.DB, cfg.KernelVersion)
	inst.HookRunner = cfg.HookRunner
	inst.DynamicHooks = cfg.DynamicHooks
	if err := installer.EnsureSchema(cfg.DB); err != nil {
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
	enabled := make(map[string]bool, len(installs))
	for _, inst := range installs {
		enabled[inst.AddonKey] = true
		lc, ok := h.Lifecycles.Get(inst.AddonKey)
		if !ok {
			continue
		}
		contribs = append(contribs, navigation.Contribution{
			AddonKey: inst.AddonKey,
			Groups:   lc.Manifest().Navigation,
		})
	}
	// Las entradas con `condition` (addon_installed: fiscal_mexico en el menú
	// de Facturas de customers, p. ej.) se resuelven contra los addons
	// HABILITADOS de la org: con Build (installed nil) se servían siempre, y un
	// addon apagado seguía dejando sus entradas en el menú de otro.
	return navigation.BuildFor(core, contribs, func(key string) bool { return enabled[key] }), nil
}

// InstalledManifests returns manifests of every enabled addon for an org —
// used by the frontend to drive federation loading and slot registration.
//
// The reported Version ALWAYS comes from the metacore_installations row (the
// source of truth), never from the in-memory lifecycle registry. The registry
// is populated at boot / Install / Upgrade and can lag the table (another
// replica upgraded, a same-version Upgrade that was rejected, a manual SQL
// fix...); the UI compares this version against the marketplace to decide
// whether to show "Actualizar", so a stale value made the badge stick until a
// service restart. When memory and table disagree the manifest is returned as
// a COPY with Version overridden (the shared registry entry is not mutated)
// and a rate-limited warning is logged to expose the drift.
//
// An enabled installation with NO lifecycle in memory is omitted (and logged,
// once per key): without the manifest there is nothing to drive federation
// loading, slots or navigation, and fabricating a stub manifest from the row
// would hand the frontend an addon with empty contributions. This matches
// Navigation, which also skips such rows. The log makes the gap visible.
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
			h.warnOnce("missing:"+inst.AddonKey, "metacore: enabled installation has no lifecycle in memory; omitted from installed manifests",
				"addon", inst.AddonKey, "table_version", inst.Version)
			continue
		}
		m := lc.Manifest() // by value: safe to adjust without touching the registry
		if inst.Version != "" && m.Version != inst.Version {
			h.warnOnce("drift:"+inst.AddonKey+":"+inst.Version+":"+m.Version,
				"metacore: in-memory manifest version differs from metacore_installations; reporting the table version",
				"addon", inst.AddonKey, "table_version", inst.Version, "memory_version", m.Version)
			m.Version = inst.Version
		}
		out = append(out, m)
	}
	return out, nil
}

// warnOnce logs msg at most once per dedupe key for the life of the Host.
func (h *Host) warnOnce(dedupe, msg string, args ...any) {
	if _, loaded := h.driftWarned.LoadOrStore(dedupe, struct{}{}); !loaded {
		slog.Warn(msg, args...)
	}
}
