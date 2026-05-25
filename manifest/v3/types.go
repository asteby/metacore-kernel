// Package v3 implements the Module Contract v3 manifest types and
// validator used by the metacore kernel installer.
//
// The package is intentionally decoupled from manifest (the v2 types) so
// the kernel can dual-read both versions during the 3.x release line. The
// authoritative grammar is the JSON schema in
// docs/spec/v3/manifest-v3.schema.json; the Go structs below mirror it
// 1:1 so json.Unmarshal followed by Validate is a faithful round trip.
package v3

// APIVersion is the only accepted value for the top-level apiVersion field
// in a v3 manifest. The kernel rejects anything else.
const APIVersion = "asteby.com/v3"

// Kind enumerates the top-level shape selector.
const (
	KindAddon         = "Addon"
	KindPreset        = "Preset"
	KindTheme         = "Theme"
	KindConnectorPack = "ConnectorPack"
)

// Manifest is the top-level v3 contract document.
type Manifest struct {
	APIVersion      string           `json:"apiVersion"`
	Kind            string           `json:"kind"`
	Metadata        Metadata         `json:"metadata"`
	Compatibility   Compatibility    `json:"compatibility"`
	Tenancy         *Tenancy         `json:"tenancy,omitempty"`
	Capabilities    []Capability     `json:"capabilities,omitempty"`
	Models          []Model          `json:"models,omitempty"`
	Frontend        *Frontend        `json:"frontend,omitempty"`
	Contributions   *Contributions   `json:"contributions,omitempty"`
	ExtensionPoints *ExtensionPoints `json:"extension_points,omitempty"`
	Lifecycle       *Lifecycle       `json:"lifecycle,omitempty"`
	I18n            *I18n            `json:"i18n,omitempty"`
	RBAC            *RBAC            `json:"rbac,omitempty"`
	Settings        []Setting        `json:"settings,omitempty"`
	Billing         *Billing         `json:"billing,omitempty"`
	Preset          *Preset          `json:"preset,omitempty"`
	Theme           *Theme           `json:"theme,omitempty"`
	ConnectorPack   *ConnectorPack   `json:"connector_pack,omitempty"`
	Signature       *Signature       `json:"signature,omitempty"`
}

// Frontend describes the federated UI bundle the host loads at runtime for
// this addon. It mirrors the legacy manifest.FrontendSpec so FromV3 maps it
// 1:1. Entry + Format identify the bundle; Container/Expose/Integrity/Layout
// tune how the host locates, verifies and frames it.
type Frontend struct {
	// Entry is the URL (or relative path) of the remoteEntry.js / bundle.
	Entry string `json:"entry"`
	// Format: "federation" | "script" (legacy window.__addon registration).
	Format string `json:"format"`
	// Expose is the federation module name to import (e.g. "./plugin").
	Expose string `json:"expose,omitempty"`
	// Integrity SRI hash, optional but recommended.
	Integrity string `json:"integrity,omitempty"`
	// Container is the global name the remoteEntry.js assigns itself on window.
	Container string `json:"container,omitempty"`
	// Layout selects how the host frames the addon UI ("shell" | "immersive").
	Layout string `json:"layout,omitempty"`
}

// Metadata is identity + presentation + authorship.
type Metadata struct {
	Key         string   `json:"key"`
	Name        string   `json:"name"`
	Description string   `json:"description,omitempty"`
	Version     string   `json:"version"`
	Category    string   `json:"category,omitempty"`
	Icon        *Icon    `json:"icon,omitempty"`
	Author      string   `json:"author,omitempty"`
	Website     string   `json:"website,omitempty"`
	License     string   `json:"license,omitempty"`
	Readme      string   `json:"readme,omitempty"`
	Screenshots []string `json:"screenshots,omitempty"`
	Features    []string `json:"features,omitempty"`
}

// Icon is the triple {type, slug, color} the host renders for the avatar.
type Icon struct {
	Type  string `json:"type"`
	Slug  string `json:"slug"`
	Color string `json:"color,omitempty"`
}

