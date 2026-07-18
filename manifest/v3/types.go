// Package v3 implements the Module Contract v3 manifest types and
// validator used by the metacore kernel installer.
//
// The package is intentionally decoupled from manifest (the v2 types) so
// the kernel can dual-read both versions during the 3.x release line. The
// authoritative grammar is the JSON schema in
// docs/spec/v3/manifest-v3.schema.json; the Go structs below mirror it
// 1:1 so json.Unmarshal followed by Validate is a faithful round trip.
package v3

import (
	"encoding/json"
	"fmt"
)

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

	// Connectors declares the third-party credential providers this addon needs
	// (e.g. "github"). Unlike the standalone kind:ConnectorPack manifest, these
	// ride embedded in a normal Addon: the host stores their Credentials per org
	// (encrypted) and the connectors runtime resolves them for wasm handlers
	// (connector_get) and for a webhook's secret_ref. Empty = no connectors.
	Connectors []Connector `json:"connectors,omitempty"`

	// Schedules declares declarative cron jobs the kernel scheduler fires per
	// org: every Schedule.Every it dispatches Schedule.Do (a wasm:/webhook:/
	// compiled: handler). The webhook-primary + cron-reconcile pattern uses this
	// as the backstop that reconciles what an inbound webhook missed. Empty = none.
	Schedules []Schedule `json:"schedules,omitempty"`

	// Webhooks declares inbound webhook routes the host mounts and the kernel
	// webhookin receiver routes: a verified request to InboundWebhook.Path
	// dispatches InboundWebhook.Do. Signature is checked per Verify, the secret
	// resolved from SecretRef (a connector credential). Empty = no webhooks.
	Webhooks []InboundWebhook `json:"webhooks,omitempty"`

	Signature *Signature `json:"signature,omitempty"`
}

// Connector declares a third-party credential provider an addon depends on. The
// host (ops) collects + encrypts the Credentials per org and the connectors
// runtime resolves them; wasm handlers read a credential via connector_get
// (gated by the connector:read capability) and a webhook's secret_ref resolves
// to one of these credentials.
type Connector struct {
	// Key identifies the connector (e.g. "github"); connector_get(key) and a
	// webhook's secret_ref ("<key>.<credential>") reference it.
	Key string `json:"key"`
	// Label is the human/i18n name shown in the authorization UI.
	Label string `json:"label,omitempty"`
	// Auth is the credential-acquisition flow: "oauth2" | "token".
	Auth string `json:"auth,omitempty"`
	// Credentials enumerates the fields the org supplies (an access_token,
	// repo, webhook_secret …). Reuses the Setting shape (key/type/default/
	// required/validation); "secret"-typed fields are stored encrypted.
	Credentials []Setting `json:"credentials,omitempty"`
}

// Schedule is one declarative cron job. The kernel scheduler parses Every as a
// Go duration ("30s","5m","1h") and, per installed org, fires Do on that
// interval. Do references the same dispatchable handler set as a transition
// hook: "wasm:<export>" | "webhook:<key>" | "compiled:<fn>".
type Schedule struct {
	// Key identifies the schedule within the addon (e.g. "sync_issues").
	Key string `json:"key"`
	// Every is the Go duration between fires (time.ParseDuration).
	Every string `json:"every"`
	// Do is the handler reference dispatched on each tick.
	Do string `json:"do"`
}

// InboundWebhook declares a webhook route the host mounts under the addon+org
// namespace. On a verified request the kernel webhookin receiver routes the
// body to Do. Verify selects the signature scheme; SecretRef names the connector
// credential holding the signing secret.
type InboundWebhook struct {
	// Key identifies the webhook within the addon (e.g. "github_push").
	Key string `json:"key"`
	// Path is the route mounted under the addon+org namespace (e.g.
	// "/webhooks/github").
	Path string `json:"path"`
	// Verify is the signature scheme: "" (none) | "hmac-sha256".
	Verify string `json:"verify,omitempty"`
	// SecretRef resolves the signing secret as "<connector>.<credential>"
	// (e.g. "github.webhook_secret"). Required when Verify is set.
	SecretRef string `json:"secret_ref,omitempty"`
	// Do is the handler reference the verified body is routed to.
	Do string `json:"do"`
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
	// Countries scopes the addon to specific markets as ISO 3166-1 alpha-2
	// codes (e.g. ["MX"]). Region-specific addons (fiscal complements like
	// Carta Porte / SAT) declare their countries so the catalog can hide them
	// from users elsewhere. Empty/omitted means GLOBAL — available in every
	// country (the default for general addons).
	Countries []string `json:"countries,omitempty"`
	// I18n carries marketplace catalog localizations keyed by locale
	// ("es", "en", …). Distinct from the top-level Manifest.I18n, which
	// points at app-UI string bundles. The top-level Name/Description/Features
	// stay the default/fallback copy; the hub serves the localized variant
	// when the catalog is browsed in that locale, falling back to the default.
	I18n map[string]MetadataLocale `json:"i18n,omitempty"`
}

