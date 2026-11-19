package agent

import (
	"context"
	"fmt"
	"strings"

	"github.com/asteby/metacore-kernel/agent/contextmgr"
	"github.com/google/uuid"
)

// The ports below are how the kernel's show_ui/list_blocks/list_widgets tools
// talk to the HOST without importing it (Ops, a connector, a test). The kernel
// owns the contract (genui/v2 + the agent profile); the host owns what no
// library can know: who the user is, which models and widgets they may read,
// which addons are enabled, where documents are stored.

// UIIdentity is the identity of the conversation a tool call belongs to: the
// same org/user/scope the data tools act under. It is built from the
// conversation context, never from model-provided parameters.
type UIIdentity struct {
	OrganizationID uuid.UUID
	UserID         uuid.UUID
	ConversationID uuid.UUID
	// Scope is the active scope of the session (branch, organizational unit,
	// range...) as the host put it in ConversationContext.Metadata["scope"].
	// The kernel does not interpret it; it travels to the host's checks.
	Scope map[string]any
}

// ScopeMetadataKey is the ConversationContext.Metadata key a host uses to hand
// the session scope to the tools.
const ScopeMetadataKey = "scope"

// UIIdentityFrom builds the identity of a tool call. A nil context gives the
// zero identity, which a host's resolver must refuse.
func UIIdentityFrom(c *contextmgr.ConversationContext) UIIdentity {
	if c == nil {
		return UIIdentity{}
	}
	id := UIIdentity{OrganizationID: c.OrganizationID, UserID: c.UserID, ConversationID: c.ConversationID}
	switch s := c.Metadata[ScopeMetadataKey].(type) {
	case map[string]any:
		id.Scope = s
	case map[string]string:
		id.Scope = make(map[string]any, len(s))
		for k, v := range s {
			id.Scope[k] = v
		}
	}
	return id
}

// UISource is one non-inline data source of a document, as declared in
// data.<Name>: Kind is "q" | "widget" | "plan" and Value the q token, the
// widget key or the plan name.
type UISource struct {
	Name  string `json:"name"`
	Kind  string `json:"kind"`
	Value string `json:"value"`
}

// SourceDeniedError is what a SourceResolver returns when the user may not read
// some sources. Sources lists the denied source NAMES (data.<name>); the reason
// shown to the model must not reveal whether the model/widget exists.
type SourceDeniedError struct {
	Sources []string
}

func (e *SourceDeniedError) Error() string {
	return fmt.Sprintf("fuentes no autorizadas o inexistentes para este usuario: %s", strings.Join(e.Sources, ", "))
}

// SourceResolver authorizes the sources of a document against the identity of
// the conversation. It is called for EVERY non-inline source before the
// document reaches the user. Return nil only when the identity may read all of
// them (the `<table>.index` permission of each q model, the visibility of each
// widget, the active scope); return *SourceDeniedError otherwise. Any other
// error is an infrastructure failure and the document is not shown either:
// the tool fails closed.
type SourceResolver interface {
	AuthorizeSources(ctx context.Context, id UIIdentity, sources []UISource) error
}

// BlockInfo describes a named block the user can reference with
// {type:"ref", ref:Ref} (contributions.blocks[] of an enabled addon).
type BlockInfo struct {
	// Ref is the reference to put in the document: "<addon>.<key>".
	Ref         string `json:"ref"`
	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`
	// Permission is informational: the host's ExpandRefs applies it.
	Permission string `json:"permission,omitempty"`
}

// WidgetInfo describes a dashboard widget the user can read from a document as
// data.<name> = {widget: Key}.
type WidgetInfo struct {
	// Key is "<addon>.dashboard.<key>".
	Key         string `json:"key"`
	Title       string `json:"title,omitempty"`
	Kind        string `json:"kind,omitempty"`
	Description string `json:"description,omitempty"`
}

// UICatalog is the read-only catalog of reusable pieces visible to the
// identity, and the expansion of references into the blocks they stand for.
// The model COMPOSES with these (list_blocks/list_widgets) instead of
// reinventing a chart or a table that already exists.
type UICatalog interface {
	// ListBlocks returns the named blocks the identity may reference.
	ListBlocks(ctx context.Context, id UIIdentity) ([]BlockInfo, error)
	// ListWidgets returns the dashboard widgets the identity may read.
	ListWidgets(ctx context.Context, id UIIdentity) ([]WidgetInfo, error)
	// ExpandRefs resolves the {type:"ref"} nodes of doc against the catalog the
	// identity may use and returns the expanded document (a new value; doc is
	// not modified). Hosts implement it with manifestv3.ExpandDocBlockRefs
	// over their (already filtered) catalog. A reference outside the catalog
	// must be an error: refs never grant access to what the user cannot see.
	ExpandRefs(ctx context.Context, id UIIdentity, doc map[string]any) (map[string]any, error)
}

// UIAuditEntry is what the host persists about a document the user was shown
// (or that was rejected). The document itself is Doc: where it is stored and
// for how long is the host's retention policy.
type UIAuditEntry struct {
	// Accepted is false for a rejected attempt (Code/Path say why).
	Accepted bool
	Code     string
	Path     string
	// DocHash is the sha256 (hex) of the canonical JSON of the validated
	// document, after reference expansion. Empty for a rejection that never
	// parsed.
	DocHash string
	// Doc is the validated, expanded document (nil when rejected).
	Doc map[string]any
	// Sources are the non-inline sources the document reads, as authorized.
	Sources []UISource
	// InlineSources are the names the model declared as inline data.
	InlineSources []string
	// Blocks is the count of blocks by type.
	Blocks map[string]int
	// Profile is the validation profile name ("agent").
	Profile string
}

// UIAuditor persists and audits every document. For an accepted document a
// failure to record is a failure to show: the tool returns the error and the
// user sees nothing it cannot account for. A rejection is recorded best-effort.
type UIAuditor interface {
	RecordUI(ctx context.Context, id UIIdentity, e UIAuditEntry) error
}

// UIAuthorizer is the whole host port in one interface, for hosts that
// implement it in one place (Ops' conversation service). Hosts that split the
// pieces use UIHost directly.
type UIAuthorizer interface {
	SourceResolver
	UICatalog
	UIAuditor
}

// UIHost is the host's wiring of the ports. Sources is required for any
// document that reads a source (nil refuses them all: fail closed); Catalog is
// required for refs and for the list tools; Audit is optional only in
// development and tests (nil skips auditing).
type UIHost struct {
	Sources SourceResolver
	Catalog UICatalog
	Audit   UIAuditor
}

// UIHostFrom wires one UIAuthorizer into a UIHost.
func UIHostFrom(a UIAuthorizer) UIHost { return UIHost{Sources: a, Catalog: a, Audit: a} }
