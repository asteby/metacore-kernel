package manifest

// APIVersion is the kernel contract version this package implements.
// Addons declare `kernel: ">=X.Y <Z"` to opt into a compatibility window.
//
// Bumped to 3.0.0 once the bundle ingestion path learned to dual-read the
// Module Contract v3 manifest (see bundle.parseManifest + manifest.FromV3):
// the kernel now satisfies v3 addons' `kernel: ">=3.0.0 <4.0.0"` requirement.
// Legacy v2 addons declare no range (checkKernelRange skips them) so they are
// unaffected.
const APIVersion = "3.0.0"

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
	// name), "url" (absolute URL) or "svg" (IconSlug is a relative path to an
	// SVG asset bundled inside the addon, served by the hub). Mirrors the host
	// MarketplaceIntegration shape so frontends consume them identically.
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

	Navigation []NavGroup             `json:"navigation,omitempty"`
	Extensions []ModelExtension       `json:"extensions,omitempty"`
	Settings   []SettingDef           `json:"settings,omitempty"`
	Hooks      map[string]string      `json:"hooks,omitempty"`
	Actions    map[string][]ActionDef `json:"actions,omitempty"`
	// Tools are LLM-facing actions. Conversational hosts sync these into
	// their agent-tool registry on install so an AI can trigger them from a
	// user message. Unlike Actions (UI-triggered record operations) Tools
	// are semantic and carry extraction hints for parameter inference.
	Tools            []ToolDef         `json:"tools,omitempty"`
	ModelDefinitions []ModelDefinition `json:"model_definitions,omitempty"`
	// Events lists the event names the addon may emit. It predates the
	// capability-based gate (`{kind:"event:emit", target:"<name>"}`) and is
	// kept on the type for backwards compatibility with v0.x manifests.
	//
	// @deprecated — Will be derived from Capabilities[event:emit] in v2 and
	// removed. The runtime gate is already the capability set; declaring
	// names here is documentation only. ValidateAdvisory emits a warning
	// when an Events entry has no matching `event:emit` capability so the
	// drift surfaces at install time. New manifests should declare event
	// names via Capabilities exclusively.
	Events []string `json:"events,omitempty"`
	// LifecycleHooks maps a hook event ("install" | "uninstall" | "enable"
	// | "disable" | "upgrade" | "before_create" | "after_create" |
	// "before_update" | "after_update" | "before_delete" | "after_delete")
	// to one or more HookDef entries the kernel dispatches when the event
	// fires. Lifecycle transitions run from the installer; CRUD events
	// fire from dynamic.Service. See [docs/lifecycle-hooks.md] for the
	// full contract (timeouts, error semantics, payload shape).
	LifecycleHooks map[string][]HookDef         `json:"lifecycle_hooks,omitempty"`
	I18n           map[string]map[string]string `json:"i18n,omitempty"`

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
	// Countries scopes the addon to ISO 3166-1 alpha-2 markets (e.g. ["MX"]).
	// Empty/omitted = global. The hub uses it to filter the catalog by the
	// browsing user's country so region-specific (fiscal) addons don't surface
	// where they don't apply.
	Countries []string `json:"countries,omitempty"`
	// MetadataI18n carries marketplace catalog localizations keyed by locale
	// ("es", "en"). Populated by FromV3 from v3 metadata.i18n. The hub stores
	// and serves these so the catalog renders in the browsed locale; the flat
	// Name/Description/Features above stay the default/fallback copy. Tagged
	// "metadata_i18n" because the top-level Manifest.I18n (app-UI string
	// bundles) already owns the "i18n" JSON key.
	MetadataI18n map[string]MetadataLocale `json:"metadata_i18n,omitempty"`

	// Connectors / Schedules / Webhooks are the host projection of the v3
	// pipeline-runtime primitives (see manifest/v3.Connector/Schedule/
	// InboundWebhook). Connectors enumerate the per-org credential providers the
	// host stores and the connectors runtime resolves; Schedules are the cron
	// jobs the kernel scheduler fires per org; Webhooks are the inbound routes
	// the host mounts and the kernel webhookin receiver routes. All optional —
	// empty slices leave the addon with no runtime primitives (back-compat).
	Connectors []ConnectorDef      `json:"connectors,omitempty"`
	Schedules  []ScheduleDef       `json:"schedules,omitempty"`
	Webhooks   []InboundWebhookDef `json:"webhooks,omitempty"`

	// Documents is the host projection of v3 contributions.documents[]: the
	// printable-document templates the addon binds to its models. The host
	// render engine reads these off the installed manifest to serve per-record
	// PDFs. Empty = the addon prints nothing (back-compat default).
	Documents []DocumentDef `json:"documents,omitempty"`
}

// DocumentDef is the host/runtime projection of a v3 DocumentDef: a printable
// document template bound to a model. Template is a bundle-relative path to an
// HTML file the host render engine hydrates (with {{record.*}},
// {{org.branding.*}}, {{line_items}}, {{now}} tokens) and converts to PDF.
// Paper is A4|letter|ticket80; Filename is an optional download-name template.
type DocumentDef struct {
	Key      string `json:"key"`
	Model    string `json:"model"`
	Template string `json:"template"`
	Paper    string `json:"paper"`
	Filename string `json:"filename,omitempty"`
}

// ConnectorDef is the host/runtime projection of a v3 Connector: a third-party
// credential provider whose Credentials the host stores per org (encrypted) and
// the connectors runtime resolves for wasm handlers and webhook secrets.
type ConnectorDef struct {
	Key         string          `json:"key"`
	Label       string          `json:"label,omitempty"`
	Auth        string          `json:"auth,omitempty"`
	Credentials []CredentialDef `json:"credentials,omitempty"`
}

