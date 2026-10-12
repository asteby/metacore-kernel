package agent

import (
	"context"
	"encoding/json"
	"time"

	"github.com/asteby/metacore-kernel/vector"
	"github.com/google/uuid"
)

// PermissionChecker decides whether a user may run an action on a model. It is
// the only authorization seam of the runtime: tools call it before touching
// data on behalf of the human the copilot acts for.
type PermissionChecker interface {
	// Can reports whether userID, within orgID, holds a permission that covers
	// action ("view", "create", "update", "delete", ...) on model.
	Can(orgID, userID uuid.UUID, model, action string) bool
}

// ModuleInfo describes one module (addon) enabled for an organization.
type ModuleInfo struct {
	Key         string
	Name        string
	Description string
	// Capabilities are the higher-level, human-readable abilities the module
	// contributes (used to tell the model what the org can actually do).
	Capabilities []string
}

// ModuleGate is the single point where the runtime learns which optional
// modules exist for an organization, so it never offers or touches data that
// belongs to a module the org has not enabled.
type ModuleGate interface {
	// IsEnabled reports whether moduleKey is enabled for orgID. Implementations
	// decide their failure policy (ops fails open on lookup errors).
	IsEnabled(orgID uuid.UUID, moduleKey string) bool
	// OwnerOf returns the module key that owns model, or "" for core models.
	OwnerOf(model string) string
	// Enabled lists the modules enabled for orgID, for prompts and catalogs.
	Enabled(orgID uuid.UUID) ([]ModuleInfo, error)
}

// Personality carries the user-facing identity of the assistant.
type Personality struct {
	Name        string
	Description string
	// SystemPrompt is the host-authored persona preamble; the runtime appends
	// its own rules after it.
	SystemPrompt string
}

// BrandProvider resolves the assistant persona (name, mascot voice, extra
// instructions) for an organization, so white-label hosts can rebrand the
// copilot without forking the runtime.
type BrandProvider interface {
	PersonalityFor(orgID uuid.UUID) Personality
}

// ModelMeta is the host-neutral description of one data model the copilot can
// reach. The host fills it from whatever registry it has (compiled models,
// manifest-defined dynamic models); the runtime never sees the host's types.
type ModelMeta struct {
	// Key is the registry key the model is looked up by.
	Key string
	// Table is the SQL table ("" when the model is not table-backed). Keys
	// containing a dot are never valid tables.
	Table string
	// ModelKey is the declared model name when it differs from Key.
	ModelKey string
	// Label and Title are raw display strings; either may be an i18n key.
	Label string
	Title string
	// Columns are the column names exposed to the model for listing.
	Columns []string
	// SearchIn are the columns text search runs over.
	SearchIn []string
	// JSONColumns are jsonb/json columns (nested values must be JSON-encoded).
	JSONColumns []string
	// GeneratedColumns can never be written (GENERATED ALWAYS and the like).
	GeneratedColumns []string
	// VectorEnabled / VectorFields describe semantic-search support.
	VectorEnabled bool
	VectorFields  []string
	// Schema is the host-rendered payload describe_model returns (columns,
	// fields, search_in, filters...). Opaque to the runtime.
	Schema map[string]any
}

// ModelCatalog is the registry port behind CRUD, model-key resolution and
// activity labels. Implemented by the host over its model registry.
type ModelCatalog interface {
	// Lookup finds a model by registry key, table name or declared model key.
	Lookup(key string) (ModelMeta, bool)
	// All lists every registered model.
	All() []ModelMeta
	// AliasSource maps an alias key to the model it stands for.
	AliasSource(key string) (string, bool)
	// Hints are host-specific LLM aliases for a lower-cased token (for
	// example "cliente" -> ["customers", "Customer"]). May return nil.
	Hints(lower string) []string
	// Translate localizes an i18n key; it returns raw when there is no entry.
	Translate(raw string) string
	// NavIcon returns the lucide icon the host's navigation uses for a model
	// ("" when unknown).
	NavIcon(modelKey string) string
}

// SemanticIndex is the semantic-search port: similarity search over the
// records of a vectorizable model plus (re)indexing after writes. The
// kernel's vector.Pipeline implements it over pgvector.
type SemanticIndex interface {
	VectorSearch(modelKey, query string, orgID uuid.UUID, limit int) ([]vector.SearchResult, error)
	EnqueueRecord(modelKey string, recordID uuid.UUID)
}

// NamedPermissionChecker checks a permission by its literal name
// ("sales.create"). Used for permissions addons declare in their manifests.
type NamedPermissionChecker interface {
	HasPermission(orgID, userID uuid.UUID, permission string) (bool, error)
}

// DeclaredCapabilityProvider returns the higher-level capabilities (guides,
// workflows) the modules enabled for an org declare. The host reads them from
// its manifests; the runtime validates and permission-filters them.
type DeclaredCapabilityProvider interface {
	DeclaredCapabilities(orgID uuid.UUID) ([]CapabilityDescriptor, error)
}

// ChatModel is the LLM port the orchestrator loop talks to. The host adapts
// its provider stack (langchaingo, hub, claude direct runner...) to it.
type ChatModel interface {
	// ChatWithFunctions runs one turn and returns an OpenAI-shaped response
	// map (choices/message/tool_calls) or a plain {"content": ...} map.
	ChatWithFunctions(ctx context.Context, messages []map[string]interface{}, systemPrompt string, tools []map[string]interface{}) (map[string]interface{}, error)
}

// ConversationStore reads what the runtime needs from persisted
// conversations without knowing the host's tables.
type ConversationStore interface {
	// LastUserMessageMetadata returns the metadata blobs ({"attachments":[...]})
	// of the user's most recent messages in a thread, newest first.
	LastUserMessageMetadata(conversationID uuid.UUID, limit int) ([]json.RawMessage, error)
}

// ClockZone gives the organization's local time for "now" prompt clauses.
type ClockZone interface {
	// Now returns the current time in the org's/user's timezone.
	Now(orgID, userID uuid.UUID) time.Time
}
