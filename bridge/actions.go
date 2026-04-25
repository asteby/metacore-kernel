// actions.go projects a metacore addon's declared Actions into the host's
// native action-interceptor registry so they show up as buttons / modals in
// the host UI automatically on install, and are cleaned up on uninstall.
//
// This is the host-agnostic mirror of what link's tools_bridge does for
// LLM-facing tools: each host consumes the plane of the manifest it
// understands (link → Tools, ops → Actions). The kernel stays neutral and
// only delegates to whichever ActionInterceptorRegistry the host wires.
package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/asteby/metacore-kernel/manifest"
	"github.com/asteby/metacore-kernel/security"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// ActionsBridge wires manifest.Actions into a host ActionInterceptorRegistry
// and tracks what each addon registered so uninstall can reverse the
// registration precisely.
type ActionsBridge struct {
	db         *gorm.DB
	registry   ActionInterceptorRegistry
	dispatcher *security.WebhookDispatcher

	mu         sync.Mutex
	registered map[string][]string // addonKey → ["model::action", ...]
}

// NewActionsBridge builds the bridge. registry is the host's
// ActionInterceptorRegistry (ops wraps its package-level functions in a
// thin adapter; link can supply a no-op if it has no UI actions).
// dispatcher is used to send HMAC-signed webhook calls when an action's
// hook targets a remote addon backend.
func NewActionsBridge(db *gorm.DB, registry ActionInterceptorRegistry, disp *security.WebhookDispatcher) *ActionsBridge {
	return &ActionsBridge{
		db:         db,
		registry:   registry,
		dispatcher: disp,
		registered: map[string][]string{},
	}
}

// SyncAddonActions registers one action interceptor per manifest.Action for
// the addon's declared hook URL. Idempotent — calling twice leaves the
// registry in the same shape.
//
// The webhook URL is resolved from `manifest.hooks["<model>::<action>"]`. If
// no explicit hook is declared the action is still registered but its
// interceptor is a no-op (the host's UI still shows the button; it just
// becomes a host-local action with no remote callout).
func (b *ActionsBridge) SyncAddonActions(m manifest.Manifest) error {
	if b.registry == nil {
		return fmt.Errorf("bridge.SyncAddonActions: nil ActionInterceptorRegistry")
	}
	// Uninstall previous registration for this addon so we don't leak stale
	// interceptors when a new manifest version drops an action.
	b.removeAddon(m.Key)

	keys := make([]string, 0, 8)
	for modelName, actions := range m.Actions {
		if strings.TrimSpace(modelName) == "" {
			return fmt.Errorf("manifest.actions: empty model name")
		}
		for _, a := range actions {
			if a.Key == "" {
				return fmt.Errorf("manifest.actions[%s]: action key required", modelName)
			}
			hookKey := modelName + "::" + a.Key
			webhookURL := ""
			if m.Hooks != nil {
				webhookURL = m.Hooks[hookKey]
			}
			interceptor := b.buildInterceptor(m.Key, hookKey, webhookURL)
			b.registry.Register(m.Key, modelName, a.Key, interceptor)
			keys = append(keys, hookKey)
		}
	}
	b.mu.Lock()
	b.registered[m.Key] = keys
	b.mu.Unlock()
	return nil
}

// RemoveAddonActions tears down every interceptor this bridge registered
// for the addon. Other sources (compiled addons, host-native actions) are
// untouched.
func (b *ActionsBridge) RemoveAddonActions(addonKey string) {
	b.removeAddon(addonKey)
}

// RegisteredKeys returns the current "model::action" registrations for an
// addon — useful for diagnostics and tests.
func (b *ActionsBridge) RegisteredKeys(addonKey string) []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	keys := b.registered[addonKey]
	out := make([]string, len(keys))
	copy(out, keys)
	return out
}

func (b *ActionsBridge) removeAddon(addonKey string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, hookKey := range b.registered[addonKey] {
		parts := strings.SplitN(hookKey, "::", 2)
		if len(parts) != 2 {
			continue
		}
		if b.registry != nil {
			b.registry.Unregister(addonKey, parts[0], parts[1])
		}
	}
	delete(b.registered, addonKey)
}

// buildInterceptor returns the interceptor body that runs for each declared
// action. When a webhook URL is present it delegates to the signed
// dispatcher; otherwise it is a deliberate no-op so the UI still sees the
// button.
func (b *ActionsBridge) buildInterceptor(addonKey, hookKey, webhookURL string) ActionInterceptor {
	if webhookURL == "" || b.dispatcher == nil {
		return func(*ActionContext, interface{}, map[string]interface{}) (interface{}, error) {
			return nil, nil
		}
	}
	return signedActionInterceptor(b.db, b.dispatcher, addonKey, hookKey, webhookURL)
}

// signedActionInterceptor builds an ActionInterceptor that POSTs the action
// payload to the addon's webhook URL with the HMAC signing + host-context
// headers the remote addon expects.
//
// The (org, addon) → installation lookup is done with a raw SQL query
// against metacore_installations so we don't depend on the host's model
// package; the kernel's installer.Installation table name is fixed.
func signedActionInterceptor(db *gorm.DB, disp *security.WebhookDispatcher, addonKey, hookKey, webhookURL string) ActionInterceptor {
	return func(ctx *ActionContext, recordID interface{}, payload map[string]interface{}) (interface{}, error) {
		if db == nil {
			return nil, fmt.Errorf("bridge: signed action interceptor needs DB")
		}
		var inst struct {
			ID uuid.UUID `gorm:"column:id"`
		}
		err := db.Raw(
			`SELECT id FROM metacore_installations
			 WHERE organization_id = ? AND addon_key = ? AND status = 'enabled'
			 LIMIT 1`, ctx.OrgID, addonKey).Scan(&inst).Error
		if err != nil || inst.ID == uuid.Nil {
			return nil, fmt.Errorf("metacore: no enabled installation for %s", addonKey)
		}
		body, err := marshalActionBody(recordID, payload, hookKey, ctx)
		if err != nil {
			return nil, err
		}
		callCtx := security.WithHostContext(context.Background(), security.HostContext{
			Host:   "metacore",
			Tenant: ctx.OrgID.String(),
			Extras: map[string]string{
				"X-Metacore-Invocation": "action",
				"X-Metacore-User-ID":    ctx.UserID.String(),
			},
		})
		callCtx = security.WithEvent(callCtx, hookKey)

		status, respBody, err := disp.DispatchAndRead(callCtx, inst.ID, webhookURL, "POST", body)
		if err != nil {
			return nil, err
		}
		if status >= 400 {
			return nil, fmt.Errorf("addon %s webhook returned %d: %s", addonKey, status, string(respBody))
		}
		return string(respBody), nil
	}
}

// marshalActionBody builds the JSON the addon backend receives. Keys match
// what link sends for tools so a single addon can treat action + tool
// uniformly.
func marshalActionBody(recordID interface{}, payload map[string]interface{}, hookKey string, ctx *ActionContext) ([]byte, error) {
	wrapped := map[string]interface{}{
		"record_id": recordID,
		"payload":   payload,
		"hook":      hookKey,
		"org_id":    ctx.OrgID,
	}
	return json.Marshal(wrapped)
}