// CredentialDef is one field of a ConnectorDef the org supplies. Secret==true
// (derived from a "secret" credential type) signals the host to store it
// encrypted. Mirrors the v3 Setting fields used by connector credentials.
type CredentialDef struct {
	Key        string      `json:"key"`
	Type       string      `json:"type,omitempty"`
	Default    interface{} `json:"default,omitempty"`
	Required   bool        `json:"required,omitempty"`
	Validation string      `json:"validation,omitempty"`
	Secret     bool        `json:"secret,omitempty"`
}

// ScheduleDef is the host/runtime projection of a v3 Schedule: a cron job the
// kernel scheduler fires per org. Every is a Go duration string; Do is the
// dispatchable handler reference ("wasm:"/"webhook:"/"compiled:").
type ScheduleDef struct {
	Key   string `json:"key"`
	Every string `json:"every"`
	Do    string `json:"do"`
}

// InboundWebhookDef is the host/runtime projection of a v3 InboundWebhook: a
// route the host mounts and the kernel webhookin receiver routes after
// verifying the signature (Verify) against the secret resolved from SecretRef.
type InboundWebhookDef struct {
	Key       string `json:"key"`
	Path      string `json:"path"`
	Verify    string `json:"verify,omitempty"`
	SecretRef string `json:"secret_ref,omitempty"`
	Do        string `json:"do"`
}

// MetadataLocale is one locale's catalog copy (name/description/features).
// Any field may be empty — consumers fall back to the default Manifest copy.
type MetadataLocale struct {
	Name        string   `json:"name,omitempty"`
	Description string   `json:"description,omitempty"`
	Features    []string `json:"features,omitempty"`
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
	// Filter is a static column→value filter the host applies when rendering
	// this entry's list view (e.g. {"status":"reception"}), so an addon can
	// publish one nav entry per status. Empty/omitted means no filter.
	Filter map[string]string `json:"filter,omitempty"`

	// ViewType / GroupBy are the host projection of the v3 NavItem kanban hint:
	// ViewType "kanban" makes the SDK render a board grouped by GroupBy (else the
	// default DynamicTable). Empty ViewType = table. See manifest/v3.NavItem.
	ViewType string `json:"viewType,omitempty"`
	GroupBy  string `json:"groupBy,omitempty"`
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

	// Layout selects how the host frames the addon UI on screen.
	//
	//	""          — legacy / unset: treated as "shell" for retro-compat
	//	              with every manifest published before this field landed.
	//	"shell"     — default chrome: sidebar + topbar + content slot. The
	//	              host renders navigation around the addon and the
	//	              federated module only owns the content viewport.
	//	"immersive" — full-page takeover. The addon owns the entire viewport
	//	              (no sidebar, no topbar) and is responsible for any
	//	              navigation back to the shell. Use for point-of-sale
	//	              terminals, kitchen-display screens, customer-facing
	//	              waiting-room displays and other kiosk-style UIs where
	//	              the surrounding ERP chrome is noise.
	//
	// Validate rejects any value outside this closed set so addon authors
	// catch typos at install time rather than at first paint.
	Layout string `json:"layout,omitempty"`
}

// BackendSpec declares how the addon's backend code is executed.
//
//	Runtime = "webhook" — legacy remote HTTP dispatch (no Entry needed).
//	Runtime = "wasm"    — sandboxed in-process module at Entry. Exports
//	                      enumerates the function symbols each hook can
//	                      dispatch to; kernel/runtime/wasm enforces it.
//	Runtime = "binary"  — RESERVED for a future native side-car runtime.
//	                      The manifest validates (so addon authors can
//	                      declare it ahead of time) but the kernel has no
//	                      installer / invoker for it yet. ValidateAdvisory
//	                      emits a warning and host.LoadWASMFromBundle
//	                      surfaces a runtime_not_implemented error at
//	                      install time. See docs/wasm-abi.md
//	                      "Reserved / advisory fields".
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
//
//	{ "kind": "db:read",   "target": "orders"                  }
//	{ "kind": "db:write",  "target": "addon_tickets.*"         }
//	{ "kind": "http:fetch","target": "https://api.stripe.com/*"}
//	{ "kind": "event:emit","target": "sale.created"            }
//	{ "kind": "event:subscribe", "target": "invoice.stamped"   }
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
//
// Icon/Color/Image are OPTIONAL visual hints forwarded verbatim from
// manifest/v3 FieldOption so a STATIC option list can render richly (a coloured
// status dot, a brand icon, a thumbnail). They ride the legacy Option as a
// carrier so they survive the v3 → host conversion and land on the served
// modelbase.OptionDef. All optional; the runtime ignores them.
type Option struct {
	Value string `json:"value"`
	Label string `json:"label"`
	Icon  string `json:"icon,omitempty"`
	Color string `json:"color,omitempty"`
	Image string `json:"image,omitempty"`
	// When is the OPTIONAL static cascade guard forwarded verbatim from
	// manifest/v3 FieldOption.When so it survives the v3 → host conversion
	// and lands on the served modelbase.OptionDef.When. Nil = always applies.
	When *OptionCondition `json:"when,omitempty"`
}

// OptionCondition is the legacy carrier for the static option cascade guard —
// it forwards manifest/v3 OptionCondition through the host conversion onto the
// served modelbase.OptionCondition. Field names the sibling enum field (falls
// back to the container's DependsOn when empty); In / NotIn scope the value.
type OptionCondition struct {
	Field string   `json:"field,omitempty"`
	In    []string `json:"in,omitempty"`
	NotIn []string `json:"not_in,omitempty"`
}