// MetadataLocale is one locale's catalog copy. Every field is optional so an
// author can localize just the description, and the renderer falls back to the
// default Metadata field for anything omitted.
type MetadataLocale struct {
	Name        string   `json:"name,omitempty"`
	Description string   `json:"description,omitempty"`
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

	// Relations declares the INVERSE edges of this model — the child records a
	// detail page should be able to list under it (e.g. a Customer's vehicles,
	// addresses and attachments). Unlike ForeignKeys (which declare the physical
	// FK constraints carried by the OWNING column) Relations are owner-rooted
	// view hints the host projects into served TableMetadata so the SDK can
	// render a "related records" panel with zero per-app wiring. Optional —
	// flat models omit it and keep the legacy behaviour. See ModelRelation for
	// the supported shapes (including polymorphic children via Scope).
	Relations []ModelRelation `json:"relations,omitempty"`

	// Seed declares DEFAULT seed data the installer inserts on install,
	// analogous to how it runs migrations — but declarative and idempotent.
	// On install the host upserts/skips each row keyed by Seed.Key (a natural
	// key column) scoped to the installing org, so re-installs and upgrades do
	// not duplicate rows. Optional — models without seed data omit it and are
	// unaffected. See Seed for the shape.
	Seed *Seed `json:"seed,omitempty"`

	// Formulas declare columns whose value is COMPUTED from other columns on
	// the SAME row via a pure arithmetic expression, evaluated by the kernel
	// before every create/update write (Tier-2 of the declarative compute
	// engine). Optional — models without computed columns omit it. See Formula
	// for the shape and the security contract on Expr.
	Formulas []Formula `json:"formulas,omitempty"`

	// Sequences declare atomic, gap-tolerant counters the kernel maintains per
	// org (or per branch) for this model — the primitive behind invoice folios,
	// remission numbers, service-order tickets, etc. Each Sequence has a Key, a
	// Scope (org|branch) and a Format template ("A-{seq:06}"). A Column may bind
	// to a sequence via Column.Sequence so the next value is stamped
	// automatically on create; a wasm handler can also pull one via the
	// `sequence_next` host import. Optional — models without folios omit it.
	Sequences []Sequence `json:"sequences,omitempty"`

	// Locking selects the row-locking strategy the kernel applies on Update.
	//   ""     — no extra locking (default; the legacy behaviour).
	//   "row"  — the kernel wraps the whole Update in a transaction and loads
	//            the target row with SELECT … FOR UPDATE before evaluating the
	//            column Constraints, so an increment-then-check guard (e.g.
	//            "quantity >= 0" after a concurrent decrement) is race-free: two
	//            transactions touching the same row serialise instead of both
	//            reading a stale value. Only meaningful together with column
	//            Constraints; harmless otherwise.
	Locking string `json:"locking,omitempty"`

	// StageField names the column that carries the model's pipeline stage
	// (e.g. "stage"). Declaring it — together with Stages — turns the model
	// into a stage machine: the kernel derives a `status` display type for the
	// column from Stages (colour/label/order, no separate `options` needed) and,
	// on every Update that changes the column, validates the move against
	// Transitions and fires the matching OnTransition hooks. Empty = the model
	// has no stage machine (the default; flat models are unaffected).
	StageField string `json:"stage_field,omitempty"`

	// Stages enumerates the ordered lifecycle stages of the StageField column.
	// Each Stage supplies the value/label/colour the kernel projects onto the
	// derived `status` display. Empty = no stage machine (back-compat): the
	// Update path applies no transition restriction at all.
	Stages []Stage `json:"stages,omitempty"`

	// Transitions whitelists the allowed (from → to) stage moves. On an Update
	// that changes StageField, the kernel rejects a move that is not listed here
	// with HTTP 422 (invalid_transition). Only enforced when Stages is non-empty.
	Transitions []Transition `json:"transitions,omitempty"`

	// OnTransition declares Bitrix-style hooks the kernel fires AFTER a valid
	// stage move is persisted and BEFORE the canonical event. Each hook matches a
	// (from, to) pair ("*" = wildcard) and dispatches Do (a `wasm:`/`webhook:`/
	// `compiled:` handler reference) with the {before, after, actor, org} payload.
	OnTransition []TransitionHook `json:"on_transition,omitempty"`
}

// Stage is one lifecycle stage of a model's StageField column. The kernel
// projects the set onto the column's derived `status` display (each Stage
// becoming a value/label option with a colour), ordered by Order, so a board
// or a status badge renders without a separate `options` declaration.
type Stage struct {
	// Key is the stored column value for this stage (e.g. "in_progress").
	Key string `json:"key"`
	// Label is the human/i18n label rendered for the stage.
	Label string `json:"label,omitempty"`
	// Color is a colour token (e.g. "slate", "blue", "green") for the badge.
	Color string `json:"color,omitempty"`
	// Order positions the stage left-to-right on a board / in a status select.
	Order int `json:"order,omitempty"`
	// IsFinal marks a terminal stage (no outgoing transitions expected).
	IsFinal bool `json:"is_final,omitempty"`
}

// Transition is one allowed move between two stages of a stage machine. From
// and To are Stage keys; the kernel rejects any Update that moves the
// StageField to a (from, to) pair not present in the model's Transitions.
type Transition struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// TransitionHook is a declarative side effect the kernel fires when a record
// transitions between stages. From/To select the matching moves ("*" matches
// any stage).
//
// A hook carries Set, Do, or both — at least one is required:
//
//   - Set assigns fields on the transitioning row itself, with no handler
//     (Bitrix-style "on entering this stage, stamp these fields"). It is applied
//     BEFORE the row is persisted, so the writes ride the same Save and appear in
//     the Issue.updated canonical event (subscribers see them). Value semantics:
//     a scalar is assigned directly; on a json/array column a "+tag" string
//     appends idempotently and a "-tag" string removes. When Set is present Do
//     becomes optional; when both are present Set is applied before Do dispatches.
//   - Do references the handler to dispatch as
//     `wasm:<export>` | `webhook:<key>` | `compiled:<fn>`.
//
// When Required is true a Do dispatch failure rolls back the whole transition
// (HTTP 422); otherwise the failure is logged and the transition stands.
type TransitionHook struct {
	From     string         `json:"from"`
	To       string         `json:"to"`
	Set      map[string]any `json:"set,omitempty"`
	Do       string         `json:"do,omitempty"`
	Required bool           `json:"required,omitempty"`
}

// Formula declares a column on the owning model whose value the kernel
// computes from a pure arithmetic expression over the SAME row's other
// columns, evaluated before every create/update DB write. This is Tier-2 of
// the declarative compute engine (e.g. SalesOrderItem.subtotal =
// quantity * unit_price - discount), so the parent's Tier-1 rollup sums
// already-correct values.
//
// SECURITY: Expr is parsed with a strict allowlist — only identifiers
// ([A-Za-z_][A-Za-z0-9_]*) that resolve to real columns on the model,
// decimal numbers, whitespace, the operators + - * / and parentheses. Any
// other character (quotes, semicolons, comments, function calls) is rejected
// at validation time.
type Formula struct {
	// Target is the column this formula writes. Must be a declared column on
	// the owning model.
	Target string `json:"target"`
	// Expr is the arithmetic expression, e.g. "quantity * unit_price - discount".
	// Required for the arithmetic tiers; MUST be empty when Tier is 3 (the wasm
	// handler replaces the expression entirely).
	Expr string `json:"expr,omitempty"`
	// Tier selects the compute engine for this formula:
	//   0 / 2 — the default pure-arithmetic Tier-2 evaluator over Expr.
	//   3     — a wasm handler computes the value: the kernel invokes Handler
	//           with the (merged) row as payload and writes the returned value
	//           into Target. Use it when the computation needs data or logic an
	//           arithmetic expression cannot express (price-list resolution,
	//           tiered margins, rounding policies).
	Tier int `json:"tier,omitempty"`
	// Handler is the Tier-3 backend, "wasm:<export>" — an export the addon's
	// wasm module declares. Required (and only allowed) when Tier is 3. The
	// invocation is host-wired (dynamic.HookRegistry.SetFormulaInvoker); when no
	// invoker is configured the formula is skipped, never a hard failure.
	Handler string `json:"handler,omitempty"`
}

// Seed is a model's declarative default-data block. The installer inserts Rows
// on install, treating Key as the natural key for idempotency: a row is only
// inserted when no existing row (for the installing org) already carries that
// Key value. Key MUST name a declared column on the owning model; each entry in
// Rows is an object mapping column name → value.
type Seed struct {
	// Key is the column used for idempotency — the natural key the installer
	// matches on to decide whether a row already exists (upsert/skip).
	Key string `json:"key"`
	// Rows are the default records, each an object of column name → value.
	Rows []map[string]any `json:"rows"`
}