// Compatibility holds versioned dependency declarations.
type Compatibility struct {
	Requires []Requirement `json:"requires"`
}

// Requirement is a single peer dependency. Key "kernel" is reserved.
type Requirement struct {
	Key      string `json:"key"`
	Version  string `json:"version"`
	Optional bool   `json:"optional,omitempty"`
	Reason   string `json:"reason,omitempty"`
}

// Tenancy declares per-tenant data isolation strategy.
type Tenancy struct {
	Isolation string `json:"isolation,omitempty"`  // "shared" (default) | "schema" | "database"
	RLSColumn string `json:"rls_column,omitempty"` // default "organization_id"
}

// Capability is a single scoped permission grant.
type Capability struct {
	Kind   string `json:"kind"`
	Target string `json:"target"`
	Reason string `json:"reason,omitempty"`
}

// Model is a data model the addon owns.
type Model struct {
	Key         string           `json:"key"`
	Table       string           `json:"table"`
	Label       string           `json:"label,omitempty"`
	Columns     []Column         `json:"columns"`
	Indices     []Index          `json:"indices,omitempty"`
	ForeignKeys []ForeignKey     `json:"foreign_keys,omitempty"`
	Extensions  []ModelExtension `json:"extensions,omitempty"`
}

// Column is a single physical column declaration.
type Column struct {
	Name       string      `json:"name"`
	Type       string      `json:"type"`
	PrimaryKey bool        `json:"primary_key,omitempty"`
	NotNull    bool        `json:"not_null,omitempty"`
	Default    interface{} `json:"default,omitempty"`
	Label      string      `json:"label,omitempty"`
}

// Index is a single index declaration.
type Index struct {
	Name    string   `json:"name"`
	Columns []string `json:"columns"`
	Unique  bool     `json:"unique,omitempty"`
	Method  string   `json:"method,omitempty"`
}

// ForeignKey is a cross-model reference, materialised physically or logically.
type ForeignKey struct {
	Columns    []string  `json:"columns"`
	References Reference `json:"references"`
	Policy     string    `json:"policy"` // "logical" | "physical"
	OnDelete   string    `json:"on_delete,omitempty"`
}

// Reference is the target of a ForeignKey.
type Reference struct {
	Model   string   `json:"model"`
	Columns []string `json:"columns"`
}

// ModelExtension attaches columns to a model owned by another addon.
type ModelExtension struct {
	TargetModel string   `json:"target_model"`
	Columns     []Column `json:"columns"`
}

// Contributions is what this addon contributes to other modules' extension points.
type Contributions struct {
	Navigation    []NavGroup         `json:"navigation,omitempty"`
	Slots         []SlotContribution `json:"slots,omitempty"`
	Actions       []Action           `json:"actions,omitempty"`
	Tools         []Tool             `json:"tools,omitempty"`
	Subscriptions []Subscription     `json:"subscriptions,omitempty"`
}

// NavGroup is a sidebar group.
type NavGroup struct {
	Title  string    `json:"title"`
	Icon   string    `json:"icon,omitempty"`
	Target string    `json:"target,omitempty"`
	Items  []NavItem `json:"items"`
}

// NavItem is a single sidebar entry.
type NavItem struct {
	Title      string    `json:"title"`
	URL        string    `json:"url,omitempty"`
	Icon       string    `json:"icon,omitempty"`
	Model      string    `json:"model,omitempty"`
	Permission string    `json:"permission,omitempty"`
	Items      []NavItem `json:"items,omitempty"`
}

// SlotContribution renders into a slot_kind published by another addon.
type SlotContribution struct {
	SlotKind   string `json:"slot_kind"`
	Entry      string `json:"entry"`
	Order      int    `json:"order,omitempty"`
	Permission string `json:"permission,omitempty"`
}

