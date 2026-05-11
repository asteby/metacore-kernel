// installer_broadcaster.go closes the loop opened by the installer's
// ManifestChangeBroadcaster interface: it wraps a *ws.Hub so a manifest
// hot-swap immediately fans out a JSON message to every WebSocket
// connection of every member of the affected org, with no extra wiring on
// the host side beyond a single line at boot.
//
// The kernel's installer package never imports ws (Law 1 — depend on
// interfaces, not on transports). Bridge is the host-side seam, so the
// ws.Hub dependency lives here.
//
// The host wires it like this:
//
//	hub := ws.NewHub()
//	go hub.Run()
//	br, _ := bridge.New(bridge.Config{DB: db})
//	br.Host().Installer.WithBroadcaster(
//	    bridge.NewWSManifestBroadcaster(hub, br.DB(), nil),
//	)
//
// The third argument is an optional UsersForOrgFn override; the default
// queries `users` for every active user_id in the org and fan-outs via
// hub.SendToUsers. Hosts whose user table lives elsewhere supply their own
// resolver and the bridge stays decoupled from the host's user model.
package bridge

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/asteby/metacore-kernel/installer"
	"github.com/asteby/metacore-kernel/obs"
	"github.com/asteby/metacore-kernel/ws"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// WSManifestChangedType is the ws.MessageType the broadcaster emits. Frontends
// subscribe to this constant to know when to evict their per-addon
// metadata-cache entry and reload manifests. It is exported so consumers can
// match against it without re-declaring a literal.
const WSManifestChangedType ws.MessageType = "ADDON_MANIFEST_CHANGED"

// UsersForOrgFn resolves the set of user IDs that should receive a manifest-
// change message scoped to orgID. Returning an empty slice is valid (e.g.
// no operators online) — the hub call becomes a cheap no-op. Returning an
// error makes the broadcaster log and drop the event; the install itself
// still succeeds because the installer treats Broadcast errors as non-fatal.
type UsersForOrgFn func(ctx context.Context, orgID uuid.UUID) ([]uuid.UUID, error)

// WSManifestBroadcaster is the concrete installer.ManifestChangeBroadcaster
// hosts mount onto an installer to forward hot-swap events over a ws.Hub. It
// is goroutine-safe (the hub is) and never blocks the install path beyond
// the bounded send into the hub's buffered channel.
type WSManifestBroadcaster struct {
	hub   *ws.Hub
	users UsersForOrgFn
	// publish is the seam tests use to capture what would have been sent
	// over the hub. Production wires it to hub.SendToUsers in
	// NewWSManifestBroadcaster; tests overwrite it via withPublisher (a
	// package-private helper) so they can assert on user-id fan-out and
	// payload shape without needing a real WebSocket transport.
	publish func(userIDs []uuid.UUID, msg ws.Message)
}

// NewWSManifestBroadcaster wires the broadcaster. db backs the default
// users-for-org resolver (a single SELECT against `users.organization_id`);
// custom hosts whose user table is named differently or scoped via a join
// supply their own usersFn and may pass db=nil. Panics if hub is nil
// because a nil hub indicates a host wiring bug that would otherwise NPE
// later inside Broadcast under load.
func NewWSManifestBroadcaster(hub *ws.Hub, db *gorm.DB, usersFn UsersForOrgFn) *WSManifestBroadcaster {
	if hub == nil {
		panic("bridge: NewWSManifestBroadcaster requires a non-nil ws.Hub")
	}
	if usersFn == nil {
		usersFn = defaultUsersForOrg(db)
	}
	return &WSManifestBroadcaster{
		hub:     hub,
		users:   usersFn,
		publish: hub.SendToUsers,
	}
}

// Broadcast publishes evt to every WebSocket connection of every user
// belonging to evt.OrgID. The message payload mirrors the kernel event 1:1
// so frontends do not need a separate schema; ws.MessageType doubles as the
// topic. Errors resolving the user list are logged and swallowed (returning
// them would propagate up into installer.Install, which already documents
// the contract of "log and continue").
func (w *WSManifestBroadcaster) Broadcast(ctx context.Context, evt installer.ManifestChangeEvent) error {
	users, err := w.users(ctx, evt.OrgID)
	if err != nil {
		obs.Warn(ctx, "bridge.manifest_broadcaster.users_failed",
			slog.String("org_id", evt.OrgID.String()),
			slog.String("addon_key", evt.AddonKey),
			slog.String("err", err.Error()),
		)
		return nil // installer will swallow either way — surface via log only
	}
	if len(users) == 0 {
		return nil
	}
	w.publish(users, ws.Message{
		Type:    WSManifestChangedType,
		Payload: manifestChangedPayload(evt),
	})
	return nil
}

// manifestChangedPayload normalises the kernel event into the JSON shape
// frontends consume. Keys mirror the camelCase the SDK already uses for
// metadata-cache entries so a consumer can drop the payload straight into
// its cache-invalidation reducer.
func manifestChangedPayload(evt installer.ManifestChangeEvent) map[string]any {
	return map[string]any{
		"orgId":     evt.OrgID,
		"addonKey":  evt.AddonKey,
		"oldHash":   evt.OldHash,
		"newHash":   evt.NewHash,
		"version":   evt.Version,
		"timestamp": evt.Timestamp,
	}
}

// defaultUsersForOrg returns a UsersForOrgFn backed by a single SELECT
// against the `users` table. Hosts whose user model lives in a differently-
// named table inject a custom resolver instead; the bridge does not
// presume any host's column naming beyond `organization_id`.
func defaultUsersForOrg(db *gorm.DB) UsersForOrgFn {
	return func(ctx context.Context, orgID uuid.UUID) ([]uuid.UUID, error) {
		if db == nil {
			return nil, fmt.Errorf("bridge: default UsersForOrgFn requires a *gorm.DB")
		}
		var ids []uuid.UUID
		err := db.WithContext(ctx).
			Table("users").
			Where("organization_id = ?", orgID).
			Pluck("id", &ids).Error
		if err != nil {
			return nil, err
		}
		return ids, nil
	}
}
