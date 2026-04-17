package dynamic

import (
	"context"
	"sync"

	"github.com/asteby/metacore-kernel/modelbase"
	"gorm.io/gorm"
)

// HookContext carries metadata about the operation being performed.
type HookContext struct {
	Model string
	User  modelbase.AuthUser
	DB    *gorm.DB
}

// Hook signatures — each is optional per model.
type BeforeCreateHook func(ctx context.Context, hc HookContext, input map[string]any) error
type AfterCreateHook func(ctx context.Context, hc HookContext, record any) error
type BeforeUpdateHook func(ctx context.Context, hc HookContext, id string, input map[string]any) error
type AfterUpdateHook func(ctx context.Context, hc HookContext, record any) error
type BeforeDeleteHook func(ctx context.Context, hc HookContext, id string) error
type AfterDeleteHook func(ctx context.Context, hc HookContext, id string) error

// HookRegistry holds lifecycle hooks per model.
type HookRegistry struct {
	mu            sync.RWMutex
	beforeCreate  map[string][]BeforeCreateHook
	afterCreate   map[string][]AfterCreateHook
	beforeUpdate  map[string][]BeforeUpdateHook
	afterUpdate   map[string][]AfterUpdateHook
	beforeDelete  map[string][]BeforeDeleteHook
	afterDelete   map[string][]AfterDeleteHook
}

func NewHookRegistry() *HookRegistry {
	return &HookRegistry{
		beforeCreate: make(map[string][]BeforeCreateHook),
		afterCreate:  make(map[string][]AfterCreateHook),
		beforeUpdate: make(map[string][]BeforeUpdateHook),
		afterUpdate:  make(map[string][]AfterUpdateHook),
		beforeDelete: make(map[string][]BeforeDeleteHook),
		afterDelete:  make(map[string][]AfterDeleteHook),
	}
}

func (r *HookRegistry) RegisterBeforeCreate(model string, h BeforeCreateHook) {
	r.mu.Lock(); defer r.mu.Unlock()
	r.beforeCreate[model] = append(r.beforeCreate[model], h)
}

func (r *HookRegistry) RegisterAfterCreate(model string, h AfterCreateHook) {
	r.mu.Lock(); defer r.mu.Unlock()
	r.afterCreate[model] = append(r.afterCreate[model], h)
}

func (r *HookRegistry) RegisterBeforeUpdate(model string, h BeforeUpdateHook) {
	r.mu.Lock(); defer r.mu.Unlock()
	r.beforeUpdate[model] = append(r.beforeUpdate[model], h)
}

func (r *HookRegistry) RegisterAfterUpdate(model string, h AfterUpdateHook) {
	r.mu.Lock(); defer r.mu.Unlock()
	r.afterUpdate[model] = append(r.afterUpdate[model], h)
}

func (r *HookRegistry) RegisterBeforeDelete(model string, h BeforeDeleteHook) {
	r.mu.Lock(); defer r.mu.Unlock()
	r.beforeDelete[model] = append(r.beforeDelete[model], h)
}

func (r *HookRegistry) RegisterAfterDelete(model string, h AfterDeleteHook) {
	r.mu.Lock(); defer r.mu.Unlock()
	r.afterDelete[model] = append(r.afterDelete[model], h)
}

func (r *HookRegistry) runBeforeCreate(ctx context.Context, hc HookContext, input map[string]any) error {
	if r == nil { return nil }
	r.mu.RLock(); hooks := r.beforeCreate[hc.Model]; r.mu.RUnlock()
	for _, h := range hooks { if err := h(ctx, hc, input); err != nil { return err } }
	return nil
}

func (r *HookRegistry) runAfterCreate(ctx context.Context, hc HookContext, record any) error {
	if r == nil { return nil }
	r.mu.RLock(); hooks := r.afterCreate[hc.Model]; r.mu.RUnlock()
	for _, h := range hooks { if err := h(ctx, hc, record); err != nil { return err } }
	return nil
}

func (r *HookRegistry) runBeforeUpdate(ctx context.Context, hc HookContext, id string, input map[string]any) error {
	if r == nil { return nil }
	r.mu.RLock(); hooks := r.beforeUpdate[hc.Model]; r.mu.RUnlock()
	for _, h := range hooks { if err := h(ctx, hc, id, input); err != nil { return err } }
	return nil
}

func (r *HookRegistry) runAfterUpdate(ctx context.Context, hc HookContext, record any) error {
	if r == nil { return nil }
	r.mu.RLock(); hooks := r.afterUpdate[hc.Model]; r.mu.RUnlock()
	for _, h := range hooks { if err := h(ctx, hc, record); err != nil { return err } }
	return nil
}

func (r *HookRegistry) runBeforeDelete(ctx context.Context, hc HookContext, id string) error {
	if r == nil { return nil }
	r.mu.RLock(); hooks := r.beforeDelete[hc.Model]; r.mu.RUnlock()
	for _, h := range hooks { if err := h(ctx, hc, id); err != nil { return err } }
	return nil
}

func (r *HookRegistry) runAfterDelete(ctx context.Context, hc HookContext, id string) error {
	if r == nil { return nil }
	r.mu.RLock(); hooks := r.afterDelete[hc.Model]; r.mu.RUnlock()
	for _, h := range hooks { if err := h(ctx, hc, id); err != nil { return err } }
	return nil
}