// ToolDef is an LLM-callable function the addon exposes. Hosts with
// conversational AI register these into their agent-tool registry so a
// user message can trigger them. The endpoint receives an HMAC-signed
// webhook produced by kernel/security.WebhookDispatcher.
type ToolDef struct {
	ID               string           `json:"id"` // unique within the addon
	Name             string           `json:"name"`
	Description      string           `json:"description"`        // shown to the LLM — be specific
	Category         string           `json:"category,omitempty"` // communication | query | action | integration
	InputSchema      []ToolInputParam `json:"input_schema,omitempty"`
	TriggerKeywords  []string         `json:"trigger_keywords,omitempty"`
	TriggerIntents   []string         `json:"trigger_intents,omitempty"`
	Endpoint         string           `json:"endpoint"`                     // relative or absolute URL
	Method           string           `json:"method,omitempty"`             // defaults to POST
	AutoCreateRecord string           `json:"auto_create_record,omitempty"` // host-specific record type
	Settings         map[string]any   `json:"settings,omitempty"`
	Timeout          int              `json:"timeout,omitempty"` // seconds
	CacheTTL         int              `json:"cache_ttl,omitempty"`
	Priority         int              `json:"priority,omitempty"`
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
	Key            string     `json:"key"`
	Name           string     `json:"name"`
	Label          string     `json:"label"`
	Icon           string     `json:"icon,omitempty"`
	Fields         []FieldDef `json:"fields,omitempty"`
	RequiresState  []string   `json:"requiresState,omitempty"`
	Confirm        bool       `json:"confirm,omitempty"`
	ConfirmMessage string     `json:"confirmMessage,omitempty"`
	Modal          string     `json:"modal,omitempty"`      // slot name for a custom modal
	// Steps is the host/runtime projection of a v3 Action.steps wizard: one
	// page per step, per-step validation, single submit with the union of all
	// steps' values. Mutually exclusive with Fields. See manifest/v3.ActionStep.
	Steps []ActionStepDef `json:"steps,omitempty"`
	Placement      string     `json:"placement,omitempty"`  // "row" (default), "table", or "create" — see v3.Action.Placement
	ModalWidth     string     `json:"modalWidth,omitempty"` // explicit modal width (CSS length / px); SDK reads action.modalWidth

	// Trigger declares how the action dispatches when invoked. Optional —
	// when nil the legacy behaviour applies (the host resolves the action via
	// the implicit Hooks map / webhook URL). See ActionTrigger for the per-
	// type contract; consumers (bridge/actions.go, runtime/wasm) pick the
	// new field up incrementally in follow-up PRs.
	Trigger *ActionTrigger `json:"trigger,omitempty"`

	// Idempotency is the host/runtime projection of a v3 Action.idempotency
	// block. When non-nil the kernel's ExecAction keys a stored response by
	// (org, model, action, <payload[KeyField]>) and replays it on repeats,
	// skipping the dispatch. Nil = the action is not replay-guarded. See
	// manifest/v3.ActionIdempotency and dynamic.Service.ExecAction.
	Idempotency *IdempotencyDef `json:"idempotency,omitempty"`
}

// ActionStepDef is the host/runtime projection of a v3 ActionStep: one wizard
// page (title + optional description + the fields rendered on that page).
type ActionStepDef struct {
	Title       string     `json:"title"`
	Description string     `json:"description,omitempty"`
	Fields      []FieldDef `json:"fields"`
}

// IdempotencyDef is the host/runtime projection of a v3 ActionIdempotency: the
// payload field whose value is the idempotency key. See manifest/v3 for the
// full contract.
type IdempotencyDef struct {
	KeyField string `json:"keyField"`
}

// ActionTrigger declares how an ActionDef is dispatched when invoked from
// the UI. It is union-discriminated by Type and intentionally minimal — the
// goal of this revision is to land the contract so addon authors can declare
// triggers ahead of the kernel learning to honour them.
//
//	Type = "wasm"    — invoke an exported function on the addon's wasm module.
//	                   Export is required and MUST appear in Backend.Exports
//	                   so the wasm host can resolve it at dispatch time. Use
//	                   this for in-process actions that should reuse the
//	                   addon's compiled module (and, when RunInTx=true, the
//	                   request's open DB transaction).
//
//	Type = "webhook" — dispatch via the legacy HTTP webhook (Backend.URL or
//	                   the implicit Hooks map). Export MUST be empty (the
//	                   webhook target is encoded in the URL, not by symbol)
//	                   and RunInTx MUST be false because the network hop
//	                   escapes the request transaction.
//
//	Type = "noop"    — UI-only action. The kernel records the click for
//	                   observability and bridge-side hooks can react to it,
//	                   but no addon code runs and Export / RunInTx must
//	                   stay empty / false.
//
// RunInTx is honoured only by Type=wasm and instructs the kernel to invoke
// the export inside the same DB transaction as the row-level mutation that
// prompted the action. This is what unlocks "stamp this invoice and write
// the linked log entry atomically" without leaking partially-applied state.
type ActionTrigger struct {
	Type    string `json:"type"`             // "wasm" | "webhook" | "noop"
	Export  string `json:"export,omitempty"` // wasm export name; required when Type=wasm
	RunInTx bool   `json:"run_in_tx,omitempty"`
}

