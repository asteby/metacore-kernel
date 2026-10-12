// Package convruntime is the bus-driven delivery path of the conversation
// runtime. It is intentionally thin: it does NOT own the conversational engine,
// it is the decoupled entry point that turns an inbound message event into an
// agent invocation.
//
// Flow (gateway -> inbox -> agent -> reply):
//
//  1. A channel gateway delivers an inbound message. The inbox addon persists
//     it through the kernel dynamic service, which emits the canonical event
//     "<inbox>.Message.created".
//  2. This driver is subscribed (ungated, host-level) to that event. On each
//     one it applies three filters - sender_type == "contact" (cuts the
//     agent->agent echo), the conversation's process type is dispatchable, and
//     the process is active - and dedups by message id against a ledger (the
//     bus is at-least-once).
//  3. A surviving message is handed to the host's Dispatcher, which enqueues it
//     into its engine (debounce queue -> LLM loop, or a flow engine).
//  4. The engine writes its reply as a new inbox message with sender_type
//     "agent"/"flow"; the runtime never talks to the gateway directly.
//
// The package depends on three small host ports (Subscriber, Conversations,
// Dispatcher) and a *gorm.DB for the ledger; it imports no host models.
package convruntime

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	kdynamic "github.com/asteby/metacore-kernel/dynamic"
	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// DefaultEvent is the canonical event the driver subscribes to by default.
const DefaultEvent = "link_inbox.Message.created"

// DefaultSubscriberKey is the bus handle used to unsubscribe. It is NOT an
// addon key: the driver subscribes ungated.
const DefaultSubscriberKey = "convruntime"

// ErrConversationNotFound is returned by Conversations.Load for a missing row.
var ErrConversationNotFound = errors.New("convruntime: conversation not found")

// dispatchableProcessTypes are the process types the runtime drives. "user"
// (human operator) and "orchestrator" are deliberately excluded.
var dispatchableProcessTypes = map[string]struct{}{"agent": {}, "flow": {}}

// Handler is the bus handler signature (the host adapts its bus to it).
type Handler func(ctx context.Context, orgID uuid.UUID, payload any) error

// Subscriber is the slice of the host's event bus the driver needs.
type Subscriber interface {
	// SubscribeAll registers handler for event without gating it behind an
	// addon-enabled check.
	SubscribeAll(event, subscriberKey string, handler Handler)
	// Unsubscribe removes every handler registered under subscriberKey.
	Unsubscribe(subscriberKey string)
}

// Conversation is the routing view of a conversation.
type Conversation struct {
	ID             uuid.UUID
	OrganizationID uuid.UUID
	// ProcessType is "agent", "flow", "user" or "orchestrator".
	ProcessType   string
	ProcessActive bool
}

// Conversations loads conversations for routing.
type Conversations interface {
	// Load returns ErrConversationNotFound when the row is gone.
	Load(ctx context.Context, id uuid.UUID) (Conversation, error)
}

// Dispatcher hands an accepted message to the host's engine.
type Dispatcher interface {
	Dispatch(ctx context.Context, conv Conversation, content, messageID string)
}

// ProcessedMessage is the dedup ledger row: one per accepted message id, so a
// second delivery of the same event is a no-op insert. Internal bookkeeping,
// not data of record.
type ProcessedMessage struct {
	MessageID      uuid.UUID `gorm:"type:uuid;primaryKey"`
	OrganizationID uuid.UUID `gorm:"type:uuid;index"`
	ConversationID uuid.UUID `gorm:"type:uuid;index"`
	ProcessedAt    time.Time
}

// TableName pins the ledger table name.
func (ProcessedMessage) TableName() string { return "convruntime_processed" }

// Config wires the driver.
type Config struct {
	Bus           Subscriber
	DB            *gorm.DB
	Conversations Conversations
	Dispatcher    Dispatcher
	// Event overrides DefaultEvent; SubscriberKey overrides DefaultSubscriberKey.
	Event         string
	SubscriberKey string
}

type driver struct {
	db   *gorm.DB
	conv Conversations
	disp Dispatcher
}