// ModelRelation is one inverse 1:N / N:M edge rooted at the owning Model. The
// host projects it into modelbase.TableMetadata.Relations so a detail page can
// list the owner's child records.
//
//	Kind = "one_to_many"  — the owner has many rows on Through; ForeignKey is
//	                        the column on Through pointing back at the owner.
//	Kind = "many_to_many" — Through is the target model joined to the owner;
//	                        ForeignKey is the join column pointing at the owner.
//
// Scope adds a static equality filter applied to the child query, which is what
// makes POLYMORPHIC children addressable: an Attachment table shared by many
// owners carries an `owner_model` discriminator, so a Customer's attachments
// relation declares Scope {"owner_model":"Customer"} alongside
// ForeignKey "owner_id". Empty Scope = no extra filter (the common case).
type ModelRelation struct {
	Name       string            `json:"name"`            // stable identifier, e.g. "vehicles"
	Kind       string            `json:"kind"`            // "one_to_many" | "many_to_many"
	Through    string            `json:"through"`         // child model key, e.g. "Vehicle"
	ForeignKey string            `json:"foreign_key"`     // child column pointing back, e.g. "customer_id"
	Scope      map[string]string `json:"scope,omitempty"` // static child filter for polymorphic children
	Label      string            `json:"label,omitempty"` // i18n key / human label for the panel

	// Readonly declares that the relation panel in the UI must not allow
	// creating/editing/deleting child rows. Used for relations pointing at an
	// append-only ledger (e.g. an inventory kardex). Optional; defaults false.
	Readonly bool `json:"readonly,omitempty"`

	// Rollups declare PARENT columns whose value is an aggregate over this
	// relation's child rows (Tier-1 of the declarative compute engine, akin to
	// a Salesforce Roll-Up Summary or an Odoo stored computed field). On every
	// child create/update/delete the kernel recomputes each rollup target on
	// the affected parent via a single UPDATE. Optional — relations without
	// aggregates omit it. Only meaningful for one_to_many. See Rollup.
	Rollups []Rollup `json:"rollups,omitempty"`
}

// Rollup declares a PARENT column whose value the kernel maintains as an
// aggregate (sum/count/avg/min/max) over the child rows of the relation it is
// attached to. This is Tier-1 of the declarative compute engine.
//
// Either From (a single child column) or Expr (a child arithmetic expression)
// supplies the aggregated value; fn=count ignores both. Exactly one of
// From/Expr may be set (count may omit both).
//
// SECURITY: Expr goes RAW into the aggregate SQL, so it is parsed with the
// same strict arithmetic allowlist as Formula.Expr (identifiers resolving to
// real child columns, decimal numbers, + - * / and parentheses only). Any
// other character is rejected at validation time, and every identifier is
// double-quoted when the SQL is built.
type Rollup struct {
	// Target is the PARENT column the aggregate is written to. Must be a
	// declared column on the model owning the relation.
	Target string `json:"target"`
	// Fn is the aggregate function: sum (default) | count | avg | min | max.
	Fn string `json:"fn,omitempty"`
	// From is a single child column to aggregate. Mutually exclusive with Expr.
	From string `json:"from,omitempty"`
	// Expr is a child arithmetic expression to aggregate, e.g.
	// "subtotal + tax_amount". Mutually exclusive with From.
	Expr string `json:"expr,omitempty"`
}