// FieldDef is an input field used by action forms and model definitions.
type FieldDef struct {
	Name     string      `json:"name"`
	Label    string      `json:"label"`
	Type     string      `json:"type"`
	Required bool        `json:"required,omitempty"`
	Default  interface{} `json:"default,omitempty"`
	Options  []Option    `json:"options,omitempty"`
	Size     int         `json:"size,omitempty"`

	// Key carries the field identifier with the SAME json tag the host
	// (modelbase.FieldDef) and the SDK read. Name (json:"name") is kept for
	// legacy consumers, but the host JSON round-trip keys off "key" — without
	// this the field key arrived empty at the renderer.
	Key string `json:"key,omitempty"`

	// Rich action-field properties forwarded verbatim from manifest/v3
	// ActionField so the declarative modal renders fully (searchable pickers,
	// multi-column line-items with totals/balance) instead of collapsing every
	// field to a plain input. The legacy flat FieldDef intentionally had no slot
	// for these; the SDK reads them off the host-served action metadata, so they
	// must survive the v3 → host conversion. JSON tags match modelbase.FieldDef.
	Widget         string            `json:"widget,omitempty"`
	Ref            string            `json:"ref,omitempty"`
	Placeholder    string            `json:"placeholder,omitempty"`
	SearchEndpoint string            `json:"searchEndpoint,omitempty"`
	ItemFields     []FieldDef        `json:"item_fields,omitempty"`
	LockRows       bool              `json:"lock_rows,omitempty"`
	Total          bool              `json:"total,omitempty"`
	Balance        *FieldBalanceRule `json:"balance,omitempty"`

	// DependsOn and OptionsConfig forward the v3 dependent-picker declaration
	// (ActionField.depends_on + the `options` object form). DependsOn names the
	// sibling field whose value supplies the cascade filter_value; OptionsConfig
	// carries the dynamic source declaration (source/filter_by/value/label_ref/
	// description) the host projects onto modelbase.FieldDef so the served action
	// spec reaches the SDK and the host's OptionsConfigResolver. JSON tags match
	// modelbase.FieldDef so they survive the host round-trip. Nil/empty = none.
	DependsOn     string             `json:"depends_on,omitempty"`
	OptionsConfig *DynamicOptionsDef `json:"optionsConfig,omitempty"`

	// OptionsSource forwards the v3 ActionField.options_source (a host-registered
	// dynamic options provider key, e.g. "connector_repos"). It rides the matching
	// modelbase.FieldDef json tag so the key survives the v3 → host conversion and
	// lands on the served action metadata; the host then materialises the
	// value/label list from its provider registry. Empty = static/no options.
	OptionsSource string `json:"options_source,omitempty"`

	// Upload-field properties forwarded verbatim from manifest/v3 ActionField
	// for a field with type "upload". Accept is the file-picker allow-list,
	// MaxSize the byte cap and StoragePath the logical storage prefix. The SDK
	// reads them off the host-served action metadata; matching JSON tags keep
	// them intact through the v3 → host conversion. Ignored on non-upload fields.
	Accept      string `json:"accept,omitempty"`
	MaxSize     int64  `json:"max_size,omitempty"`
	StoragePath string `json:"storage_path,omitempty"`

	// LabelImage/LabelIcon/LabelColor name a column on the REMOTE model a
	// dynamic_select resolves against whose value the SDK renders as a visual
	// beside each option's label (a product thumbnail, a brand icon, a status
	// colour). Forwarded verbatim from manifest/v3 ActionField with matching
	// JSON tags so they survive the v3 → host conversion and reach the SDK off
	// the served action metadata. Ignored on non-reference fields.
	LabelImage string `json:"label_image,omitempty"`
	LabelIcon  string `json:"label_icon,omitempty"`
	LabelColor string `json:"label_color,omitempty"`

	// VisibleWhen carries the v3 conditional-visibility predicate onto the served
	// action FieldDef so the SDK's action-modal renderer shows/hides the field
	// against the live form values. Nil = always visible. Pure UI metadata.
	VisibleWhen *VisibleWhenDef `json:"visible_when,omitempty"`
}

// FieldBalanceRule mirrors manifest/v3 FieldBalanceRule with identical JSON
// tags so it round-trips through the host's action metadata untouched.
type FieldBalanceRule struct {
	DebitColumn    string `json:"debit_column"`
	CreditColumn   string `json:"credit_column"`
	Message        string `json:"message,omitempty"`
	RequireNonzero *bool  `json:"require_nonzero,omitempty"`
}

// DynamicOptionsDef is the legacy carrier for the v3 `options` OBJECT form (a
// dependent-picker source declaration). It rides ColumnDef/FieldDef so the
// {source, filter_by, value, label_ref, description} block survives the v3 →
// host conversion and lands on the served modelbase.FieldOptionsConfig the host
// reads to build the kernel's OptionsConfigResolver entry. JSON tags match both
// modelbase.FieldOptionsConfig and the v3 contract so it round-trips intact.
type DynamicOptionsDef struct {
	// Type is always "dynamic" for this carrier (the object form is the dynamic
	// source declaration). It rides so the JSON round-trip onto
	// modelbase.FieldOptionsConfig.Type lands "dynamic" and the kernel's
	// Service.Options takes the query branch without the host re-stamping it.
	Type        string `json:"type,omitempty"`
	Source      string `json:"source,omitempty"`
	FilterBy    string `json:"filter_by,omitempty"`
	Value       string `json:"value,omitempty"`
	Label       string `json:"label,omitempty"`
	LabelRef    string `json:"label_ref,omitempty"`
	Description string `json:"description,omitempty"`
}

