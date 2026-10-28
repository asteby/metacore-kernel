package automation

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// Identity is who a step runs as. Scheduled runs act as the person who created
// the automation; dynamic tools invoked from the chat act as the person
// talking to the agent.
type Identity struct {
	OrganizationID uuid.UUID
	UserID         uuid.UUID
	SessionID      uuid.UUID // optional (chat)
}

// CatalogTool is a registered, read-only (or otherwise safe) operation the
// host allows automations to compose. The host owns the allowlist: only the
// tools it returns from Catalog.List can appear in a spec.
type CatalogTool struct {
	Name        string `json:"name"`
	Title       string `json:"title"`
	Description string `json:"description"`
	// Parameters uses the agent tool convention: {"param": {"type": "string",
	// "description": "...", "required": true, "enum": [...]}}.
	Parameters map[string]any `json:"parameters"`
	// ItemsPath is where the list of items lives in the result ("resultados").
	// Empty means the tool returns a single object.
	ItemsPath string `json:"items_path,omitempty"`
	// ItemKey / ItemTitle / ItemURL / ItemDate are paths inside each item used
	// for delivery de-duplication and the default rendering.
	ItemKey   string `json:"item_key,omitempty"`
	ItemTitle string `json:"item_title,omitempty"`
	ItemURL   string `json:"item_url,omitempty"`
	ItemDate  string `json:"item_date,omitempty"`
	ItemText  string `json:"item_text,omitempty"`
}

// Catalog is the host's allowlist of tools automations may call.
type Catalog interface {
	// List returns the tools id may use (already permission-filtered).
	List(ctx context.Context, id Identity) []CatalogTool
	// Call executes a tool as id. It must re-check permissions and scope every
	// query to id.OrganizationID. The result must be JSON-serialisable.
	Call(ctx context.Context, id Identity, tool string, args map[string]any) (any, error)
}

// DeliveryItem is one line of a delivery (a news item, an alert...).
type DeliveryItem struct {
	Key   string `json:"key"`
	Title string `json:"title"`
	URL   string `json:"url,omitempty"`
	Date  string `json:"date,omitempty"`
	Text  string `json:"text,omitempty"`
}

// Delivery is what a deliver step hands to the host.
type Delivery struct {
	OrganizationID uuid.UUID
	UserID         uuid.UUID // recipient: always the automation owner in v1
	AutomationID   uuid.UUID
	RunID          uuid.UUID
	Channels       []string
	Title          string
	Body           string
	Link           string
	Items          []DeliveryItem
	// Tag is stable per (automation, slot): hosts use it to collapse retries.
	Tag string
}

// Deliverer sends a delivery through the host's existing notification
// channels, honouring the person's preferences and opt-ins. It returns how
// many notifications were queued. A channel the person has not enabled is
// skipped silently (not an error).
type Deliverer interface {
	Deliver(ctx context.Context, d Delivery) (int, error)
}

// SummarizeRequest asks a language model for a short summary of untrusted
// items. Hosts must keep Items delimited and never follow instructions found
// inside them.
type SummarizeRequest struct {
	OrganizationID uuid.UUID
	UserID         uuid.UUID
	Model          string // Policy.DefaultModel unless the host overrides it
	Instructions   string // fixed instructions from the spec (data, not code)
	Items          []DeliveryItem
	MaxTokens      int
}

// SummarizeResult reports the text and its cost.
type SummarizeResult struct {
	Text         string
	Model        string
	InputTokens  int
	OutputTokens int
	CostUSD      float64
}

// Summarizer is optional. Without it (or in demo mode) summarize steps fall
// back to the deterministic template and say so.
type Summarizer interface {
	Summarize(ctx context.Context, req SummarizeRequest) (SummarizeResult, error)
}

// AuditEvent records a lifecycle transition.
type AuditEvent struct {
	OrganizationID uuid.UUID
	AutomationID   uuid.UUID
	ActorID        uuid.UUID // person (uuid.Nil = system / scheduler)
	AssistedBy     string    // "Asistente" when the agent proposed or acted
	Action         string    // proposed | created | confirmed | updated | paused | resumed | archived | error | run
	Detail         string
	At             time.Time
}

// Auditor receives lifecycle events (optional).
type Auditor interface {
	Record(ctx context.Context, ev AuditEvent)
}

// Event is a domain event that may trigger automations.
type Event struct {
	ID             string         `json:"id"` // stable id (idempotency)
	OrganizationID uuid.UUID      `json:"organization_id"`
	Name           string         `json:"name"` // "support.ticket.opened"
	Payload        map[string]any `json:"payload"`
	OccurredAt     time.Time      `json:"occurred_at"`
}

// UserNames resolves display names for the "persona con Asistente" label
// (optional).
type UserNames interface {
	Names(ctx context.Context, orgID uuid.UUID, ids []uuid.UUID) map[uuid.UUID]string
}