// Column is a single physical column declaration.
//
// Beyond the DDL plane (Name/Type/PrimaryKey/NotNull/Default) the struct also
// carries OPTIONAL display hints the host projects onto the served
// modelbase.ColumnDef so the SDK can render a column richly (a clickable URL,
// the creator's avatar, a currency, a status badge) without per-app wiring.
// These hints are pure UI metadata: they never touch the SQL/DDL plane — the
// installer ignores them exactly as it ignores Label and Comment.
type Column struct {
	Name       string      `json:"name"`
	Type       string      `json:"type"`
	PrimaryKey bool        `json:"primary_key,omitempty"`
	NotNull    bool        `json:"not_null,omitempty"`
	Default    interface{} `json:"default,omitempty"`
	Label      string      `json:"label,omitempty"`
	Comment    string      `json:"comment,omitempty"`

	// Display selects the cell renderer the SDK applies to the column's
	// values, mapping to modelbase.ColumnDef.CellStyle. Examples: url, email,
	// phone, currency, creator, status, badge, tags, color, code, percent,
	// image, boolean, date. Empty = inferred from name/type (see auto-detect
	// in dynamic.DeriveTableColumns) or the plain text fallback.
	Display string `json:"display,omitempty"`
	// Widget selects the FORM input the SDK renders for this column in the
	// create/edit modal, mapping to modelbase.FieldDef.Type/Widget. It is the
	// DDL-agnostic input plane (Display is the read-only cell plane): a column
	// stored as `text` can declare widget "image" or "upload" so a product
	// photo / logo / attachment renders a file picker (upload → store → save
	// the URL) instead of a raw text box. Other useful values: textarea,
	// select, email, url, date, number, boolean, dynamic_select. Empty = the
	// host infers the widget from the storage type plus the name heuristics in
	// dynamic.DeriveFormFields (which already promote logo/image/photo columns
	// to a picker). Pure UI metadata; ignored by the DDL plane.
	Widget string `json:"widget,omitempty"`
	// DisplayConfig carries renderer-specific options (maps to
	// modelbase.ColumnDef.StyleConfig). Common keys: label_field, url_field,
	// currency, decimals, base_path, new_tab, name_field, max_length.
	// A `base_path` here is additionally projected onto ColumnDef.BasePath.
	DisplayConfig map[string]interface{} `json:"display_config,omitempty"`
	// Tooltip is a path to the primary text rendered on hover/secondary slot
	// (e.g. the creator's name). Maps to modelbase.ColumnDef.Tooltip.
	Tooltip string `json:"tooltip,omitempty"`
	// Description is a path to a subtitle (e.g. the creator's email). Maps to
	// modelbase.ColumnDef.Description.
	Description string `json:"description,omitempty"`

	// Ref turns the column into a FOREIGN-KEY picker: it names the target
	// model key or table (e.g. "Customer" or "addon_customers") the column
	// points at. The host projects it onto modelbase.ColumnDef.Ref /
	// modelbase.FieldDef.Ref so the SDK renders a searchable dynamic_select
	// in the native create/edit form (resolving options against
	// `/api/options/:Ref`) — no per-app action required. It is the explicit,
	// declarative twin of the host's belongs_to auto-derivation: an author
	// states the relation directly on the column instead of relying on name
	// heuristics. Pure UI metadata; the DDL/install plane ignores it.
	Ref string `json:"ref,omitempty"`
	// Options declares a STATIC select for the column: a fixed value/label
	// choice list (with optional icon/color/image visuals per FieldOption,
	// from #127) the host projects onto modelbase.ColumnDef.Options /
	// modelbase.FieldDef.Options so the SDK renders a plain select in the
	// native create/edit form — again with no custom action. Use Ref for a
	// relation picker and Options for a hardcoded enum (status, type, …).
	// Both are pure UI metadata and never touch the SQL/DDL plane.
	//
	// Options may ALSO be the OBJECT form (DynamicOptions): a dependent-picker
	// declaration {source, filter_by, value, label_ref, description} that the
	// host projects onto modelbase.FieldOptionsConfig so a dynamic_select column
	// scopes its candidates by a sibling field and resolves its label from a
	// related model. The array form (static enum) and object form (dynamic
	// source) are mutually exclusive on one column.
	Options FieldOptions `json:"options,omitempty"`
	// DependsOn names a SIBLING column whose current value supplies the cascade
	// `filter_value` for this column's dependent picker (used with the
	// Options object form). The host projects it onto modelbase.ColumnDef.DependsOn
	// (json `depends_on`) so the SDK re-fetches the options whenever the
	// depended-on field changes. Empty = no cascade (lists everything). Optional.
	DependsOn string `json:"depends_on,omitempty"`
	// OptionsSource declares a DYNAMIC select for the column: instead of a
	// hardcoded Options list, it names a PROVIDER key (e.g.
	// "registered_models", "installed_addons") the HOST registers and resolves
	// at metadata-serve time, materialising the localized value/label list
	// onto the served modelbase Options. The kernel itself implements no
	// providers — it only carries the key through the v3 → host conversion
	// (manifest.ColumnDef.OptionsSource → modelbase OptionsSource) so any host
	// in the ecosystem can plug its own sources. Open enum (schema pattern
	// ^[a-z][a-z0-9_]*$): an unknown key simply yields no options on hosts
	// that don't register it. Use Ref for a row-level relation picker, Options
	// for a hardcoded enum, OptionsSource for host-computed lists. Pure UI
	// metadata; the DDL/install plane ignores it.
	OptionsSource string `json:"options_source,omitempty"`

	// Constraints declare GUARD predicates the kernel evaluates inside the
	// create/update transaction, BEFORE the row is written. Each is a boolean
	// comparison over the SAME row's columns (e.g. "quantity >= 0" or
	// "price >= cost"); a false result aborts the write with a 422 carrying the
	// ErrorKey. This is the declarative twin of a CHECK constraint that the
	// kernel enforces at the application layer so an addon gets stock-never-
	// negative / price-≥-cost without a wasm handler or raw db_exec. Combine with
	// the owning Model.Locking = "row" to make an increment-then-check guard
	// safe under concurrency (SELECT … FOR UPDATE). Optional; the SAME strict
	// arithmetic allowlist as Formula.Expr governs each side of the comparison.
	Constraints []Constraint `json:"constraints,omitempty"`

	// Sequence binds this column to one of the owning Model.Sequences by key: on
	// create, when the column is empty, the kernel stamps the next formatted
	// folio value automatically. The column is treated as system-generated (like
	// Readonly) for form derivation. Empty = the column carries no auto-folio.
	Sequence string `json:"sequence,omitempty"`

	// Generated declares a POSTGRES STORED GENERATED column: a pure arithmetic
	// expression over OTHER columns of the SAME row (e.g. "quantity - reserved")
	// that Postgres maintains on every write. The kernel emits it as
	// `GENERATED ALWAYS AS (<expr>) STORED`, so the value is path-independent and
	// SQL-native (filters/sorts/aggregates/exports in the database, no compute
	// hook or wasm handler). The expression is validated with the SAME strict
	// arithmetic allowlist as Formula.Expr (computeexpr): only +-*/, parentheses,
	// numbers, and identifiers resolving to sibling columns — no self-reference.
	// A STORED generated column is INCOMPATIBLE with Default and NotNull: the DDL
	// carries no DEFAULT and is never declared NOT NULL (Postgres rejects both on
	// a generated column). Empty = ordinary column. Optional.
	Generated string `json:"generated,omitempty"`

	// Readonly marks a SYSTEM-GENERATED column: a value the addon/host populates
	// (e.g. an id or number a remote API returns after a write), NOT something a
	// user types. It is pure UI-plane metadata that the host projects onto
	// modelbase.ColumnDef.Readonly / FieldDef.Readonly so DeriveFormFields
	// EXCLUDES the column from the create form and marks it read-only in edit;
	// the column still renders normally in tables/kanban/detail. The DDL and
	// write planes ignore it — an addon (or a compute rule) writes the value
	// server-side as usual. Use it for columns filled by an outbound sync, an
	// external id, or any value the user must never hand-edit.
	Readonly bool `json:"readonly,omitempty"`
}

// Sequence declares one atomic counter the kernel maintains for the owning
// model, formatted through a template. The counter increments atomically
// (UPDATE … RETURNING) so concurrent creates never collide on a folio.
type Sequence struct {
	// Key is the sequence identifier, unique within the model (e.g. "invoice").
	// A Column binds to it via Column.Sequence, and the wasm `sequence_next`
	// import addresses it by (model, key).
	Key string `json:"key"`
	// Scope selects the counter's partitioning: "org" (one counter per
	// organization, the default) or "branch" (one counter per branch, so each
	// branch has its own folio series). Empty = "org".
	Scope string `json:"scope,omitempty"`
	// Format is the template rendered around the numeric value. It MUST contain
	// exactly one `{seq}` or zero-padded `{seq:0N}` placeholder (e.g. "A-{seq:06}"
	// → "A-000042"). Any other text is emitted verbatim.
	Format string `json:"format"`
}