// Action is a UI-triggered operation. Beyond the thin {key,label,handler,
// target_model} dispatch core it can declare a rich, declarative action modal
// (a form built from Fields) and/or delegate to a custom federated modal
// component (Modal — the slot_kind the host loads from the addon's frontend
// bundle). Both surfaces are consumed by the SDK's ActionModalDispatcher.
type Action struct {
	Key         string  `json:"key"`
	Label       string  `json:"label,omitempty"`
	Icon        string  `json:"icon,omitempty"`
	Handler     Handler `json:"handler"`
	TargetModel string  `json:"target_model,omitempty"`

	// Fields declares a declarative form the host renders in the action modal
	// before dispatching the handler. Optional — an action with no fields and
	// no modal is a plain one-click action.
	Fields []ActionField `json:"fields,omitempty"`

	// Modal is the slot_kind of a custom federated modal component the host
	// mounts instead of (or alongside) the declarative form — for actions whose
	// UI is too rich for a flat field list (e.g. a checkout panel). It is the
	// same federation slot_kind addressing used elsewhere in the contract.
	Modal string `json:"modal,omitempty"`

	// Confirm asks the host to show a confirmation step before dispatching.
	Confirm bool `json:"confirm,omitempty"`
	// ConfirmMessage is the body shown in that confirmation step.
	ConfirmMessage string `json:"confirm_message,omitempty"`
}

// ActionField is one input in an action modal's declarative form. It mirrors
// the SDK's ActionFieldDef (runtime-react/src/types.ts) 1:1 so the v3 field
// maps cleanly onto what dynamic-form / ActionModalDispatcher render.
type ActionField struct {
	Key            string           `json:"key"`
	Label          string           `json:"label,omitempty"`
	Type           string           `json:"type"`
	Required       bool             `json:"required,omitempty"`
	Options        []FieldOption    `json:"options,omitempty"`
	Default        any              `json:"default,omitempty"`
	Placeholder    string           `json:"placeholder,omitempty"`
	Widget         string           `json:"widget,omitempty"`
	Ref            string           `json:"ref,omitempty"`
	SearchEndpoint string           `json:"search_endpoint,omitempty"`
	Validation     *FieldValidation `json:"validation,omitempty"`
}

// FieldOption is a value/label choice for select-typed action fields.
type FieldOption struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

// FieldValidation carries client-side validation hints for an action field.
// The SDK reads these directly from the manifest-served action metadata.
type FieldValidation struct {
	Regex  string   `json:"regex,omitempty"`
	Min    *float64 `json:"min,omitempty"`
	Max    *float64 `json:"max,omitempty"`
	Custom string   `json:"custom,omitempty"`
}

// Tool is an LLM-facing action wired into the host agent-tool registry.
type Tool struct {
	Key         string                 `json:"key"`
	Description string                 `json:"description"`
	InputSchema map[string]interface{} `json:"input_schema,omitempty"`
	Handler     Handler                `json:"handler,omitempty"`
}

// Subscription is an event handler. Must be backed by an event:subscribe capability.
type Subscription struct {
	Event   string  `json:"event"`
	Handler Handler `json:"handler"`
	Filter  string  `json:"filter,omitempty"`
}

// Handler is the polymorphic invocation target for actions/tools/subscriptions.
type Handler struct {
	Type     string `json:"type"` // "wasm" | "webhook"
	Function string `json:"function,omitempty"`
	URL      string `json:"url,omitempty"`
}

// ExtensionPoints is what this addon publishes for others to extend.
type ExtensionPoints struct {
	Events                  []PublishedEvent `json:"events,omitempty"`
	SlotKinds               []PublishedSlot  `json:"slot_kinds,omitempty"`
	ModelExtensionsAccepted []string         `json:"model_extensions_accepted,omitempty"`
}

// PublishedEvent declares an event this addon emits.
type PublishedEvent struct {
	Name          string `json:"name"`
	PayloadSchema string `json:"payload_schema,omitempty"`
	Description   string `json:"description,omitempty"`
}

