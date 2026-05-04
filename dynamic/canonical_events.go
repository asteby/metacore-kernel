package dynamic

import (
	"context"
	"fmt"

	"github.com/asteby/metacore-kernel/modelbase"
)

// CanonicalEvent is the payload shape published on the events.Bus for every
// CRUD mutation routed through dynamic.Service. The JSON-equivalent shape is
// `{id, before?, after?}`:
//
//   - `created` carries `id` + `after` (the fresh row, including server-generated
//     fields like `created_at`).
//   - `updated` carries `id`, `before` (snapshot loaded before the input merge)
//     and `after` (the saved row after Save).
//   - `deleted` carries `id` and, when the row was readable inside the tenant
//     scope at publish time, `before` (the snapshot just before Delete). A
//     missing `before` on `deleted` means the row was already gone or out of
//     scope when we tried to read it.
//
// Subscribers receive the value untyped (events.Handler signature is
// `func(ctx, orgID, payload any)`) — they should type-assert
// `*dynamic.CanonicalEvent` for in-process delivery and rely on the JSON tags
// when forwarding elsewhere.
type CanonicalEvent struct {
	ID     string         `json:"id"`
	Before map[string]any `json:"before,omitempty"`
	After  map[string]any `json:"after,omitempty"`
}

// publishCanonical builds the event name `<addonKey>.<model>.<action>` and
// fans it out through the Bus. It is a no-op when no Bus was wired, keeping
// pre-event apps unchanged. The producer addonKey passed to Bus.Publish is
// the same one used to namespace the event — the kernel itself bypasses the
// `event:emit` capability check (`events/events.go`), so models with no addon
// owner publish trusted under "kernel".
//
// Errors from the Bus (capability denial, etc.) are intentionally swallowed:
// the DB mutation has already committed, the Bus already logs the failure,
// and we keep the same "post-mutation side-effects must not roll back the
// data" policy as the AfterCreate/Update/Delete hooks.
func (s *Service) publishCanonical(ctx context.Context, model, action string, user modelbase.AuthUser, id string, before, after map[string]any) {
	if s.bus == nil {
		return
	}
	addonKey := "kernel"
	if s.addonKeyForModel != nil {
		if k := s.addonKeyForModel(ctx, model); k != "" {
			addonKey = k
		}
	}
	payload := &CanonicalEvent{ID: id, Before: before, After: after}
	event := fmt.Sprintf("%s.%s.%s", addonKey, model, action)
	_ = s.bus.Publish(ctx, addonKey, event, user.GetOrganizationID(), payload)
}
