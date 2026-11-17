package webhooks

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/asteby/metacore-kernel/events"
)

// BridgeOptions configures the bus -> declarative subscriptions bridge.
type BridgeOptions struct {
	// OwnerType under which subscriptions are stored for the org that emitted
	// the event. Default "organization" (OwnerID = the event's org).
	OwnerType string
	// AddonKey used for the bus capability check (event:subscribe). Default
	// "kernel" (host-originated subscription).
	AddonKey string
	// Pattern the bridge subscribes to on the bus. Default "*": the bridge has
	// to see every event because subscriptions are data, not code.
	Pattern string
}

// Bridge feeds events from the in-process bus (events.Bus) into the
// declarative subscriptions of the Service (RFC 0003, F2). It delivers ONLY to
// declarative subscriptions: classic webhooks keep being fed by explicit
// Service.Trigger calls, so attaching the bridge never changes what they get.
type Bridge struct {
	svc  *Service
	opts BridgeOptions
}

// NewBridge builds a bridge over svc.
func NewBridge(svc *Service, opts BridgeOptions) *Bridge {
	if opts.OwnerType == "" {
		opts.OwnerType = "organization"
	}
	if opts.AddonKey == "" {
		opts.AddonKey = "kernel"
	}
	if opts.Pattern == "" {
		opts.Pattern = "*"
	}
	return &Bridge{svc: svc, opts: opts}
}

// Attach subscribes the bridge to bus as a DURABLE router: if the delivery
// cannot be recorded (database down) the bus reports ErrDurableDelivery, so a
// durable publisher (outbox) keeps the event and retries. That retry is safe:
// a republished event carries the same id and is deduplicated per subscription.
func (b *Bridge) Attach(bus *events.Bus) error {
	return bus.SubscribeDurableRouting(b.opts.AddonKey, b.opts.Pattern, b.Handle,
		func(ctx context.Context, orgID uuid.UUID, event string) (int, bool) {
			n, err := b.svc.countMatching(ctx, b.opts.OwnerType, orgID, event)
			return n, err == nil
		})
}

// Handle is the RoutingHandler body: it returns the number of deliveries
// created or retained for the event.
func (b *Bridge) Handle(ctx context.Context, orgID uuid.UUID, event string, payload any) (int, error) {
	ev, err := EventFromBus(orgID, event, payload, b.svc.clock())
	if err != nil {
		return 0, err
	}
	return b.svc.TriggerEvent(ctx, b.opts.OwnerType, orgID, ev)
}

// countMatching estimates, without creating anything, how many declarative
// subscriptions would take the event (filters are not evaluated).
func (s *Service) countMatching(ctx context.Context, ownerType string, ownerID uuid.UUID, event string) (int, error) {
	var subs []Webhook
	if err := s.db.WithContext(ctx).
		Where("owner_type = ? AND owner_id = ? AND active = ?", ownerType, ownerID, true).
		Find(&subs).Error; err != nil {
		return 0, err
	}
	n := 0
	for i := range subs {
		if subs[i].Declarative != nil && containsEvent(subs[i].Events, event) {
			n++
		}
	}
	return n, nil
}

// EventFromBus converts what the bus delivers (a payload and, when the host
// stamped it, the `envelope` block of docs/events-bus.md) into an Event. When
// the payload carries no envelope id, a deterministic id is derived from
// (org, event, payload bytes, 10-minute bucket): a prompt republication dedupes.
func EventFromBus(orgID uuid.UUID, name string, payload any, now time.Time) (Event, error) {
	var raw []byte
	switch p := payload.(type) {
	case nil:
		raw = []byte("{}")
	case json.RawMessage:
		raw = p
	case []byte:
		raw = p
	default:
		b, err := json.Marshal(p)
		if err != nil {
			return Event{}, fmt.Errorf("webhooks: event %q payload is not JSON: %w", name, err)
		}
		raw = b
	}
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return Event{}, fmt.Errorf("webhooks: event %q payload is not JSON: %w", name, err)
	}
	body, ok := decoded.(map[string]any)
	if !ok {
		body = map[string]any{"value": decoded}
	}
	ev := Event{Name: name, OrgID: orgID, OccurredAt: now}
	if env, ok := body["envelope"].(map[string]any); ok {
		ev.ID = str(env["id"])
		ev.Source = str(env["source"])
		ev.BranchID = str(env["branch_id"])
		ev.CorrelationID = str(env["correlation_id"])
		ev.ActorID = str(env["actor_id"])
		if agg, ok := env["aggregate"].(map[string]any); ok {
			ev.AggregateType = str(agg["type"])
			ev.AggregateID = str(agg["id"])
		}
		if seq, ok := env["sequence"].(float64); ok {
			ev.Sequence = int64(seq)
		}
		if t, err := time.Parse(time.RFC3339, str(env["occurred_at"])); err == nil {
			ev.OccurredAt = t
		}
		if g, ok := env["generation"].(float64); ok && g > 0 && ev.ID != "" {
			ev.ID = fmt.Sprintf("%s.g%d", ev.ID, int64(g)) // a Replay is a new generation
		}
		cp := make(map[string]any, len(body))
		for k, v := range body {
			if k != "envelope" {
				cp[k] = v
			}
		}
		body = cp
	}
	if ev.ID == "" {
		h := sha256.New()
		h.Write([]byte(orgID.String()))
		h.Write([]byte{0})
		h.Write([]byte(name))
		h.Write([]byte{0})
		h.Write(raw)
		// 10-minute bucket: a relay retry of the same event lands in the same bucket
		// and dedupes, while a legitimate identical event later is a new occurrence.
		// Hosts that stamp envelope.id get exact dedupe.
		h.Write([]byte{0})
		h.Write([]byte(fmt.Sprint(now.Unix() / 600)))
		ev.ID = "h-" + hex.EncodeToString(h.Sum(nil))[:40]
	}
	ev.Payload = body
	return ev, nil
}

func str(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}