// PublishedSlot declares a UI slot this addon owns.
type PublishedSlot struct {
	Name        string `json:"name"`
	PropsSchema string `json:"props_schema,omitempty"`
	Description string `json:"description,omitempty"`
}

// Lifecycle is the hook map plus the upgrade ladder.
type Lifecycle struct {
	Install   string        `json:"install,omitempty"`
	Uninstall string        `json:"uninstall,omitempty"`
	Enable    string        `json:"enable,omitempty"`
	Disable   string        `json:"disable,omitempty"`
	Upgrade   []UpgradeStep `json:"upgrade,omitempty"`
}

// UpgradeStep is one entry in the upgrade ladder.
type UpgradeStep struct {
	From     string `json:"from"`
	Type     string `json:"type"` // "wasm" | "sql" | "webhook"
	Function string `json:"function"`
}

// I18n is the i18n bundle pointer.
type I18n struct {
	DefaultLocale string       `json:"default_locale"`
	Bundles       []I18nBundle `json:"bundles"`
}

// I18nBundle is a single (locale, path) pair.
type I18nBundle struct {
	Locale string `json:"locale"`
	Path   string `json:"path"`
}

// RBAC is the role + permission catalog.
type RBAC struct {
	Roles       []Role          `json:"roles,omitempty"`
	Permissions []PermissionDef `json:"permissions,omitempty"`
}

// Role is a named bundle of permission keys.
type Role struct {
	Key         string   `json:"key"`
	Label       string   `json:"label"`
	Description string   `json:"description,omitempty"`
	Permissions []string `json:"permissions,omitempty"`
}

// PermissionDef declares a permission key.
type PermissionDef struct {
	Key         string `json:"key"`
	Label       string `json:"label,omitempty"`
	Description string `json:"description,omitempty"`
}

// Setting is a user-facing tenant-scoped setting.
type Setting struct {
	Key      string          `json:"key"`
	Type     string          `json:"type"`
	Label    string          `json:"label,omitempty"`
	Default  interface{}     `json:"default,omitempty"`
	Required bool            `json:"required,omitempty"`
	Options  []SettingOption `json:"options,omitempty"`
}

// SettingOption is a value/label pair for select-typed settings.
type SettingOption struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

// Billing groups marketplace billing surface.
type Billing struct {
	MeteredEvents []MeteredEvent `json:"metered_events,omitempty"`
}

// MeteredEvent declares a billing meter.
type MeteredEvent struct {
	Event        string  `json:"event"`
	Unit         string  `json:"unit"`
	RevenueShare float64 `json:"revenue_share,omitempty"`
}

// Preset block for kind: "Preset" manifests.
type Preset struct {
	Addons   []PresetAddon          `json:"addons"`
	Defaults map[string]interface{} `json:"defaults,omitempty"`
}

// PresetAddon is one entry in a preset's bundle list.
type PresetAddon struct {
	Key      string `json:"key"`
	Version  string `json:"version"`
	Optional bool   `json:"optional,omitempty"`
}

// Theme block for kind: "Theme" manifests.
type Theme struct {
	Tokens        string            `json:"tokens,omitempty"`
	Fonts         []string          `json:"fonts,omitempty"`
	IconOverrides map[string]string `json:"icon_overrides,omitempty"`
}

// ConnectorPack block for kind: "ConnectorPack" manifests.
type ConnectorPack struct {
	Providers []ConnectorProvider `json:"providers,omitempty"`
}

// ConnectorProvider declares a third-party API + its credential settings.
type ConnectorProvider struct {
	Key         string    `json:"key"`
	Label       string    `json:"label,omitempty"`
	Credentials []Setting `json:"credentials"`
}

// Signature is the detached ed25519 signature over the manifest body.
type Signature struct {
	Algorithm string `json:"algorithm"`
	KeyID     string `json:"key_id"`
	Value     string `json:"value"`
	SignedAt  string `json:"signed_at"`
}
