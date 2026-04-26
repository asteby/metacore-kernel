package lifecycle

import (
	"context"
	"sync"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// ActionContext is the runtime context passed to action interceptors.
// Kernel consumers embed their concrete types via the Extras map so the
// kernel stays thin.
type ActionContext struct {
	Ctx       context.Context
	DB        *gorm.DB
	OrgID     uuid.UUID
	UserID    uuid.UUID
	Model     string
	Action    string
	Installed *Installation
	Extras    map[string]any
}

// Installation is a lightweight view of an addon installation. The full model
// (with settings) lives in the consuming app.
type Installation struct {
	AddonKey       string
	OrganizationID uuid.UUID
	Version        string
	Settings       map[string]any
}

// ActionInterceptor runs after a model action completes successfully.
// If result is non-nil it overrides the default response.
type ActionInterceptor func(ctx *ActionContext, recordID any, payload map[string]any) (any, error)

// InterceptorRegistry maps "model::action" → interceptor.
type InterceptorRegistry struct {
	mu    sync.RWMutex
	items map[string]interceptorEntry
}

type interceptorEntry struct {
	AddonKey    string
	Interceptor ActionInterceptor
}

// NewInterceptorRegistry creates an empty registry.
func NewInterceptorRegistry() *InterceptorRegistry {
	return &InterceptorRegistry{items: make(map[string]interceptorEntry)}
}

// Register attaches an interceptor to a model+action pair.
func (r *InterceptorRegistry) Register(addonKey, model, action string, fn ActionInterceptor) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.items[model+"::"+action] = interceptorEntry{AddonKey: addonKey, Interceptor: fn}
}

// Get returns the interceptor for a model+action if present.
func (r *InterceptorRegistry) Get(model, action string) (ActionInterceptor, string, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	e, ok := r.items[model+"::"+action]
	if !ok {
		return nil, "", false
	}
	return e.Interceptor, e.AddonKey, true
}

// UnregisterAddon removes all interceptors owned by an addon key.
func (r *InterceptorRegistry) UnregisterAddon(addonKey string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for k, e := range r.items {
		if e.AddonKey == addonKey {
			delete(r.items, k)
		}
	}
}
