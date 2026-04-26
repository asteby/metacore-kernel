// Package bridge is the host-side glue that promotes a manifest into the
// concrete features a host app exposes:
//
//   - Lifecycle wiring against host.Host (manifests, navigation, install).
//   - Action interceptors (UI-triggered hooks → signed webhook calls).
//   - LLM tools projection (manifest.Tools → host's agent-tool table).
//   - Process-global tool.Registry hydration for /tools/execute.
//
// The kernel does NOT import host model packages. Instead, hosts wire
// concrete implementations of the interfaces below. ports.go is the contract;
// bridge.go and friends consume it. *gorm.DB freely crosses the boundary
// because other kernel packages (installer, dynamic, navigation) already
// depend on it.
package bridge

import (
	"github.com/asteby/metacore-kernel/manifest"
	"github.com/google/uuid"
)

// ActionContext is the kernel-owned struct passed to ActionInterceptor calls.
// It mirrors what UI hosts populate for their action runner without pulling
// host-specific model packages into the kernel. Hosts may carry a richer
// context internally and shim into this struct at the boundary.
type ActionContext struct {
	OrgID     uuid.UUID
	UserID    uuid.UUID
	Locale    string
	RequestID string
}

// ActionInterceptor is the function signature the bridge registers for each
// (model, action) pair declared in a manifest. The host's UI runtime calls
// the interceptor after persisting the row so the addon can react. Returning
// (nil, nil) means "no override — proceed with default response".
type ActionInterceptor func(ctx *ActionContext, recordID interface{}, payload map[string]interface{}) (interface{}, error)

// ActionInterceptorRegistry is the host-owned registry of (addonKey, model,
// action) → ActionInterceptor entries. The bridge calls Register on install
// and Unregister on uninstall. Implementations must be safe for concurrent
// use.
type ActionInterceptorRegistry interface {
	Register(addonKey, model, action string, fn ActionInterceptor)
	Unregister(addonKey, model, action string)
}

// Tool is the host-side projection of a manifest.ToolDef into the host's
// agent-tool storage layer. Each host has its own row shape (e.g. some hosts
// key by MarketplaceSlug, others use a header-marker convention), so the
// kernel only knows the addon-keyed identity.
type Tool struct {
	OrgID    uuid.UUID
	AddonKey string
	Def      manifest.ToolDef
}

// ToolStore abstracts the host's agent-tool storage. The bridge calls:
//
//   - LoadByAddon to read existing rows when re-syncing a manifest.
//   - Upsert to create/update rows for the manifest's declared tools.
//   - DeleteByAddon to drain rows on uninstall.
//
// Implementations decide how to materialize a Tool — including resolving the
// host-specific AgentID (e.g. binding to the org's default agent).
type ToolStore interface {
	LoadByAddon(orgID uuid.UUID, addonKey string) ([]Tool, error)
	Upsert(tools []Tool) error
	DeleteByAddon(orgID uuid.UUID, addonKey string) error
}

// AgentResolver returns the default agent UUID for an org. Hosts that bind
// agent_tools to a parent agent implement this; hosts without agents can
// return uuid.Nil.
type AgentResolver interface {
	DefaultAgent(orgID uuid.UUID) (uuid.UUID, error)
}

// LegacyLifecycle is the minimum surface the bridge needs to mirror
// compiled-in (pre-bundle) addons into the kernel's lifecycle registry.
// Hosts that ship a parallel "legacy" addon system implement this; hosts
// that only consume kernel-native addons can return an empty map.
type LegacyLifecycle interface {
	Manifest() manifest.Manifest
}

// LegacyManifestSource exposes every compiled-in addon manifest the host
// already has registered outside the kernel. The bridge mirrors them into
// the kernel's lifecycle registry on construction so navigation and
// /manifests answer consistently regardless of which side an addon was
// registered on.
type LegacyManifestSource interface {
	All() map[string]LegacyLifecycle
}

// SecretResolver returns the cleartext install-time secret for an
// installation. Returning (nil, nil) is allowed and signals "this install
// was never enrolled with HMAC" — the dispatcher then falls through to
// unsigned mode. The bridge wires this into kernel/security.WebhookDispatcher.
type SecretResolver func(installationID uuid.UUID) ([]byte, error)