// Wire subscribes the driver and returns a cancel func that unsubscribes it
// (call during graceful shutdown). It migrates the dedup ledger. A nil
// dependency is a wiring bug and returns an error so the caller logs it
// instead of silently running dark.
func Wire(cfg Config) (cancel func(), err error) {
	noop := func() {}
	switch {
	case cfg.Bus == nil:
		return noop, errNil("Bus")
	case cfg.DB == nil:
		return noop, errNil("DB")
	case cfg.Conversations == nil:
		return noop, errNil("Conversations")
	case cfg.Dispatcher == nil:
		return noop, errNil("Dispatcher")
	}
	if err := cfg.DB.AutoMigrate(&ProcessedMessage{}); err != nil {
		return noop, err
	}
	event, key := cfg.Event, cfg.SubscriberKey
	if event == "" {
		event = DefaultEvent
	}
	if key == "" {
		key = DefaultSubscriberKey
	}
	d := &driver{db: cfg.DB, conv: cfg.Conversations, disp: cfg.Dispatcher}
	cfg.Bus.SubscribeAll(event, key, d.handle)
	log.Printf("convruntime: bus driver wired (subscribed to %s)", event)
	return func() { cfg.Bus.Unsubscribe(key) }, nil
}

// handle never returns an error that would break the bus fanout: a bad message
// is logged and dropped so one poisoned event cannot stall every other
// conversation.
func (d *driver) handle(ctx context.Context, orgID uuid.UUID, payload any) error {
	ce, ok := payload.(*kdynamic.CanonicalEvent)
	if !ok || ce.After == nil {
		return nil
	}
	after := ce.After

	// Filter 1: only contact-authored messages - the primary anti-loop guard.
	if senderType, _ := after["sender_type"].(string); senderType != "contact" {
		return nil
	}

	messageID, err := parseUUID(ce.ID)
	if err != nil {
		if messageID, err = uuidFromAny(after["id"]); err != nil {
			return nil
		}
	}
	convID, err := uuidFromAny(after["conversation_id"])
	if err != nil {
		return nil
	}

	conv, err := d.conv.Load(ctx, convID)
	if err != nil {
		if !errors.Is(err, ErrConversationNotFound) {
			log.Printf("convruntime: load conversation %s: %v", convID, err)
		}
		return nil
	}
	// Tenant guard: the event's org must own the conversation.
	if conv.OrganizationID != uuid.Nil && orgID != uuid.Nil && conv.OrganizationID != orgID {
		log.Printf("convruntime: org mismatch for conversation %s, dropping", convID)
		return nil
	}

	// Filter 2: only process types the runtime drives. Filter 3: an operator
	// can pause the process.
	if _, ok := dispatchableProcessTypes[conv.ProcessType]; !ok || !conv.ProcessActive {
		return nil
	}

	content, _ := after["content"].(string)
	if content == "" {
		// Media-only inbound has no prompt for the agent yet.
		return nil
	}

	// Dedup: the ledger insert is the arbiter. RowsAffected 0 means another
	// delivery already claimed this message.
	claimed, err := d.claim(orgID, messageID, convID)
	if err != nil {
		log.Printf("convruntime: ledger claim failed (msg=%s): %v", messageID, err)
		return nil
	}
	if !claimed {
		return nil
	}

	log.Printf("convruntime: dispatching message %s (conv=%s, process=%s)", messageID, conv.ID, conv.ProcessType)
	d.disp.Dispatch(ctx, conv, content, messageID.String())
	return nil
}

func (d *driver) claim(orgID, messageID, convID uuid.UUID) (bool, error) {
	res := d.db.Clauses(clause.OnConflict{DoNothing: true}).Create(&ProcessedMessage{
		MessageID: messageID, OrganizationID: orgID, ConversationID: convID, ProcessedAt: time.Now(),
	})
	if res.Error != nil {
		return false, res.Error
	}
	return res.RowsAffected > 0, nil
}

func errNil(what string) error { return fmt.Errorf("convruntime: nil %s", what) }

// parseUUID is uuid.Parse with the empty-string case folded into an error.
func parseUUID(s string) (uuid.UUID, error) {
	if s == "" {
		return uuid.Nil, fmt.Errorf("empty uuid")
	}
	return uuid.Parse(s)
}

// uuidFromAny extracts a UUID from a canonical-event field, which may arrive
// as a string (JSON round-trip) or a uuid.UUID (in-process delivery).
func uuidFromAny(v any) (uuid.UUID, error) {
	switch t := v.(type) {
	case uuid.UUID:
		if t == uuid.Nil {
			return uuid.Nil, fmt.Errorf("nil uuid")
		}
		return t, nil
	case string:
		return parseUUID(t)
	case fmt.Stringer:
		return parseUUID(t.String())
	default:
		return uuid.Nil, fmt.Errorf("unsupported uuid type %T", v)
	}
}