// Constraint is one declarative GUARD predicate on a column: a boolean
// comparison the kernel evaluates before every create/update write.
//
// SECURITY: Expr is a single comparison `<arith> <op> <arith>` where op is one
// of >= <= > < == != (and `=` as an alias for ==). Each side is parsed with the
// SAME strict arithmetic allowlist as Formula.Expr (identifiers resolving to
// real columns, decimal numbers, + - * / and parentheses only), so a constraint
// can never inject SQL — it is evaluated in Go against the row's numeric values.
type Constraint struct {
	// Expr is the guard predicate, e.g. "quantity >= 0" or "price >= cost".
	Expr string `json:"expr"`
	// ErrorKey is the i18n key / stable code surfaced to the client (422) when
	// the predicate is false, e.g. "stock.negative".
	ErrorKey string `json:"error_key"`
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

	// Dashboard contributes one or more widgets to the host's modular
	// dashboard. Two flavours coexist in the same grid: DECLARATIVE widgets
	// (every kind except "custom") carry a Query the host computes with the
	// kernel aggregation engine (org-scoped, soft-delete aware, permission-
	// gated) and the SDK paints with a built-in renderer — zero per-addon
	// code. FEDERATED widgets (kind "custom") reference an Expose from the
	// addon's frontend bundle so the addon ships a 100%-custom React widget,
	// placed and sized inside the same grid. Optional — addons with no
	// dashboard surface omit it. See DashboardWidget for the full contract.
	Dashboard []DashboardWidget `json:"dashboard,omitempty"`

	// Documents contributes printable document templates. Each entry binds a
	// bundle-relative HTML template to one of the addon's models; the host
	// hydrates the template with the record (and its child rows) and the org
	// branding, then renders it to PDF. Optional — addons with nothing to
	// print omit it. See DocumentDef for the full contract.
	Documents []DocumentDef `json:"documents,omitempty"`

	// Config declares the addon's configuration surface so hosts can render a
	// "Configure" affordance (e.g. in the installed-addons view). Optional —
	// an addon with nothing to configure omits it and hosts show nothing.
	Config *ConfigEntry `json:"config,omitempty"`
}

// ConfigEntry points at where an addon's configuration lives: a declarative
// model (the host renders its generic CRUD page, e.g. /m/<model>) or an
// addon-owned URL (a custom screen the addon's frontend ships). Exactly one of
// Model/URL should be set; Title optionally overrides the affordance label
// with an i18n key.
type ConfigEntry struct {
	Model string `json:"model,omitempty"`
	URL   string `json:"url,omitempty"`
	Title string `json:"title,omitempty"`
}

// DocumentDef is one printable document an addon contributes
// (contributions.documents[]). It binds a bundle-relative HTML template to a
// model so the host can render a per-record PDF (delivery notes, tickets,
// invoices) hydrated with the record, its line-items and the org branding.
//
// The host serves it at GET /api/data/:model/:id/documents/:key.pdf, gated by
// the model's read permission and scoped to the caller's organization. The
// template body uses {{record.<col>}}, {{org.branding.<field>}}, {{line_items}}
// and {{now}} tokens the host substitutes at render time.
type DocumentDef struct {
	// Key is the document's stable id, unique within the addon. It is the
	// ":key" path segment of the PDF route. Required, snake_case.
	Key string `json:"key"`
	// Model is the model key (from this addon's models or model extensions)
	// the document renders a record of. Required.
	Model string `json:"model"`
	// Template is the bundle-relative path to the HTML template, e.g.
	// "templates/remision.html". Required, must end in .html.
	Template string `json:"template"`
	// Paper is the page geometry: A4|letter|ticket80. ticket80 is an 80mm-wide
	// receipt roll for POS. Required.
	Paper string `json:"paper"`
	// Filename is an optional download filename (without the .pdf extension).
	// It may contain {{record.<col>}} tokens the host expands. When empty the
	// host falls back to "<key>-<id>".
	Filename string `json:"filename,omitempty"`
}

// DashboardWidget is one tile an addon contributes to the host's modular
// dashboard (manifest.contributions.dashboard[]). The host renders the union
// of every enabled addon's widgets, grouped by Group and ordered by Order,
// filtered by the viewer's permissions.
//
// Kind selects the shape. Every kind except "custom" is DECLARATIVE: the host
// computes the aggregate from Query with the kernel aggregation engine and the
// SDK paints it with a built-in renderer. Kind "custom" is FEDERATED: the host
// mounts the addon's own React component (named by Expose, from the addon's
// frontend bundle) into Slot, with the same Card chrome so it blends in.
type DashboardWidget struct {
	// Key is the widget's stable id, unique within the addon. The capability
	// gating it is "<addon>.dashboard.<key>". Required, snake_case.
	Key string `json:"key"`
	// Title is the i18n key for the widget heading. Required.
	Title string `json:"title"`
	// Subtitle is an optional i18n key shown under the title.
	Subtitle string `json:"subtitle,omitempty"`
	// Icon is an optional lucide slug rendered in the accent colour.
	Icon string `json:"icon,omitempty"`
	// Kind is the renderer selector: stat|bar|line|area|pie|donut|list|
	// progress|custom. Required.
	Kind string `json:"kind"`
	// Size is the grid span over a 4-column grid: sm(1)|md(2)|lg(3)|full(4).
	// Default sm.
	Size string `json:"size,omitempty"`
	// Group is the optional i18n key the widget is bucketed under. Default:
	// the addon's name.
	Group string `json:"group,omitempty"`
	// Order positions the widget within its group (ascending). Default 100.
	Order int `json:"order,omitempty"`
	// Accent is an optional colour token: emerald|sky|violet|amber|rose|slate.
	Accent string `json:"accent,omitempty"`
	// Format applies to the scalar value and series points:
	// number|currency|percent|compact. Default number.
	Format string `json:"format,omitempty"`
	// Permission optionally gates visibility. If empty and Query.Model is set
	// the host derives "<table>.index".
	Permission string `json:"permission,omitempty"`
	// Empty is an optional i18n key for the widget's empty state.
	Empty string `json:"empty,omitempty"`

	// Query is the aggregation spec for declarative widgets (every kind but
	// "custom"). Required for those kinds; ignored for "custom".
	Query *WidgetQuery `json:"query,omitempty"`
	// Compare optionally requests a delta vs the previous window (a "+14.2%"
	// chip). Requires Query.DateField + Query.Range.
	Compare *WidgetCompare `json:"compare,omitempty"`

	// Slot is the federation slot the host mounts a kind:"custom" widget into.
	// Default "dashboard.widgets". Ignored for declarative kinds.
	Slot string `json:"slot,omitempty"`
	// Expose is the federated module name from the addon's frontend bundle
	// (e.g. "./StockHeatmap") rendered for a kind:"custom" widget. Required for
	// "custom"; the addon must also declare a frontend block. Ignored otherwise.
	Expose string `json:"expose,omitempty"`
}