// HookDef is one declared lifecycle hook entry. The kernel dispatches
// every entry under a given LifecycleHooks key in Priority-ascending order
// when the matching event fires.
//
//   - Event mirrors the map key for self-documenting manifests. When set
//     it MUST match the key (Validate rejects mismatches); when empty the
//     map key is authoritative.
//   - Target is the dispatch destination (wasm export, webhook URL, or
//     reserved prompt). Required.
//   - Priority orders multiple hooks bound to the same event — lower
//     numbers fire first. Default 0.
//   - Async detaches the dispatch onto a goroutine. Allowed only on
//     after_*-family events because before-events and lifecycle
//     transitions must block on the result to honour a veto.
type HookDef struct {
	Event    string     `json:"event"`
	Target   HookTarget `json:"target"`
	Priority int        `json:"priority,omitempty"`
	Async    bool       `json:"async,omitempty"`
}

// HookTarget describes where a lifecycle hook dispatches to.
//
//	Type = "wasm"    — invoke the exported function `Function` on the
//	                   addon's compiled wasm module. The function MUST
//	                   appear in Backend.Exports.
//	Type = "webhook" — POST the event payload to URL (HMAC-signed via
//	                   the host's webhook dispatcher).
//	Type = "prompt"  — reserved for a future LLM dispatcher; validates
//	                   but currently runs as a no-op unless the host
//	                   registers a custom prompt dispatcher.
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
	// Relations declares model-to-model edges the kernel uses to derive
	// joins, eager loading, REST sub-resources and SDK metadata. The slice
	// is optional — addons that only expose flat tables can omit it and
	// keep the legacy behaviour. See RelationDef for the supported shapes.
	Relations []RelationDef `json:"relations,omitempty"`
	Table     interface{}   `json:"table,omitempty"` // UI table spec (opaque)
	Modal     interface{}   `json:"modal,omitempty"` // UI modal spec (opaque)

	// Seed declares default data the installer inserts on install, idempotent
	// by a natural key column (Seed.Key). The host (ops executor) reads
	// def.Seed to perform the seeding after migrations. Optional — flat models
	// without seed data leave it nil and keep the legacy behaviour.
	Seed *SeedDef `json:"seed,omitempty"`

	// Formulas declare columns whose value the kernel computes from a pure
	// arithmetic expression over the SAME row's other columns, evaluated
	// before every create/update write (Tier-2 of the declarative compute
	// engine). Optional. See Formula and dynamic.RegisterComputeHooks.
	Formulas []Formula `json:"formulas,omitempty"`

	// StageField / Stages / Transitions / OnTransition are the host-side
	// projection of a v3 model's stage machine (see manifest/v3.Model). The
	// dynamic engine reads them to derive the `status` display of StageField, to
	// validate stage moves on Update, and to fire OnTransition hooks. Empty
	// StageField/Stages = no stage machine (the legacy, unrestricted behaviour).
	StageField   string              `json:"stageField,omitempty"`
	Stages       []StageDef          `json:"stages,omitempty"`
	Transitions  []TransitionDef     `json:"transitions,omitempty"`
	OnTransition []TransitionHookDef `json:"onTransition,omitempty"`

	// Sequences carries the v3 Model.sequences (atomic per-org/branch folio
	// counters) through the v3 → host conversion so the dynamic engine can stamp
	// auto-folios on create and the wasm sequence_next import can resolve them.
	// See manifest/v3.Sequence. Empty = the model has no folios.
	Sequences []SequenceDef `json:"sequences,omitempty"`

	// Locking is the host/runtime projection of a v3 Model.locking. "row" makes
	// dynamic.Service.Update take a SELECT … FOR UPDATE lock on the target row
	// inside a transaction before evaluating column Constraints, so a concurrent
	// increment-then-check guard is race-free. Empty = no extra locking. See
	// manifest/v3.Model and dynamic constraint evaluation.
	Locking string `json:"locking,omitempty"`

	// FormLayout carries the v3 Model.form_layout (create/edit form grouping —
	// collapsible sections or a step wizard) through the v3 → host conversion so
	// the host can project it onto the served form metadata. Columns bind to a
	// section/step via ColumnDef.Section. Pure UI metadata; the DDL/write planes
	// ignore it. Nil = a flat form (legacy). See manifest/v3.FormLayout.
	FormLayout *FormLayoutDef `json:"form_layout,omitempty"`
}

// FormLayoutDef is the legacy carrier for the v3 FormLayout block. It rides the
// ModelDefinition so the {mode, sections} grouping survives the v3 → host
// conversion and lands on the served form metadata the SDK reads. JSON tags
// match the v3 + host contract so it round-trips byte-for-byte.
type FormLayoutDef struct {
	Mode     string           `json:"mode"`
	Sections []FormSectionDef `json:"sections"`
}

// FormSectionDef is the legacy carrier for one v3 FormSection (a collapsible
// section or a wizard step). Title/Description may be a literal or an i18n key.
type FormSectionDef struct {
	Key         string          `json:"key"`
	Title       string          `json:"title,omitempty"`
	Description string          `json:"description,omitempty"`
	Collapsed   bool            `json:"collapsed,omitempty"`
	VisibleWhen *VisibleWhenDef `json:"visible_when,omitempty"`
}

// StageDef is the host/runtime projection of a v3 Stage. See manifest/v3.Stage
// for the full contract.
type StageDef struct {
	Key     string `json:"key"`
	Label   string `json:"label,omitempty"`
	Color   string `json:"color,omitempty"`
	Order   int    `json:"order,omitempty"`
	IsFinal bool   `json:"isFinal,omitempty"`
}

