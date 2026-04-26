// tools.go projects manifest.Tools into the host's agent-tool store so an
// LLM-facing tool declared in a manifest becomes a first-class row the
// host's agent can invoke. This is the kernel-promoted version of a
// host-side SyncAddonTools that used to talk directly to a host's agent-tool
// model — now the host hides its row shape behind ToolStore and the bridge
// stays host-agnostic.
//
// Idempotency: SyncAddonTools loads the existing rows for (org, addon),
// upserts the manifest's tools, and lets the host drop orphans through
// DeleteByAddon (or a host-side diff inside its Upsert). The bridge does
// NOT delete on a per-tool basis — that's a host concern because hosts'
// row shapes carry foreign keys (agent_id, etc.) the bridge can't touch.
package bridge

import (
	"fmt"

	"github.com/asteby/metacore-kernel/manifest"
	"github.com/google/uuid"
)

// SyncAddonTools projects manifest.Tools into the host's ToolStore for the
// (orgID, addonKey) pair. A zero-Tool manifest is treated as "remove all"
// so the host's table is drained of stale rows when a new manifest version
// drops every tool it used to declare.
//
// The bridge:
//
//  1. Loads existing rows so the host can compute orphans on Upsert.
//  2. Calls Upsert with the manifest-derived Tool projections.
//  3. Calls DeleteByAddon for orphan rows when the new manifest is empty.
//
// Hosts that want stricter orphan handling can do it inside Upsert by
// diffing against the slice the bridge passes; ToolStore implementations
// retain full control over the row shape.
func SyncAddonTools(store ToolStore, orgID uuid.UUID, m manifest.Manifest) error {
	if store == nil {
		return fmt.Errorf("bridge.SyncAddonTools: nil ToolStore")
	}
	if m.Key == "" {
		return fmt.Errorf("bridge.SyncAddonTools: manifest missing Key")
	}
	if len(m.Tools) == 0 {
		return store.DeleteByAddon(orgID, m.Key)
	}
	tools := make([]Tool, 0, len(m.Tools))
	for _, t := range m.Tools {
		tools = append(tools, Tool{
			OrgID:    orgID,
			AddonKey: m.Key,
			Def:      t,
		})
	}
	return store.Upsert(tools)
}

// RemoveAddonTools deletes every row this bridge previously added for the
// (org, addon) pair. Called from Uninstall.
func RemoveAddonTools(store ToolStore, orgID uuid.UUID, addonKey string) error {
	if store == nil {
		return fmt.Errorf("bridge.RemoveAddonTools: nil ToolStore")
	}
	return store.DeleteByAddon(orgID, addonKey)
}