// WidgetQuery is the aggregation spec a declarative DashboardWidget carries.
// The host resolves Model to a logical table, then runs the kernel aggregation
// engine org-scoped and soft-delete aware.
type WidgetQuery struct {
	// Model is the model key of the SAME addon to aggregate over. Required.
	Model string `json:"model"`
	// Aggregate is the aggregate function: count|sum|avg|min|max. count
	// ignores Field. Default count.
	Aggregate string `json:"aggregate,omitempty"`
	// Field is the column to aggregate. Required unless Aggregate is count.
	Field string `json:"field,omitempty"`
	// Where is a filter map: equality by default, or {op: value} where op is
	// eq|neq|gt|gte|lt|lte|contains — the same operators the list builder uses.
	// Values are raw JSON so booleans/numbers/strings round-trip faithfully.
	Where map[string]json.RawMessage `json:"where,omitempty"`
	// GroupBy buckets the result by a column (bar/line/area/pie/donut/list).
	GroupBy string `json:"group_by,omitempty"`
	// LabelField names a column whose value labels each bucket. If it is a ref
	// (FK) the host resolves a human-readable name.
	LabelField string `json:"label_field,omitempty"`
	// DateField drives a time series for line/area widgets.
	DateField string `json:"date_field,omitempty"`
	// Interval buckets the time series: day|week|month. Used with DateField.
	Interval string `json:"interval,omitempty"`
	// Range is the time window: last_7_days|last_30_days|last_12_months|
	// this_month|this_year|this_day|all.
	Range string `json:"range,omitempty"`
	// Order sorts buckets by value: asc|desc.
	Order string `json:"order,omitempty"`
	// Limit caps the number of buckets (host clamps to 1..24).
	Limit int `json:"limit,omitempty"`
}

// WidgetCompare requests a delta computation against an earlier window.
type WidgetCompare struct {
	// To is the comparison window. "previous_period" compares against the
	// window immediately preceding Query.Range.
	To string `json:"to"`
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

	// Filter declares a static column→value filter the host applies when it
	// renders this entry's list view, so an addon can publish one nav entry per
	// status (e.g. {"status":"reception"} for an "En recepción" entry pointing
	// at the same model). The host AND-combines these with any runtime filters.
	// Empty/omitted means no filter (the default, unfiltered list).
	Filter map[string]string `json:"filter,omitempty"`

	// ViewType selects the renderer the SDK mounts for this entry's model.
	// "table" (the default when empty) renders the DynamicTable; "kanban"
	// renders a board grouped by GroupBy. The same model can have one table nav
	// and one kanban nav. Hosts that don't understand a value fall back to table.
	ViewType string `json:"view_type,omitempty"`

	// GroupBy names the column the kanban board groups its columns by (typically
	// the model's stage_field). Required when ViewType is "kanban"; ignored
	// otherwise. The host projects ViewType + GroupBy onto the served
	// TableMetadata so the SDK picks the renderer with zero per-app wiring.
	GroupBy string `json:"group_by,omitempty"`
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

	// Placement declares where the host surfaces the action's trigger.
	//   ""/"row" — a per-row action in the model's table (default; executes
	//              against the hovered record).
	//   "table"  — a toolbar button at the page level (no record context).
	//   "create" — a toolbar button that REPLACES the generic "create" button,
	//              for addons that ship a custom create experience (e.g. a
	//              journal entry with debit/credit lines). Opens with an empty
	//              record; the host suppresses its default create button.
	// Hosts that don't understand a value fall back to "row".
	Placement string `json:"placement,omitempty"`

	// Fields declares a declarative form the host renders in the action modal
	// before dispatching the handler. Optional — an action with no fields and
	// no modal is a plain one-click action.
	Fields []ActionField `json:"fields,omitempty"`

	// Steps splits the declarative action form into a MULTI-STEP WIZARD: the
	// SDK renders one page per step (title + its fields) with per-step
	// validation, and submits the union of all steps' values on finish —
	// exactly as if they had been declared flat in Fields. Mutually exclusive
	// with Fields (a wizard IS the form). Use it for guided flows a flat field
	// list serves poorly (a workshop checklist, a purchase reception); anything
	// richer belongs in a federated Modal.
	Steps []ActionStep `json:"steps,omitempty"`

	// Modal is the slot_kind of a custom federated modal component the host
	// mounts instead of (or alongside) the declarative form — for actions whose
	// UI is too rich for a flat field list (e.g. a checkout panel). It is the
	// same federation slot_kind addressing used elsewhere in the contract.
	Modal string `json:"modal,omitempty"`

	// Confirm asks the host to show a confirmation step before dispatching.
	Confirm bool `json:"confirm,omitempty"`
	// ConfirmMessage is the body shown in that confirmation step.
	ConfirmMessage string `json:"confirm_message,omitempty"`

	// RequiresState gates the action on the target record's `status` column:
	// the action is only valid (and the host only surfaces it) when the
	// record's status is one of these values. The kernel ENFORCES this at
	// dispatch — an action invoked against a record in a disallowed state is
	// rejected. Empty/omitted means no state gate (the action is always valid),
	// so existing actions are unaffected.
	RequiresState []string `json:"requires_state,omitempty"`

	// ModalWidth lets an addon size the action's modal explicitly: a CSS length
	// ("720px", "60rem") or a number (px). Empty = the host's default (compact
	// for a flat field list, roomy for line-items). The SDK reads it as
	// `action.modalWidth`. Lets a rich create/edit form declare a wider modal.
	ModalWidth string `json:"modal_width,omitempty"`

	// Idempotency makes the action REPLAY-SAFE. When set, the kernel keys a
	// stored response by (org, model, action, <the KeyField value read from the
	// invocation payload>) and returns the SAME response on any later invocation
	// carrying that key, WITHOUT dispatching the handler again. This is what lets
	// a payment capture or a fiscal stamp be retried over a flaky network without
	// double-charging / double-stamping. Optional — omit for ordinary actions.
	Idempotency *ActionIdempotency `json:"idempotency,omitempty"`
}

// ActionIdempotency declares the payload field that carries an action's
// idempotency key. The client supplies a stable, unique value in that field
// (a UUID it generates per logical attempt); the kernel replays the first
// response for any repeat of the same (org, model, action, key).
type ActionIdempotency struct {
	// KeyField names the field in the action PAYLOAD whose value is the
	// idempotency key (e.g. "request_id"). Required when idempotency is set;
	// an invocation that omits the field (empty key) is dispatched normally
	// with no replay guarantee.
	KeyField string `json:"key_field"`
}


// ActionStep is one page of a multi-step declarative action wizard (see
// Action.Steps): a title, an optional description, and the fields the SDK
// renders and validates on that page. Field semantics are identical to
// Action.Fields entries.
type ActionStep struct {
	// Title is the step's heading (or an i18n key resolving to it).
	Title string `json:"title"`
	// Description is optional helper text rendered under the title.
	Description string `json:"description,omitempty"`
	// Fields are the step's form fields — same contract as Action.Fields.
	Fields []ActionField `json:"fields"`
}

