// Package manifest defines the declarative contract an addon ships.
// This is the single source of truth — consumed by the kernel (Go) and
// mirrored by the SDK (TS) via generated types.
package manifest

// APIVersion is the kernel contract version this package implements.
// Addons declare `kernel: ">=X.Y <Z"` to opt into a compatibility window.
const APIVersion = "2.0.0"

// Manifest describes everything an addon provides: metadata, extension points,
// data model, navigation, permissions and distribution info.
type Manifest struct {
	Key         string `json:"key"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Version     string `json:"version"`
	Category    string `json:"category"`
	// Icon legacy-compat single string (PascalCase lucide or simple-icons
	// slug). Prefer the explicit triplet below for richer rendering.
	Icon string `json:"icon,omitempty"`
	// IconType: "brand" (simple-icons slug), "lucide" (PascalCase icon
	// name) or "url" (absolute URL). Mirrors link/MarketplaceIntegration so
	// the hub frontend consumes them identically.
	IconType  string `json:"icon_type,omitempty"`
	IconSlug  string `json:"icon_slug,omitempty"`
	IconColor string `json:"icon_color,omitempty"`

	// Kernel is a semver range the host kernel must satisfy. Empty = legacy.
	Kernel   string   `json:"kernel,omitempty"`
	Requires []Module `json:"requires,omitempty"`
	Models   []Module `json:"models,omitempty"`

	// TenantIsolation declares how the addon's data is isolated across orgs.
	//
	//   "shared"            — single schema addon_<key>, organization_id column
	//                         + Postgres RLS. The default for most addons.
	//   "schema-per-tenant" — one schema per installation
	//                         (addon_<key>_<orgshort>) created at install and
	//                         dropped on uninstall. Use for regulated data.
	//   "database-per-tenant" — reserved for future use.
	//
	// Empty value is treated as "shared" for backwards compatibility.
	TenantIsolation string `json:"tenant_isolation,omitempty"`

	Navigation       []NavGroup             `json:"navigation,omitempty"`
	Extensions       []ModelExtension       `json:"extensions,omitempty"`
	Settings         []SettingDef           `json:"settings,omitempty"`
	Hooks            map[string]string      `json:"hooks,omitempty"`
	Actions          map[string][]ActionDef `json:"actions,omitempty"`
	// Tools are LLM-facing actions. Conversational hosts (link) sync these
	// into their agent-tool registry on install so an AI can trigger them
	// from a user message. Unlike Actions (UI-triggered record ops) Tools
	// are semantic and carry extraction hints for parameter inference.
	Tools            []ToolDef              `json:"tools,omitempty"`
	ModelDefinitions []ModelDefinition      `json:"model_definitions,omitempty"`
	Events           []string               `json:"events,omitempty"`
	LifecycleHooks   map[string][]HookDef   `json:"lifecycle_hooks,omitempty"`
	I18n             map[string]map[string]string `json:"i18n,omitempty"`

	// Frontend describes the federated UI bundle.
	Frontend *FrontendSpec `json:"frontend,omitempty"`

	// Backend selects the execution model for addon-authored code. When nil
	// the legacy "webhook" behaviour applies (Hooks map dispatches HTTP
	// calls). Set Runtime to "wasm" to run a compiled module in-process.
	Backend *BackendSpec `json:"backend,omitempty"`

	// Capabilities are the scoped permissions the addon requests. The host
	// prompts the admin for approval and the runtime enforces them.
	Capabilities []Capability `json:"capabilities,omitempty"`

	// Permissions is legacy declarative model/scope access. Prefer Capabilities.
	Permissions []Permission `json:"permissions,omitempty"`

	Signature *Signature `json:"signature,omitempty"`

	Author      string   `json:"author,omitempty"`
	Website     string   `json:"website,omitempty"`
	License     string   `json:"license,omitempty"`
	Readme      string   `json:"readme,omitempty"`
	Screenshots []string `json:"screenshots,omitempty"`
	Features    []string `json:"features,omitempty"`
	Price       string   `json:"price,omitempty"`
}

// Module is a named reference with a translatable label.
type Module struct {
	Key   string `json:"key"`
	Label string `json:"label"`
}

// NavGroup is a sidebar group contributed by the addon. Target merges into an
// existing core group (e.g. "sidebar.sales"); empty groups live under "Addons".
type NavGroup struct {
	Title  string    `json:"title"`
	Icon   string    `json:"icon"`
	Target string    `json:"target,omitempty"`
	Items  []NavItem `json:"items"`
}

// NavItem is a single sidebar entry. Model, if set, binds to a dynamic CRUD page.
type NavItem struct {
	Title      string    `json:"title"`
	URL        string    `json:"url,omitempty"`
	Icon       string    `json:"icon,omitempty"`
	Model      string    `json:"model,omitempty"`
	Permission string    `json:"permission,omitempty"`
	Items      []NavItem `json:"items,omitempty"`
}

// FrontendSpec describes the federated module the host loads at runtime.
type FrontendSpec struct {
	// Entry is the URL (or relative path) of the remoteEntry.js / bundle.
	Entry string `json:"entry"`
	// Format: "federation" | "script" (legacy window.__addon registration).
	Format string `json:"format"`
	// Expose is the federation module name to import (e.g. "./plugin").
	Expose string `json:"expose,omitempty"`
	// Integrity SRI hash, optional but recommended.
	Integrity string `json:"integrity,omitempty"`
	// Container is the global name the remoteEntry.js assigns itself to on
	// `window`. When empty the SDK derives it deterministically from the
	// manifest key as `metacore_<key>`. The build-time `name` option of
	// `@originjs/vite-plugin-federation` MUST match this value for the host
	// to locate the container after injecting the script tag.
	Container string `json:"container,omitempty"`
}

// BackendSpec declares how the addon's backend code is executed.
//
//	Runtime = "webhook" — legacy remote HTTP dispatch (no Entry needed).
//	Runtime = "wasm"    — sandboxed in-process module at Entry. Exports
//	                      enumerates the function symbols each hook can
//	                      dispatch to; kernel/runtime/wasm enforces it.
//	Runtime = "binary"  — reserved for future use (native side-car).
type BackendSpec struct {
	Runtime       string   `json:"runtime"`                   // "webhook" | "wasm" | "binary"
	Entry         string   `json:"entry,omitempty"`           // e.g. "backend/backend.wasm"
	URL           string   `json:"url,omitempty"`             // webhook base URL (alt to hooks map)
	Exports       []string `json:"exports,omitempty"`         // function names the module exports
	MemoryLimitMB int      `json:"memory_limit_mb,omitempty"` // default 64
	TimeoutMs     int      `json:"timeout_ms,omitempty"`      // default 10000
}

// Capability is a scoped permission request. The runtime enforces these by
// injecting an AddonContext with only the declared access.
//
// Examples:
//   { "kind": "db:read",   "target": "orders"                  }
//   { "kind": "db:write",  "target": "addon_tickets.*"         }
//   { "kind": "http:fetch","target": "https://api.stripe.com/*"}
//   { "kind": "event:emit","target": "sale.created"            }
//   { "kind": "event:subscribe", "target": "invoice.stamped"   }
type Capability struct {
	Kind   string `json:"kind"`
	Target string `json:"target"`
	Reason string `json:"reason,omitempty"`
}

// Permission is the legacy model/scope declaration.
type Permission struct {
	Model  string `json:"model"`
	Scope  string `json:"scope"`
	Reason string `json:"reason,omitempty"`
}

// SettingDef is a per-installation configurable value.
type SettingDef struct {
	Key          string      `json:"key"`
	Label        string      `json:"label"`
	Type         string      `json:"type"`
	DefaultValue interface{} `json:"default_value,omitempty"`
	Options      []Option    `json:"options,omitempty"`
	Secret       bool        `json:"secret,omitempty"`
}

// Option is a select-field choice.
type Option struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

// ToolDef is an LLM-callable function the addon exposes. Hosts with
// conversational AI (link) register these into their agent-tool registry
// so a user message can trigger them. The endpoint receives an HMAC-signed
// webhook produced by kernel/security.WebhookDispatcher.
type ToolDef struct {
	ID              string           `json:"id"`                         // unique within the addon
	Name            string           `json:"name"`
	Description     string           `json:"description"`                // shown to the LLM — be specific
	Category        string           `json:"category,omitempty"`         // communication | query | action | integration
	InputSchema     []ToolInputParam `json:"input_schema,omitempty"`
	TriggerKeywords []string         `json:"trigger_keywords,omitempty"`
	TriggerIntents  []string         `json:"trigger_intents,omitempty"`
	Endpoint        string           `json:"endpoint"`                   // relative or absolute URL
	Method          string           `json:"method,omitempty"`           // defaults to POST
	AutoCreateRecord string          `json:"auto_create_record,omitempty"` // host-specific record type
	Settings        map[string]any   `json:"settings,omitempty"`
	Timeout         int              `json:"timeout,omitempty"`          // seconds
	CacheTTL        int              `json:"cache_ttl,omitempty"`
	Priority        int              `json:"priority,omitempty"`
}

// ToolInputParam describes a single LLM-extractable argument for a tool.
type ToolInputParam struct {
	Name           string `json:"name"`
	Type           string `json:"type"` // string | number | date | boolean | email | phone
	Description    string `json:"description"`
	Required       bool   `json:"required,omitempty"`
	Example        string `json:"example,omitempty"`
	ExtractionHint string `json:"extraction_hint,omitempty"`
	DefaultValue   string `json:"default_value,omitempty"`
	Validation     string `json:"validation,omitempty"`
	Normalize      string `json:"normalize,omitempty"`
	FormatPattern  string `json:"format_pattern,omitempty"`
}

// ActionDef is a declarative action the UI can invoke on a model row.
type ActionDef struct {
	Key            string      `json:"key"`
	Name           string      `json:"name"`
	Label          string      `json:"label"`
	Icon           string      `json:"icon,omitempty"`
	Fields         []FieldDef  `json:"fields,omitempty"`
	RequiresState  []string    `json:"requiresState,omitempty"`
	Confirm        bool        `json:"confirm,omitempty"`
	ConfirmMessage string      `json:"confirmMessage,omitempty"`
	Modal          string      `json:"modal,omitempty"` // slot name for a custom modal
}

// FieldDef is an input field used by action forms and model definitions.
type FieldDef struct {
	Name      string      `json:"name"`
	Label     string      `json:"label"`
	Type      string      `json:"type"`
	Required  bool        `json:"required,omitempty"`
	Default   interface{} `json:"default,omitempty"`
	Options   []Option    `json:"options,omitempty"`
	Size      int         `json:"size,omitempty"`
}

// HookDef is a CRUD lifecycle hook dispatch target.
type HookDef struct {
	Event    string     `json:"event"`
	Target   HookTarget `json:"target"`
	Priority int        `json:"priority,omitempty"`
	Async    bool       `json:"async,omitempty"`
}

// HookTarget describes where a lifecycle hook dispatches to.
type HookTarget struct {
	Type     string `json:"type"`
	URL      string `json:"url,omitempty"`
	Function string `json:"function,omitempty"`
	Prompt   string `json:"prompt,omitempty"`
}

// ModelExtension adds fields/actions/hooks to an existing core model.
type ModelExtension struct {
	Model   string      `json:"model"`
	Columns []ColumnDef `json:"columns,omitempty"`
	Actions []ActionDef `json:"actions,omitempty"`
}

// ModelDefinition declares a new table the addon installs. The host creates
// it in the addon's isolated schema (addon_{key}.{table}).
type ModelDefinition struct {
	TableName  string      `json:"table_name"`
	ModelKey   string      `json:"model_key"`
	Label      string      `json:"label"`
	OrgScoped  bool        `json:"org_scoped,omitempty"`
	SoftDelete bool        `json:"soft_delete,omitempty"`
	Columns    []ColumnDef `json:"columns"`
	Table      interface{} `json:"table,omitempty"` // UI table spec (opaque)
	Modal      interface{} `json:"modal,omitempty"` // UI modal spec (opaque)
}

// ColumnDef is a column on an addon-installed table.
type ColumnDef struct {
	Name     string `json:"name"`
	Type     string `json:"type"` // string, uuid, int, bigint, decimal, bool, timestamp, jsonb, text
	Size     int    `json:"size,omitempty"`
	Required bool   `json:"required,omitempty"`
	Index    bool   `json:"index,omitempty"`
	Unique   bool   `json:"unique,omitempty"`
	// Default accepts string ("'pending'", "now()"), number (42), or bool
	// literals from JSON. They are coerced to a DDL-safe string at install.
	Default  any    `json:"default,omitempty"`
	Ref      string `json:"ref,omitempty"` // foreign key target: "orders" or "addon_tickets.comments"
}

// Signature is the cryptographic provenance info stamped by the marketplace.
type Signature struct {
	DeveloperID   string            `json:"developer_id"`
	DeveloperName string            `json:"developer_name"`
	Verified      bool              `json:"verified"`
	SignedAt      string            `json:"signed_at"`
	Algorithm     string            `json:"algorithm"`
	Digest        string            `json:"digest"`
	Value         string            `json:"value"`
	Checksums     map[string]string `json:"checksums,omitempty"`
}

// ModuleKeys extracts only the keys.
func ModuleKeys(modules []Module) []string {
	keys := make([]string, len(modules))
	for i, m := range modules {
		keys[i] = m.Key
	}
	return keys
}