// TransitionDef is the host/runtime projection of a v3 Transition: one allowed
// (from → to) stage move.
type TransitionDef struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// TransitionHookDef is the host/runtime projection of a v3 TransitionHook. Set
// assigns fields on the transitioning row itself (scalar = direct assignment;
// "+tag"/"-tag" on a json column = idempotent append/remove) and is applied
// before the row is persisted. Do references the dispatchable handler
// (`wasm:`/`webhook:`/`compiled:`); Required makes a dispatch failure roll back
// the transition. A hook carries Set, Do, or both. See manifest/v3.TransitionHook.
type TransitionHookDef struct {
	From     string         `json:"from"`
	To       string         `json:"to"`
	Set      map[string]any `json:"set,omitempty"`
	Do       string         `json:"do,omitempty"`
	Required bool           `json:"required,omitempty"`
}

// Formula declares a column on the owning model whose value the kernel
// computes from a pure arithmetic expression over the SAME row's other
// columns, evaluated before every create/update DB write (Tier-2 of the
// declarative compute engine). See manifest/v3.Formula for the full contract;
// this is the legacy/runtime projection the installer and dynamic engine read.
//
// SECURITY: Expr is parsed with a strict allowlist (identifiers resolving to
// real columns, decimal numbers, + - * / and parentheses only); anything else
// is rejected at validation time.
type Formula struct {
	Target string `json:"target"`
	Expr   string `json:"expr,omitempty"`
	// Tier selects the compute engine: 0/2 = arithmetic Tier-2 over Expr (the
	// default); 3 = a wasm handler (Handler) computes the value. See
	// manifest/v3.Formula.
	Tier int `json:"tier,omitempty"`
	// Handler is the Tier-3 backend, "wasm:<export>". Set only when Tier is 3.
	Handler string `json:"handler,omitempty"`
}

// SequenceDef is the host/runtime projection of a v3 Model.Sequence: an atomic
// folio counter the kernel maintains per org (or branch). See manifest/v3.Sequence.
type SequenceDef struct {
	Key    string `json:"key"`
	Scope  string `json:"scope,omitempty"`
	Format string `json:"format"`
}

// ConstraintDef is the host/runtime projection of a v3 Column.Constraint: a
// boolean guard predicate the dynamic engine evaluates inside the create/update
// transaction. See manifest/v3.Constraint for the full contract and the
// security note on Expr.
type ConstraintDef struct {
	Expr     string `json:"expr"`
	ErrorKey string `json:"errorKey"`
}

// Rollup declares a PARENT column the kernel maintains as an aggregate
// (sum/count/avg/min/max) over a relation's child rows (Tier-1 of the
// declarative compute engine). See manifest/v3.Rollup for the full contract;
// this is the legacy/runtime projection the dynamic engine reads.
//
// Either From (a single child column) or Expr (a child arithmetic expression)
// supplies the aggregated value; fn=count ignores both. Exactly one of
// From/Expr may be set (count may omit both).
//
// SECURITY: Expr goes raw into aggregate SQL — parsed with the same strict
// arithmetic allowlist as Formula.Expr and every identifier double-quoted.
type Rollup struct {
	Target string `json:"target"`
	Fn     string `json:"fn,omitempty"`
	From   string `json:"from,omitempty"`
	Expr   string `json:"expr,omitempty"`
}

// SeedDef is the host-side projection of a v3 model's seed block. The installer
// inserts each row in Rows on install, treating Key as the natural key for
// idempotency: a row is only inserted when no existing row (scoped to the
// installing org) already carries that Key value. Key names a declared column
// on the owning ModelDefinition; each Row is an object of column name → value.
type SeedDef struct {
	// Key is the column used for idempotency (the natural key the installer
	// matches on to decide whether a row already exists).
	Key string `json:"key"`
	// Rows are the default records, each an object of column name → value.
	Rows []map[string]any `json:"rows"`
}

// RelationDef declares an inter-model relationship rooted at the owning
// ModelDefinition. The kernel uses it to derive joins, eager-loading
// hints, REST sub-resources and the metadata exposed to the TS SDK. Only
// the two shapes below are supported in this revision; richer cardinalities
// (one_to_one, polymorphic) can be added later without breaking existing
// manifests because RelationDef.Kind is a discriminator.
//
//	Kind = "one_to_many"  — the owning model has many rows on Through.
//	                        ForeignKey is the column on Through that points
//	                        back at the owner. References is the owner
//	                        column the FK targets (defaults to "id").
//	                        Pivot MUST be empty.
//
//	Kind = "many_to_many" — Pivot is a join table between owner and Through.
//	                        ForeignKey is the column on Pivot pointing at
//	                        the owner. References is the column on the
//	                        owner the pivot row holds (defaults to "id").
//	                        Through and Pivot are both required.
//
// Field shapes are the same identifier rules as ColumnDef.Name and
// ModelDefinition.TableName so the same regexes can validate them.
//
// Consumers (dynamic schema, modelbase metadata, the TS SDK) are NOT
// updated in this revision — the types only land in the manifest contract
// so addon authors can declare relations ahead of the kernel learning to
// honour them. Follow-up PRs wire each consumer up incrementally.
type RelationDef struct {
	// Name is a stable identifier for the relation, scoped to the owning
	// ModelDefinition. Lowercase snake_case, e.g. "tickets" or "tags".
	// It is what the SDK and REST sub-resources address the relation by,
	// so two relations on the same model cannot share a Name. Required.
	Name string `json:"name"`

	// Kind discriminates the relation shape: "one_to_many" | "many_to_many".
	Kind string `json:"kind"`

	// Through is the target model/table the relation points at. For
	// one_to_many it is where the foreign key column lives; for
	// many_to_many it is the model joined via Pivot. Required.
	Through string `json:"through"`

	// ForeignKey is the column carrying the relationship.
	//   one_to_many : column on Through pointing back at the owner.
	//   many_to_many: column on Pivot pointing at the owner.
	// Required in both shapes.
	ForeignKey string `json:"foreign_key"`

	// References is the owner column the ForeignKey targets. Empty means
	// "id" — the conventional primary key — so the common case stays
	// terse. Validated as a column identifier when set.
	References string `json:"references,omitempty"`

	// Pivot is the join table for many_to_many relations. It MUST be
	// empty for one_to_many (otherwise the relation shape is ambiguous)
	// and required for many_to_many. The pivot table is expected to
	// live in the addon schema; the kernel only needs the name here.
	Pivot string `json:"pivot,omitempty"`

	// Scope is a static equality filter applied to the child query, the
	// mechanism that makes POLYMORPHIC children addressable: a shared
	// Attachment/Address/Note table carries an `owner_model` discriminator,
	// so a Customer's attachments relation declares Scope
	// {"owner_model":"Customer"} alongside ForeignKey "owner_id". Empty =
	// no extra filter (the common, non-polymorphic case).
	Scope map[string]string `json:"scope,omitempty"`

	// Label is an optional i18n key / human label for the related-records
	// panel the SDK renders on a detail page. Empty falls back to Name.
	Label string `json:"label,omitempty"`

	// Readonly declares that the related-records panel must not allow
	// creating/editing/deleting child rows. Used for relations pointing at an
	// append-only ledger (e.g. an inventory kardex). Optional; defaults false.
	Readonly bool `json:"readonly,omitempty"`

	// Rollups declare PARENT columns the kernel maintains as aggregates over
	// this relation's child rows (Tier-1 of the declarative compute engine).
	// On every child create/update/delete the dynamic engine recomputes each
	// rollup target on the affected parent. Optional; only meaningful for
	// one_to_many. See Rollup.
	Rollups []Rollup `json:"rollups,omitempty"`
}

