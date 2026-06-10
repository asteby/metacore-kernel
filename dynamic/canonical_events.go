package dynamic

import (
	"context"
	"fmt"

	"github.com/asteby/metacore-kernel/modelbase"
)

// CanonicalEvent is the payload shape published on the events.Bus for every
// CRUD mutation routed through dynamic.Service. The JSON-equivalent shape is
// `{id, model, action, addon_key, actor_id?, correlation_id?, before?, after?}`:
//
//   - `created` carries `id`, `model`, `action`, `addon_key`, `actor_id`,
//     `correlation_id` + `after` (the fresh row, including server-generated
//     fields like `created_at`).
//   - `updated` carries all envelope fields + `before` (snapshot loaded before
//     the input merge) and `after` (the saved row after Save).
//   - `deleted` carries all envelope fields + `before` (the snapshot just
//     before Delete). A missing `before` on `deleted` means the row was already
//     gone or out of tenant scope when we tried to read it.
//
// Subscribers receive the value untyped (events.Handler signature is
// `func(ctx, orgID, payload any)`) — they should type-assert
// `*dynamic.CanonicalEvent` for in-process delivery and rely on the JSON tags
// when forwarding elsewhere.
//
// The ID, Before, and After fields are preserved with the same names and types
// for backward-compatibility with existing subscribers.
type CanonicalEvent struct {
	ID            string         `json:"id"`
	Model         string         `json:"model"`
	Action        string         `json:"action"`          // created|updated|deleted
	AddonKey      string         `json:"addon_key"`
	ActorID       string         `json:"actor_id,omitempty"`
	CorrelationID string         `json:"correlation_id,omitempty"`
	Before        map[string]any `json:"before,omitempty"`
	After         map[string]any `json:"after,omitempty"`
}

// correlationIDKey is an unexported typed key used to store a correlation ID
// in a context.Context. Using a private struct type (instead of a plain string)
// avoids collisions with keys from other packages.
type correlationIDKey struct{}

// WithCorrelationID returns a child context carrying the given correlation ID.
// The host (e.g. ops) should call this with the X-Request-ID from the
// incoming HTTP request before passing the context to any Service method.
func WithCorrelationID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, correlationIDKey{}, id)
}

// CorrelationIDFromContext extracts the correlation ID previously stored by
// WithCorrelationID. It returns an empty string when no ID was set.
func CorrelationIDFromContext(ctx context.Context) string {
	v, _ := ctx.Value(correlationIDKey{}).(string)
	return v
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
	payload := &CanonicalEvent{
		ID:            id,
		Model:         model,
		Action:        action,
		AddonKey:      addonKey,
		ActorID:       user.GetID().String(),
		CorrelationID: CorrelationIDFromContext(ctx),
		Before:        before,
		After:         after,
	}
	event := fmt.Sprintf("%s.%s.%s", addonKey, model, action)
	_ = s.bus.Publish(ctx, addonKey, event, user.GetOrganizationID(), payload)
}
