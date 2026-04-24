package tool

import (
	"fmt"
	"sort"
	"sync"
)

// Registry holds Tools keyed by "{addon_key}#{tool_id}". Safe for concurrent
// use. Hosts populate it on install (typically via installer.RegisterTools)
// and drain it on uninstall.
type Registry struct {
	mu    sync.RWMutex
	tools map[string]Tool
}

// NewRegistry returns an empty Registry.
func NewRegistry() *Registry {
	return &Registry{tools: map[string]Tool{}}
}

// key composes the internal map key for a (addon, tool) pair.
func key(addonKey, toolID string) string {
	return addonKey + "#" + toolID
}

// Register inserts t. Returns an error if a tool with the same addon+id is
// already registered — uninstall the prior one first or call Replace if you
// intentionally want overwrite semantics.
func (r *Registry) Register(t Tool) error {
	if t == nil {
		return fmt.Errorf("tool.Registry: nil tool")
	}
	k := key(t.AddonKey(), t.ID())
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.tools[k]; exists {
		return fmt.Errorf("tool.Registry: %q already registered", k)
	}
	r.tools[k] = t
	return nil
}

// Replace is Register without the duplicate check. Useful for hot-reloading
// during development or upgrading an addon version in place.
func (r *Registry) Replace(t Tool) {
	if t == nil {
		return
	}
	r.mu.Lock()
	r.tools[key(t.AddonKey(), t.ID())] = t
	r.mu.Unlock()
}

// Unregister removes a tool. Missing entries are a no-op.
func (r *Registry) Unregister(addonKey, toolID string) {
	r.mu.Lock()
	delete(r.tools, key(addonKey, toolID))
	r.mu.Unlock()
}

// UnregisterAddon removes every tool contributed by addonKey. Returns the
// number of tools removed. Typical use: uninstall hook.
func (r *Registry) UnregisterAddon(addonKey string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	var removed int
	for k, t := range r.tools {
		if t.AddonKey() == addonKey {
			delete(r.tools, k)
			removed++
		}
	}
	return removed
}

// ByID returns the tool registered under (addonKey, toolID), or false.
func (r *Registry) ByID(addonKey, toolID string) (Tool, bool) {
	r.mu.RLock()
	t, ok := r.tools[key(addonKey, toolID)]
	r.mu.RUnlock()
	return t, ok
}

// ByAddon returns every tool contributed by addonKey, sorted by tool ID for
// stable iteration.
func (r *Registry) ByAddon(addonKey string) []Tool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []Tool
	for _, t := range r.tools {
		if t.AddonKey() == addonKey {
			out = append(out, t)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID() < out[j].ID() })
	return out
}

// All returns every registered tool, sorted by addon key then tool ID.
func (r *Registry) All() []Tool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Tool, 0, len(r.tools))
	for _, t := range r.tools {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].AddonKey() != out[j].AddonKey() {
			return out[i].AddonKey() < out[j].AddonKey()
		}
		return out[i].ID() < out[j].ID()
	})
	return out
}

// Len returns the number of registered tools — useful for metrics.
func (r *Registry) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.tools)
}