// ColumnDef is a column on an addon-installed table.
//
// Beyond the DDL plane (Name, Type, Size, Required, Index, Unique, Default,
// Ref) the struct also carries metadata for the kernel to derive UI hints,
// search endpoints and server-side validation from a single source of truth.
// Every metadata field is optional — zero values keep the legacy behaviour
// (column is visible everywhere, not searchable, no validation, widget
// inferred from Type), so addons authored against older kernels keep working.
type ColumnDef struct {
	Name     string `json:"name"`
	Type     string `json:"type"` // string, uuid, int, bigint, decimal, bool, timestamp, jsonb, text
	Size     int    `json:"size,omitempty"`
	Required bool   `json:"required,omitempty"`
	Index    bool   `json:"index,omitempty"`
	Unique   bool   `json:"unique,omitempty"`
	// Default accepts string ("'pending'", "now()"), number (42), or bool
	// literals from JSON. They are coerced to a DDL-safe string at install.
	Default any    `json:"default,omitempty"`
	Ref     string `json:"ref,omitempty"` // foreign key target: "orders" or "addon_tickets.comments"

	// Options is a STATIC select choice list for the column. It rides the
	// legacy ColumnDef as a carrier for the v3 Column.Options so a declared
	// enum survives the v3 → host conversion and lands on the served
	// modelbase.ColumnDef.Options / modelbase.FieldDef.Options — the SDK then
	// renders a plain select in the native create/edit form. Ref (above) is the
	// relation-picker twin (dynamic_select). Both are pure UI metadata; the DDL
	// plane ignores them. The Icon/Color/Image hints on each Option forward the
	// v3 FieldOption visuals so a status/brand list renders richly.
	Options []Option `json:"options,omitempty"`

	// OptionsSource names a DYNAMIC options provider (e.g. "registered_models",
	// "installed_addons") the HOST resolves at metadata-serve time instead of a
	// hardcoded Options list. It rides the legacy ColumnDef as a carrier for the
	// v3 Column.options_source so the key survives the v3 → host conversion and
	// lands on the served modelbase.ColumnDef.OptionsSource /
	// modelbase.FieldDef.OptionsSource — the host then materialises the
	// localized value/label list onto the served Options. The kernel implements
	// no providers (open enum, host-registered); an unknown key yields no
	// options. Pure UI metadata; the DDL plane ignores it.
	OptionsSource string `json:"options_source,omitempty"`

	// Generated carries the v3 Column.generated expression through the v3 → host
	// conversion so the DDL builder (dynamic.schema) can emit the column as
	// `GENERATED ALWAYS AS (<expr>) STORED`. A pure arithmetic expression over
	// sibling columns, validated with the strict computeexpr allowlist and
	// incompatible with Default/Required. Empty = ordinary column.
	Generated string `json:"generated,omitempty"`

	// Readonly carries the v3 Column.readonly flag through the v3 → host
	// conversion onto modelbase.ColumnDef/FieldDef.Readonly, marking a
	// SYSTEM-GENERATED column that DeriveFormFields excludes from the create form
	// and marks read-only in edit. Pure UI metadata; the DDL plane ignores it.
	Readonly bool `json:"readonly,omitempty"`

	// Constraints carries the v3 Column.constraints (declarative guard
	// predicates) through the v3 → host conversion so the dynamic engine can
	// evaluate them inside the create/update transaction. See manifest/v3.Constraint
	// and dynamic constraint evaluation. Empty = no guards on this column.
	Constraints []ConstraintDef `json:"constraints,omitempty"`

	// Sequence carries the v3 Column.sequence binding (the sequence key whose
	// next formatted value is auto-stamped on create) through the v3 → host
	// conversion. Empty = no auto-folio on this column.
	Sequence string `json:"sequence,omitempty"`

	// Label is the column's human header OR an i18n key resolving to it. It
	// rides the legacy ColumnDef as a carrier for the v3 Column.label so a
	// declared label survives the v3 → host conversion: dynamic.DeriveTableColumns
	// / DeriveFormFields prefer it over the humanized column name, and when it
	// is an i18n key (e.g. "models.product.sku") the host's localized metadata
	// transformer (metadata.NewLocalizedTableTransformer) resolves it to the
	// browsed locale at serve time instead of showing the raw key. Empty =
	// fall back to the humanized column name. Pure UI metadata; never DDL.
	Label string `json:"label,omitempty"`

	// Visibility scopes where the column is rendered. Allowed values:
	//   ""      — legacy / current behaviour (visible everywhere).
	//   "all"   — same as empty, made explicit.
	//   "table" — only the list/index page.
	//   "modal" — only the create/edit modal.
	//   "list"  — only API list payloads (omitted from detail/modal forms).
	// Unknown values are rejected by Validate.
	Visibility string `json:"visibility,omitempty"`

	// Searchable opts the column into the model's full-text/contains search
	// index. It is intentionally separate from Index (which is a DDL btree
	// declaration) because UI search and DB indexing are independent
	// concerns.
	Searchable bool `json:"searchable,omitempty"`

	// Validation declares server-side constraints applied before write.
	// Pointer so the zero value is "no validation" rather than an empty
	// rule that would still be evaluated.
	Validation *ValidationRule `json:"validation,omitempty"`

	// Widget hints which input/display component the UI should render. When
	// empty the host infers it from Type (e.g. "text" for string, "number"
	// for int). The whitelist lives in validate.go (validWidgets).
	Widget string `json:"widget,omitempty"`

	// Display hints — pure table/cell rendering metadata projected onto the
	// served modelbase.ColumnDef. They never touch the DDL plane. They exist
	// on the legacy struct only as a carrier so the v3 Column display hints
	// survive the v3 → legacy ModelDefinition conversion (mapModels) and the
	// subsequent derivation into modelbase.ColumnDef. All optional.
	//
	// CellStyle selects the SDK cell renderer (url, email, currency, creator,
	// status, badge, image, …). StyleConfig carries renderer options
	// (label_field, currency, decimals, base_path, …). Tooltip/Description are
	// paths to a primary text and a subtitle. BasePath is a URL/route prefix.
	CellStyle   string                 `json:"cellStyle,omitempty"`
	StyleConfig map[string]interface{} `json:"styleConfig,omitempty"`
	Tooltip     string                 `json:"tooltip,omitempty"`
	Description string                 `json:"description,omitempty"`
	BasePath    string                 `json:"basePath,omitempty"`

	// DependsOn and OptionsConfig forward the v3 dependent-picker declaration
	// (Column.depends_on + the `options` object form) so a dynamic_select column
	// scopes its candidates by a sibling field and resolves its label from a
	// related model. DependsOn names the sibling whose value supplies the cascade
	// filter_value; OptionsConfig carries the dynamic source declaration. The
	// host projects both onto the served modelbase.ColumnDef so they reach the
	// SDK and the OptionsConfigResolver. Pure UI/query metadata; never DDL.
	DependsOn     string             `json:"depends_on,omitempty"`
	OptionsConfig *DynamicOptionsDef `json:"optionsConfig,omitempty"`

	// VisibleWhen carries the v3 Column.visible_when conditional-visibility
	// predicate through the v3 → host conversion so form derivation
	// (DeriveFormFields) projects it onto the served modelbase.FieldDef and the
	// SDK shows/hides the field against the live form values. Pure UI metadata;
	// the DDL plane ignores it. Nil = always visible.
	VisibleWhen *VisibleWhenDef `json:"visible_when,omitempty"`

	// Section carries the v3 Column.section through the v3 → host conversion so
	// form derivation (DeriveFormFields) projects it onto the served
	// modelbase.FieldDef.Section and the SDK places the field inside the matching
	// form_layout section/step. Pure UI metadata; the DDL plane ignores it. Empty
	// = the implicit "General" block.
	Section string `json:"section,omitempty"`
}