// ActionField is one input in an action modal's declarative form. It mirrors
// the SDK's ActionFieldDef (runtime-react/src/types.ts) 1:1 so the v3 field
// maps cleanly onto what dynamic-form / ActionModalDispatcher render.
type ActionField struct {
	Key            string           `json:"key"`
	Label          string           `json:"label,omitempty"`
	Type           string           `json:"type"`
	Required       bool             `json:"required,omitempty"`
	Options        FieldOptions     `json:"options,omitempty"`
	Default        any              `json:"default,omitempty"`
	Placeholder    string           `json:"placeholder,omitempty"`
	Widget         string           `json:"widget,omitempty"`
	Ref            string           `json:"ref,omitempty"`
	SearchEndpoint string           `json:"search_endpoint,omitempty"`
	Validation     *FieldValidation `json:"validation,omitempty"`

	// OptionsSource declares a DYNAMIC select for this action field: instead of
	// a hardcoded Options list, it names a PROVIDER key (e.g. "connector_repos",
	// "installed_addons") the HOST registers and resolves at metadata-serve time,
	// materialising the localized value/label list onto the served
	// modelbase.FieldDef.Options. It is the action-field twin of Column.OptionsSource
	// (both map onto modelbase FieldDef.OptionsSource); the kernel implements no
	// providers — it only carries the key through the v3 → host conversion so any
	// host can plug its own sources. Open enum (schema pattern ^[a-z][a-z0-9_]*$):
	// an unknown key simply yields no options on hosts that don't register it. Use
	// Ref for a relation picker, Options for a hardcoded enum, OptionsSource for
	// host-computed lists. Pure UI metadata.
	OptionsSource string `json:"options_source,omitempty"`

	// DependsOn names ANOTHER field in the same action form (a header field or a
	// sibling item-field) whose current value supplies this picker's cascade
	// `filter_value`. Used with the Options object form (DynamicOptions) on a
	// dynamic_select. The host projects it onto modelbase.FieldDef.DependsOn
	// (json `depends_on`) so the SDK scopes + re-fetches the picker's options
	// when the depended-on field changes. Empty = no cascade. Optional.
	DependsOn string `json:"depends_on,omitempty"`

	// ItemFields declares the columns of a repeatable line-items group. It is
	// set on a field with type "array" (the multi-row container — e.g. the
	// item rows of a "Recibir mercancía" modal, or the debit/credit lines of a
	// journal entry). Each ItemFields entry is itself an ActionField (the cell
	// widget for one column), so the structure is self-referential. The field's
	// value is an array of objects keyed by the ItemFields keys. The SDK
	// (dynamic-line-items) renders a row grid with add/remove controls.
	ItemFields []ActionField `json:"item_fields,omitempty"`

	// LockRows, set on a type:"array" line-items field, declares that its rows
	// are FIXED: the SDK (dynamic-line-items) hides the add-row and delete-row
	// controls so the user can only edit existing rows' cells (e.g. a receive
	// form prefilled from the ordered lines). Ignored on non-array fields.
	LockRows bool `json:"lock_rows,omitempty"`

	// Accept, MaxSize and StoragePath configure a field with type "upload" (a
	// file attachment input). They are declarative hints the SDK reads off the
	// served action metadata — the actual upload HANDLER lives in the host
	// (ops/SDK); this contract only lets authors DECLARE an upload field.
	//   Accept      — a comma-separated MIME / extension allow-list passed to the
	//                 file picker's `accept` attribute, e.g. "image/*,.pdf".
	//   MaxSize     — max accepted file size in BYTES (0 = host default).
	//   StoragePath — optional logical bucket / path prefix the host stores the
	//                 file under, e.g. "attachments/customers". Empty = host default.
	// All three are ignored on non-upload fields.
	Accept      string `json:"accept,omitempty"`
	MaxSize     int64  `json:"max_size,omitempty"`
	StoragePath string `json:"storage_path,omitempty"`

	// LabelImage, LabelIcon and LabelColor map a column of the REMOTE model a
	// dynamic_select (Ref / search_endpoint) resolves against to a visual the
	// SDK renders beside each option's label. They make a relation picker read
	// richly — a product thumbnail, a brand icon, a status colour — without
	// hardcoding per-option visuals (which Options[] covers for STATIC lists).
	// Each is the NAME of a field on the referenced model whose value the SDK
	// reads:
	//   LabelImage — a column holding an image URL (e.g. "image_url" → product
	//                photo / contact avatar / brand logo).
	//   LabelIcon  — a column holding an icon identifier.
	//   LabelColor — a column holding a CSS colour.
	// All optional and ignored on non-reference fields; existing pickers are
	// unaffected.
	LabelImage string `json:"label_image,omitempty"`
	LabelIcon  string `json:"label_icon,omitempty"`
	LabelColor string `json:"label_color,omitempty"`

	// Total, on an ItemFields column, flags it for summation in the line-items
	// footer. The SDK renders a totals row summing every numeric column marked
	// Total (e.g. debit and credit of a journal entry). Ignored on flat fields.
	Total bool `json:"total,omitempty"`

	// Balance declares an optional, generic balance constraint on a line-items
	// (type "array") field: the summed Total of one column must equal the summed
	// Total of another (e.g. Σdebit == Σcredit). The SDK shows a balanced /
	// out-of-balance indicator and blocks submit until the two sides match. It
	// is deliberately domain-agnostic — "debit"/"credit" are just the two column
	// keys to reconcile. Ignored when ItemFields is empty.
	Balance *FieldBalanceRule `json:"balance,omitempty"`
}

// FieldBalanceRule is a declarative reconciliation constraint between two summed
// columns of a line-items field. Generic by design: the SDK sums DebitColumn and
// CreditColumn across all rows and reports balanced only when they are equal
// (and, unless RequireNonzero is false, strictly positive). A journal entry sets
// {debit_column:"debit", credit_column:"credit"}, but any two reconcilable
// columns work.
type FieldBalanceRule struct {
	DebitColumn    string `json:"debit_column"`
	CreditColumn   string `json:"credit_column"`
	Message        string `json:"message,omitempty"`
	RequireNonzero *bool  `json:"require_nonzero,omitempty"`
}

// DynamicOptions is the OBJECT form of a field/column `options` block: instead
// of a static value/label list, it declares WHERE a dependent picker's choices
// come from and HOW to project each option. It is the kernel-side mirror of the
// dependent-options contract — a dynamic_select whose candidates are scoped by a
// sibling field (DependsOn) and whose label is resolved from a related model.
//
// All fields map onto modelbase.FieldOptionsConfig so the host's
// OptionsConfigResolver can build the runtime query with zero per-app glue:
//
//	Source      — the real table the candidates come from (scope + computed cols).
//	FilterBy    — column on Source compared to the cascade `filter_value`.
//	Value       — column on Source that becomes the option's value.
//	LabelRef    — related model (key/table) whose row, looked up by Value (a
//	              foreign id), supplies the human label. Optional: when empty the
//	              label is Value itself.
//	Description — column on Source projected onto option.description (e.g. qty).
//
// Generic by design — no domain model name is baked into the kernel; an addon
// names its own tables here.
type DynamicOptions struct {
	Source   string `json:"source,omitempty"`
	FilterBy string `json:"filter_by,omitempty"`
	Value    string `json:"value,omitempty"`
	// Label names the column ON the Source row used as the option's display
	// text (and the column the `q` typeahead + ordering match against). When
	// empty the projection defaults to "name". Use it for a SAME-source picker
	// whose label is a column on Source itself (e.g. a product picker labelled
	// by `name`); LabelRef is its cross-model twin for when the label lives on
	// ANOTHER model keyed by Value. Setting Label also fixes search/ordering,
	// which otherwise fall back to Value (the raw id) when no label is declared.
	Label       string `json:"label,omitempty"`
	LabelRef    string `json:"label_ref,omitempty"`
	Description string `json:"description,omitempty"`
}

