// broadcaster.go declares the seam through which the installer notifies the
// outside world when an installation's manifest hash changes. It is the
// kernel-side contract for the RFC-0001 D4 follow-up: keep the frontend's
// metadata-cache fresh without polling by pushing a tiny event over WebSocket
// the instant a hot-swap lands.
//
// Law 1 — services depend on interfaces, never structs. Law 3 — kernel
// packages must not import the WebSocket Hub (that's host-side glue). This
// file therefore declares a minimal interface here and provides a no-op
// default so:
//
//   - the kernel installer can call Broadcast unconditionally;
//   - hosts wire a real implementation (see bridge/installer_broadcaster.go);
//   - tests inject a fake without dragging ws.Hub into installer tests.
//
// The event shape is deliberately tiny: hosts have everything they need to
// fan out per-org WebSocket messages and the frontend has everything it needs
// to invalidate its addon-keyed metadata cache.
package installer

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// ManifestChangeEvent is the payload the installer hands the broadcaster
// whenever it observes a manifest-hash change at persist time. Field semantics:
//
//	OrgID     — organization the change is scoped to; hosts use it as the
//	            WebSocket routing key (only operators of this org receive it).
//	AddonKey  — manifest.Key of the addon whose hash flipped.
//	OldHash   — previous manifest hash. Empty on first install ("" signals
//	            "no prior record" so the frontend can decide whether to treat
//	            it as a fresh load vs. a hot-swap).
//	NewHash   — manifest hash now persisted. Always populated.
//	Version   — manifest.Version that landed; not required for cache busting
//	            but useful in observability dashboards.
//	Timestamp — host-clock instant the change was observed; sent verbatim so
//	            late-arriving WS messages can be reordered client-side if
//	            ever needed.
type ManifestChangeEvent struct {
	OrgID     uuid.UUID
	AddonKey  string
	OldHash   string
	NewHash   string
	Version   string
	Timestamp time.Time
}

// ManifestChangeBroadcaster is the abstract publisher the installer calls when
// it persists a new-or-different manifest hash. Implementations:
//
//   - bridge wires one backed by ws.Hub so the frontend gets the message;
//   - hosts that do not surface a real-time channel (CLI tools, tests) leave
//     the default NoopBroadcaster in place;
//   - tests substitute a recording fake to assert on call counts and fields.
//
// Broadcast MUST be non-fatal: a transient WebSocket failure should never
// roll back a successful install. The installer logs the error and continues.
// Implementations should therefore avoid blocking — push into a channel or
// fire-and-forget — so a slow consumer cannot stall install requests.
type ManifestChangeBroadcaster interface {
	Broadcast(ctx context.Context, evt ManifestChangeEvent) error
}

// NoopBroadcaster is the zero-cost default. It is exported so hosts that
// only sometimes want broadcasting can hand it in conditionally, and so
// tests can spell out the "no broadcasting expected" expectation.
type NoopBroadcaster struct{}

// Broadcast satisfies ManifestChangeBroadcaster by doing nothing. Returns
// nil to make sure consumers can chain it without error handling.
func (NoopBroadcaster) Broadcast(context.Context, ManifestChangeEvent) error { return nil }
