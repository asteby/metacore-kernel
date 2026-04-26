// bridge.go is the kernel-side glue a host app mounts once at startup so the
// kernel runs alongside any pre-existing addon system. It owns the *host.Host,
// hydrates lifecycle entries from the host's legacy manifests (if any),
// builds a shared *security.WebhookDispatcher, and lazily compiles the
// capability *security.Enforcer.
//
// The bridge is intentionally thin: it never talks to host model packages.
// Instead it consumes the interfaces declared in ports.go. Each host supplies
// concrete implementations at startup.
package bridge

import (
	"context"
	"fmt"

	"github.com/asteby/metacore-kernel/bundle"
	"github.com/asteby/metacore-kernel/host"
	"github.com/asteby/metacore-kernel/lifecycle"
	"github.com/asteby/metacore-kernel/manifest"
	"github.com/asteby/metacore-kernel/navigation"
	"github.com/asteby/metacore-kernel/security"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// KernelVersion is the semver this bridge advertises to addons via the
// manifest.Kernel compatibility check, and echoes back to the frontend
// for diagnostics.
const KernelVersion = "2.0.0"

// Config bundles the dependencies a host supplies when constructing the
// bridge. All fields except DB and Services are optional: hosts that don't
// ship a legacy addon system, agents, or capability enforcement leave them
// nil and the bridge degrades gracefully.
type Config struct {
	DB       *gorm.DB
	Services map[string]any

	// LegacyManifests is the host's pre-existing addon registry. The bridge
	// imports each manifest as a lifecycle.ManifestOnly so navigation /
	// installed-manifests answer consistently across kernel-native and
	// legacy addons. Nil = host has no legacy system.
	LegacyManifests LegacyManifestSource

	// SecretResolver returns the cleartext install-time secret for an
	// installation. When nil, outbound webhooks fall through to unsigned.
	SecretResolver SecretResolver
}

// Bridge is a thin adapter around kernel/host.Host the host mounts once at
// startup. It lives next to whatever legacy addon registry the host already
// owns — never replacing it — so handlers can read from either side while
// migration is in flight.
type Bridge struct {
	host *host.Host
	db   *gorm.DB

	// webhookDispatcher signs outbound addon webhooks. Lazily built on
	// first WebhookDispatcher() call.
	webhookDispatcher *security.WebhookDispatcher

	// secretResolver feeds the dispatcher's signer lookup.
	secretResolver SecretResolver

	// enforcer gates capability checks. Defaults to shadow mode; hosts can
	// flip it to enforce by toggling METACORE_ENFORCE in their wiring.
	enforcer *security.Enforcer
}

// New builds the kernel host, seeds its lifecycle registry with legacy
// manifests (if any), and returns a ready-to-use bridge. It does not call
// Boot() — the host decides when to Boot after all services are wired.
func New(cfg Config) (*Bridge, error) {
	if cfg.DB == nil {
		return nil, fmt.Errorf("bridge: nil DB")
	}
	h, err := host.New(host.Config{
		DB:            cfg.DB,
		KernelVersion: KernelVersion,
		Services:      cfg.Services,
	})
	if err != nil {
		return nil, fmt.Errorf("bridge: new host: %w", err)
	}
	b := &Bridge{
		host:           h,
		db:             cfg.DB,
		secretResolver: cfg.SecretResolver,
	}
	if cfg.LegacyManifests != nil {
		b.importLegacyManifests(cfg.LegacyManifests)
	}
	// Enable in-process WASM runtime. Harmless for legacy webhook addons;
	// addons with manifest.backend.runtime=wasm get loaded when the
	// installer calls LoadAddonBundle after extracting the tarball.
	if err := h.EnableWASM(context.Background(), nil); err != nil {
		// Not fatal — webhook addons keep working.
		fmt.Printf("bridge: wasm disabled (%v)\n", err)
	}
	return b, nil
}

// LoadAddonBundle wires a freshly-installed bundle's backend.wasm (if any)
// into the in-process runtime. Migrations + metadata still go through the
// installer first.
func (b *Bridge) LoadAddonBundle(bnd *bundle.Bundle) error {
	return b.host.LoadWASMFromBundle(context.Background(), bnd)
}

// importLegacyManifests mirrors every manifest already known to the host's
// legacy registry into the kernel. Compiled addons already living in the
// legacy registry get a ManifestOnly shim — enough for the kernel to answer
// navigation / installed-manifests queries consistently. When a compiled
// kernel-native addon is registered directly against the kernel later, this
// shim is simply not re-added (Register is idempotent per key).
func (b *Bridge) importLegacyManifests(src LegacyManifestSource) {
	for key, lc := range src.All() {
		if _, exists := b.host.Lifecycles.Get(key); exists {
			continue
		}
		km := lc.Manifest()
		b.host.Lifecycles.Register(key, &lifecycle.ManifestOnly{Data: km})
	}
}

// Host returns the wrapped kernel host so other services / handlers can use
// the kernel API directly (installer, interceptors, etc.).
func (b *Bridge) Host() *host.Host { return b.host }

// DB returns the shared *gorm.DB so other services (handlers, actions
// bridge) can perform their own queries through the same pool.
func (b *Bridge) DB() *gorm.DB { return b.db }

// WebhookDispatcher returns a shared, lazily-constructed dispatcher that
// signs outbound webhook calls with the per-installation secret when one is
// available. When the bridge has no SecretResolver the dispatcher still
// works but emits unsigned requests.
func (b *Bridge) WebhookDispatcher() *security.WebhookDispatcher {
	if b.webhookDispatcher == nil {
		lookup := webhookSignerLookup(b.secretResolver)
		b.webhookDispatcher = security.NewWebhookDispatcher(lookup)
	}
	return b.webhookDispatcher
}

// Enforcer returns the shared capability enforcer. Hosts decide elsewhere
// (env var, config flag) whether to flip from shadow to enforce mode by
// calling enforcer.SetEnforce on the returned value.
func (b *Bridge) Enforcer() *security.Enforcer {
	if b.enforcer == nil {
		b.enforcer = security.NewEnforcer(func(addonKey string) *security.Capabilities {
			lc, ok := b.host.Lifecycles.Get(addonKey)
			if !ok {
				return nil
			}
			m := lc.Manifest()
			return security.Compile(addonKey, m.Capabilities)
		})
	}
	return b.enforcer
}

// KernelVersion returns the version the bridge advertises.
func (b *Bridge) KernelVersion() string { return KernelVersion }

// SetFrontendBasePath configures the on-disk root under which the installer
// materializes federation artifacts. Must be called before Install() so
// freshly installed addons land where the static-serving route expects them.
func (b *Bridge) SetFrontendBasePath(dir string) {
	if b.host != nil && b.host.Installer != nil {
		b.host.Installer.FrontendBasePath = dir
	}
}

// Navigation returns the merged sidebar for an organization. The host
// supplies its own core groups (typically a fixed array baked into the host
// binary); the kernel folds in addon contributions from every enabled
// installation.
func (b *Bridge) Navigation(orgID uuid.UUID, core []navigation.Group) ([]navigation.Group, error) {
	return b.host.Navigation(orgID, core)
}

// webhookSignerLookup resolves a *security.Signer for an installation using
// the bridge's SecretResolver. When no resolver is wired (or it returns no
// secret) the dispatcher sends unsigned — matching pre-bridge behaviour.
func webhookSignerLookup(resolver SecretResolver) func(uuid.UUID) (*security.Signer, error) {
	return func(id uuid.UUID) (*security.Signer, error) {
		if resolver == nil {
			return nil, nil
		}
		secret, err := resolver(id)
		if err != nil {
			return nil, err
		}
		if len(secret) == 0 {
			return nil, nil
		}
		return security.NewSigner(secret), nil
	}
}

// Manifest is re-exported for convenience so consumers don't need a second
// import just for the type.
type Manifest = manifest.Manifest
