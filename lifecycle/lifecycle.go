// Package lifecycle defines the runtime interfaces an addon implements and the
// registry that tracks them. Addons can be:
//
//   - Compiled: Go code linked into the host (fastest; highest trust).
//   - Declarative: manifest-only; behavior wired via webhooks & interceptors.
//   - Federated: remote UI + webhook backend (sandboxed; lowest trust).
package lifecycle

import (
	"sync"

	"github.com/asteby/metacore-sdk/pkg/manifest"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// Addon is the minimum contract a lifecycle implementation must satisfy.
type Addon interface {
	Manifest() manifest.Manifest
	OnInstall(db *gorm.DB, orgID uuid.UUID) error
	OnUninstall(db *gorm.DB, orgID uuid.UUID) error
	OnEnable(db *gorm.DB, orgID uuid.UUID) error
	OnDisable(db *gorm.DB, orgID uuid.UUID) error
}

// Bootstrapper is implemented by addons that need runtime services after
// startup (e.g. to register action interceptors or event subscribers).
type Bootstrapper interface {
	Boot(ctx *BootContext) error
}

// BootContext carries runtime services into addons during the boot phase.
type BootContext struct {
	DB       *gorm.DB
	Services map[string]any
}

// GetService returns a named service from the boot context.
func (bc *BootContext) GetService(name string) (any, bool) {
	s, ok := bc.Services[name]
	return s, ok
}

// ManifestOnly wraps a manifest for declarative addons (no compiled logic).
type ManifestOnly struct {
	Data manifest.Manifest
}

func (a *ManifestOnly) Manifest() manifest.Manifest                   { return a.Data }
func (a *ManifestOnly) OnInstall(_ *gorm.DB, _ uuid.UUID) error       { return nil }
func (a *ManifestOnly) OnUninstall(_ *gorm.DB, _ uuid.UUID) error     { return nil }
func (a *ManifestOnly) OnEnable(_ *gorm.DB, _ uuid.UUID) error        { return nil }
func (a *ManifestOnly) OnDisable(_ *gorm.DB, _ uuid.UUID) error       { return nil }

// Registry is a thread-safe map of addon_key → Addon.
type Registry struct {
	mu    sync.RWMutex
	items map[string]Addon
}

// NewRegistry creates an empty registry.
func NewRegistry() *Registry {
	return &Registry{items: make(map[string]Addon)}
}

// Register associates an addon key with its lifecycle implementation.
func (r *Registry) Register(key string, a Addon) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.items[key] = a
}

// Get returns the lifecycle for a key.
func (r *Registry) Get(key string) (Addon, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	a, ok := r.items[key]
	return a, ok
}

// All returns a shallow copy so callers can iterate without holding the lock.
func (r *Registry) All() map[string]Addon {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make(map[string]Addon, len(r.items))
	for k, v := range r.items {
		out[k] = v
	}
	return out
}

// Unregister removes an addon from the registry.
func (r *Registry) Unregister(key string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.items, key)
}
