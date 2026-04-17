package modelbase

import "sync"

// ModelDefiner is the minimal contract a model must satisfy to be registered
// in the kernel's model registry. Apps SHOULD implement it on the pointer
// receiver of each persisted type.
//
// Note: DefineAnalytics/DefineSearch/DefineOptions intentionally live outside
// this interface so the contract stays small; richer contracts belong in
// higher-level modules (e.g. metadata) that can compose on top of this one.
type ModelDefiner interface {
	TableName() string
	DefineTable() TableMetadata
	DefineModal() ModalMetadata
}

var (
	registryMu sync.RWMutex
	registry   = map[string]func() ModelDefiner{}
)

// Register associates a factory with a key (usually the table name). The
// factory MUST return a fresh zero-valued instance on every call so callers
// can mutate the returned value without affecting other callers.
//
// Registering twice under the same key silently replaces the previous factory,
// which lets addons hot-override core models during development.
func Register(key string, factory func() ModelDefiner) {
	if key == "" || factory == nil {
		return
	}
	registryMu.Lock()
	defer registryMu.Unlock()
	registry[key] = factory
}

// Get returns a fresh instance for key by invoking its registered factory.
func Get(key string) (ModelDefiner, bool) {
	registryMu.RLock()
	factory, ok := registry[key]
	registryMu.RUnlock()
	if !ok {
		return nil, false
	}
	return factory(), true
}

// All returns a map of key -> fresh instance. Safe to iterate without holding
// the registry lock; each call allocates new instances.
func All() map[string]ModelDefiner {
	registryMu.RLock()
	defer registryMu.RUnlock()
	out := make(map[string]ModelDefiner, len(registry))
	for k, f := range registry {
		out[k] = f()
	}
	return out
}

// Keys returns a snapshot of registered keys. Useful for migration tooling
// that wants to iterate without instantiating every model.
func Keys() []string {
	registryMu.RLock()
	defer registryMu.RUnlock()
	out := make([]string, 0, len(registry))
	for k := range registry {
		out = append(out, k)
	}
	return out
}
