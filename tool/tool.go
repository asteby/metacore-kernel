package tool

import (
	"context"

	"github.com/asteby/metacore-kernel/manifest"
	"github.com/google/uuid"
)

// Tool is the runtime face of a manifest.ToolDef installed on a host.
//
// Hosts with conversational AI (link) register these into their agent-tool
// registry so a user message can trigger them. Hosts with action-triggered
// UI (ops) also use this contract so a click in the UI resolves to the same
// dispatch path.
type Tool interface {
	// ID returns the addon-scoped tool identifier (ToolDef.ID).
	ID() string
	// AddonKey is the manifest.Manifest.Key that declares this tool.
	AddonKey() string
	// Def is the original declaration — immutable for the tool's lifetime.
	Def() manifest.ToolDef
	// Execute runs the tool with caller-supplied params. Implementations
	// normalize+validate against ToolInputParam rules before dispatch.
	Execute(ctx context.Context, params map[string]any) (Result, error)
}

// Result is what a tool returns to the caller. Hosts decide how to render it.
type Result struct {
	Success  bool           `json:"success"`
	Data     any            `json:"data,omitempty"`
	Error    string         `json:"error,omitempty"`
	Metadata map[string]any `json:"metadata,omitempty"`
}

// Installation is the minimal context the kernel needs to bind a ToolDef to a
// real tenant + addon install. Hosts populate it when hydrating the Registry.
type Installation struct {
	// ID is the installer.Installation UUID — used as the signing key index.
	ID uuid.UUID
	// OrgID (tenant) owns the install.
	OrgID uuid.UUID
	// AddonKey identifies which addon this install belongs to.
	AddonKey string
	// BaseURL resolves relative endpoints declared in ToolDef.Endpoint.
	// Empty means "endpoint must be absolute".
	BaseURL string
}