// VisibleWhenDef is the legacy carrier for the v3 VisibleWhen block. It rides
// ColumnDef/FieldDef so the {field, equals, in} predicate survives the v3 →
// host conversion and lands on the served modelbase.VisibleWhen the SDK reads.
// JSON tags match the v3 + host contract so it round-trips byte-for-byte.
type VisibleWhenDef struct {
	Field  string   `json:"field"`
	Equals string   `json:"equals,omitempty"`
	In     []string `json:"in,omitempty"`
}

// ValidationRule expresses server-side input constraints. All fields are
// optional and combine additively: a value must satisfy every populated rule
// to pass. Min/Max apply to numeric values *and* to string/array length —
// the consuming validator decides which based on the column Type.
type ValidationRule struct {
	// Regex is a Go regexp/syntax pattern. When set on a non-string column
	// it is ignored. Validate compiles the pattern at manifest-load time so
	// malformed expressions are caught before install.
	Regex string `json:"regex,omitempty"`

	// Min is the inclusive lower bound. For numeric columns it bounds the
	// value; for string / array columns it bounds the length. Pointer so
	// 0 is distinguishable from "unset".
	Min *float64 `json:"min,omitempty"`

	// Max is the inclusive upper bound (same dual meaning as Min).
	Max *float64 `json:"max,omitempty"`

	// Custom names a server-side validator the addon registered with the
	// kernel (e.g. "rfc.tax_id"). Resolution and dispatch happens at write
	// time; Validate only checks the identifier shape here.
	Custom string `json:"custom,omitempty"`
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