// FieldOptions carries a field/column `options` declaration that is EITHER a
// static value/label list (the historical array form) OR a dynamic-source
// object (DynamicOptions). The two never coexist for one field — a static enum
// uses the array, a dependent picker uses the object — so a single json key
// (`options`) holds whichever shape the author wrote. UnmarshalJSON peeks the
// first significant byte (`[` vs `{`) to pick the branch; MarshalJSON re-emits
// the shape that was set so the value round-trips byte-compatibly. A nil/empty
// FieldOptions marshals to nothing (omitempty-friendly via len()/IsZero()).
type FieldOptions struct {
	Static  []FieldOption
	Dynamic *DynamicOptions
}

// Len reports the number of static options (0 for a dynamic or empty block).
// Lets existing call-sites that did `len(c.Options)` keep working.
func (o FieldOptions) Len() int { return len(o.Static) }

// IsZero reports whether neither shape is set, so the field can be omitted.
func (o FieldOptions) IsZero() bool { return len(o.Static) == 0 && o.Dynamic == nil }

// UnmarshalJSON decodes either the array (static) or object (dynamic) form.
func (o *FieldOptions) UnmarshalJSON(data []byte) error {
	trimmed := bytesTrimSpace(data)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		o.Static = nil
		o.Dynamic = nil
		return nil
	}
	switch trimmed[0] {
	case '[':
		var arr []FieldOption
		if err := json.Unmarshal(data, &arr); err != nil {
			return err
		}
		o.Static = arr
		o.Dynamic = nil
		return nil
	case '{':
		var dyn DynamicOptions
		if err := json.Unmarshal(data, &dyn); err != nil {
			return err
		}
		o.Dynamic = &dyn
		o.Static = nil
		return nil
	default:
		return fmt.Errorf("v3: options must be an array (static) or object (dynamic), got %q", string(trimmed[:1]))
	}
}

// MarshalJSON re-emits whichever shape is set.
func (o FieldOptions) MarshalJSON() ([]byte, error) {
	if o.Dynamic != nil {
		return json.Marshal(o.Dynamic)
	}
	if len(o.Static) > 0 {
		return json.Marshal(o.Static)
	}
	return []byte("null"), nil
}

// bytesTrimSpace trims leading/trailing JSON whitespace without importing
// bytes for a single call-site.
func bytesTrimSpace(b []byte) []byte {
	i := 0
	for i < len(b) && (b[i] == ' ' || b[i] == '\t' || b[i] == '\n' || b[i] == '\r') {
		i++
	}
	j := len(b)
	for j > i && (b[j-1] == ' ' || b[j-1] == '\t' || b[j-1] == '\n' || b[j-1] == '\r') {
		j--
	}
	return b[i:j]
}

// FieldOption is a value/label choice for select-typed action fields.
//
// Beyond {value,label} an option can carry OPTIONAL visual hints the SDK
// renders alongside the label so a static choice list reads richly (a coloured
// status dot, a brand icon, a thumbnail). They are generic, domain-agnostic
// presentation metadata — the runtime ignores them entirely; only the SDK
// option renderer reads them. All optional, so existing two-field options keep
// working unchanged.
//
//	Icon  — an icon identifier (lucide PascalCase or simple-icons slug) shown
//	        before the label.
//	Color — a CSS colour (hex / token) used for a dot / chip / text accent.
//	Image — a URL (or bundle-relative path) to a small image / avatar / logo
//	        rendered beside the label.
//	When  — an OPTIONAL cascade guard that gates THIS static option by the
//	        current value of a sibling enum field. When set, the option is only
//	        offered while the sibling's value satisfies the condition; the SDK
//	        hides the select entirely once no option applies. Omitted = the
//	        option always applies (retro-compatible with existing enums).
type FieldOption struct {
	Value string           `json:"value"`
	Label string           `json:"label"`
	Icon  string           `json:"icon,omitempty"`
	Color string           `json:"color,omitempty"`
	Image string           `json:"image,omitempty"`
	When  *OptionCondition `json:"when,omitempty"`
}

// OptionCondition gates a single STATIC FieldOption by the current value of a
// sibling enum field — the static twin of the relational cascade (DependsOn +
// the DynamicOptions object). It only ever appears inside the array form of
// `options`; the object form (DynamicOptions) already scopes via FilterBy.
//
//	Field — the sibling field whose value is evaluated. Optional: when empty the
//	        evaluation falls back to the container column/field's DependsOn, so an
//	        author can name the governing field once at the field level.
//	In    — the option applies when the sibling's value ∈ In (string compare).
//	NotIn — the option applies when the sibling's value ∉ NotIn.
//
// At least one of In / NotIn must be non-empty, and at least one of Field /
// container DependsOn must resolve a sibling — enforced by validation.
type OptionCondition struct {
	Field string   `json:"field,omitempty"`
	In    []string `json:"in,omitempty"`
	NotIn []string `json:"not_in,omitempty"`
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
	Comment string  `json:"comment,omitempty"` // author note; ignored by the runtime
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
	Key         string          `json:"key"`
	Type        string          `json:"type"`
	Label       string          `json:"label,omitempty"`
	Description string          `json:"description,omitempty"`
	Default     interface{}     `json:"default,omitempty"`
	Required    bool            `json:"required,omitempty"`
	Options     []SettingOption `json:"options,omitempty"`
	// Validation is an optional constraint hint (e.g. a regex or a named rule)
	// the host applies when collecting the value. Used by connector credentials.
	Validation string `json:"validation,omitempty"`
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
//
// Requires names other addon keys in the SAME preset that must be
// installed before this one. The preset resolver topologically sorts the
// addons on this field so the install loop respects ordering even when
// the preset author lists them out of order. Empty/nil = no constraint
// beyond the preset's declared manifest order. References to keys that
// are not part of the preset are reported as errors at resolve time —
// cross-preset dependencies are not modelled here (those are the host's
// concern, e.g. ops's marketplace catalog).
type PresetAddon struct {
	Key      string   `json:"key"`
	Version  string   `json:"version"`
	Optional bool     `json:"optional,omitempty"`
	Requires []string `json:"requires,omitempty"`
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
