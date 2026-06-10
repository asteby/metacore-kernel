package audit

import (
	"context"
	"fmt"
	"log"

	"github.com/asteby/metacore-kernel/dynamic"
	"github.com/asteby/metacore-kernel/events"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// subscriberKey is the addon key the Recorder registers under on the bus.
//
// It MUST be exactly "kernel": the events.Bus capability check (events.go,
// check()) bypasses the Enforcer only for the literal "kernel" key. Any other
// key (e.g. "kernel.audit") is treated as an addon and, when a host runs the
// bus in ModeEnforce with a real Enforcer, an unregistered key is rejected
// ("addon not registered"). Subscribing as the trusted host avoids that.
//
// Consequence: Unsubscribe is scoped to the "kernel" key. The audit Recorder is
// the only kernel-level bus subscriber in the platform, so this never collides;
// if that changes, give each host subscriber a distinct trusted facility.
const subscriberKey = "kernel"

// ActorResolver optionally backfills a human-readable actor name/email from an
// actor id. The kernel does not know the host's users table, so this is the
// hook a host provides if it wants denormalised actor info on the in-memory
// Entry. It is best-effort: returning ("","") (or being unset) simply leaves
// those fields blank. It is NOT persisted by the Recorder (Entry.ActorName /
// ActorEmail are gorm:"-"); a host that wants them on its own table can add
// columns and a write hook, or denormalise at query time.
type ActorResolver func(ctx context.Context, actorID uuid.UUID) (name, email string)

// Options configures a Recorder. Construct via the With* functional options
// passed to Wire.
type Options struct {
	// tableName is the destination table. Defaults to DefaultTableName.
	tableName string
	// skipModels are model names never written to the trail. The audit table's
	// own model is always implicitly skipped to prevent self-emit loops.
	skipModels map[string]bool
	// resolver optionally denormalises actor name/email.
	resolver ActorResolver
}

// Option mutates Options. Applied left-to-right in Wire.
type Option func(*Options)

// WithTableName overrides the destination table (default "audit_entries"). Pass
// "activity_logs" to keep an app's historical table name.
func WithTableName(name string) Option {
	return func(o *Options) {
		if name != "" {
			o.tableName = name
		}
	}
}

// WithSkipModels adds model names that must never be audited (e.g. high-volume
// internal tables). The audit table's own backing model is always skipped
// regardless. Repeatable; entries accumulate.
func WithSkipModels(models ...string) Option {
	return func(o *Options) {
		for _, m := range models {
			if m != "" {
				o.skipModels[m] = true
			}
		}
	}
}

// WithActorResolver installs an ActorResolver to backfill actor name/email on
// the in-memory Entry.
func WithActorResolver(r ActorResolver) Option {
	return func(o *Options) { o.resolver = r }
}

func defaultOptions() *Options {
	return &Options{
		tableName:  DefaultTableName,
		skipModels: map[string]bool{},
		resolver:   nil,
	}
}

// Recorder subscribes to canonical CRUD events on the kernel events.Bus and
// persists one audit Entry per mutation. It is created and activated by Wire;
// construct it directly only for testing the handler in isolation.
type Recorder struct {
	db   *gorm.DB
	opts *Options
}

// NewRecorder builds a Recorder without subscribing it to any bus. Prefer Wire
// for production use; this constructor exists so the persist path can be
// unit-tested without a live bus.
func NewRecorder(db *gorm.DB, opts ...Option) *Recorder {
	o := defaultOptions()
	for _, opt := range opts {
		opt(o)
	}
	return &Recorder{db: db, opts: o}
}

// TableName returns the destination table the Recorder writes to.
func (r *Recorder) TableName() string { return r.opts.tableName }

// skip reports whether a model name must not be audited. The audit table's own
// model (matched by table name and the legacy "ActivityLog" struct name) is
// always skipped so the Recorder never logs its own writes.
func (r *Recorder) skip(model string) bool {
	if model == "" {
		return true
	}
	if model == r.opts.tableName || model == DefaultTableName ||
		model == "ActivityLog" || model == "activity_logs" {
		return true
	}
	return r.opts.skipModels[model]
}

// Handle persists one canonical CRUD event as an audit Entry. It is the
// events.Handler the Recorder registers on the bus. The v0.52.0 CanonicalEvent
// is self-describing — Model/Action/AddonKey/ActorID/CorrelationID/Before/After
// are all populated by dynamic.publishCanonical — so this handler reads them
// directly with no per-request context plumbing required.
//
// Best-effort: the source mutation already committed, so a persist failure is
// logged and swallowed (never propagated back through the bus).
func (r *Recorder) Handle(ctx context.Context, orgID uuid.UUID, payload any) error {
	ce, ok := payload.(*dynamic.CanonicalEvent)
	if !ok || ce == nil {
		// Non-canonical payload (domain events like pos.order_paid). Ignore.
		return nil
	}
	if r.skip(ce.Model) {
		return nil
	}

	entry := Entry{
		OrganizationID: orgID,
		AddonKey:       ce.AddonKey,
		Model:          ce.Model,
		RecordID:       ce.ID,
		Action:         ce.Action,
		CorrelationID:  parseUUIDPtr(ce.CorrelationID),
		ActorID:        parseUUIDPtr(ce.ActorID),
		Before:         mapToJSONString(ce.Before),
		After:          mapToJSONString(ce.After),
		Changes:        computeChanges(ce.Before, ce.After),
	}

	// Optional actor denormalisation (in-memory only; not persisted).
	if r.opts.resolver != nil && entry.ActorID != nil {
		entry.ActorName, entry.ActorEmail = r.opts.resolver(ctx, *entry.ActorID)
	}

	// NewDB: a clean session unaffected by any outer transaction on r.db.
	// SkipHooks avoids re-triggering GORM callbacks (the audit table has none
	// that matter, but this is defensive against host-registered global hooks).
	if err := r.db.Session(&gorm.Session{NewDB: true, SkipHooks: false}).
		Table(r.opts.tableName).
		Create(&entry).Error; err != nil {
		log.Printf("audit: failed to persist entry (model=%s action=%s id=%s): %v",
			ce.Model, ce.Action, ce.ID, err)
	}
	return nil
}

// Wire is the OPT-IN entry point: it subscribes a Recorder to every canonical
// CRUD event on the kernel events.Bus, persisting an audit Entry per mutation.
// A host calls it ONCE at boot to activate the universal audit trail; importing
// this package without calling Wire has zero effect.
//
//	cancel, err := audit.Wire(bus, db)
//	if err != nil { return err }
//	defer cancel() // unsubscribe on graceful shutdown
//
// A single "*" subscription captures EVERY declarative model — present and
// future, including addons installed after boot — because the v0.52.0
// CanonicalEvent is self-describing. The returned cancel func unsubscribes
// (idempotent).
//
// The Recorder also auto-migrates its destination table (Migrate) so the host
// does not have to thread the schema separately; pass an already-migrated table
// and the AutoMigrate is a no-op.
func Wire(bus *events.Bus, db *gorm.DB, opts ...Option) (cancel func(), err error) {
	noop := func() {}
	if bus == nil {
		return noop, fmt.Errorf("audit: nil events.Bus")
	}
	if db == nil {
		return noop, fmt.Errorf("audit: nil *gorm.DB")
	}

	r := NewRecorder(db, opts...)
	if err := Migrate(db, r.opts.tableName); err != nil {
		return noop, fmt.Errorf("audit: migrate %s: %w", r.opts.tableName, err)
	}

	// Subscribe under the trusted "kernel" key (see subscriberKey doc): the
	// events.Bus bypasses the Enforcer's event:subscribe check for "kernel",
	// so the audit trail works even when the host runs the bus in ModeEnforce.
	if err := bus.Subscribe(subscriberKey, "*", r.Handle); err != nil {
		return noop, fmt.Errorf("audit: subscribe: %w", err)
	}
	log.Printf("audit: wired universal audit trail (table=%s, wildcard subscription)", r.opts.tableName)

	cancel = func() { bus.Unsubscribe(subscriberKey) }
	return cancel, nil
}

// parseUUIDPtr parses a string into a *uuid.UUID, returning nil for an empty or
// unparseable input (or the all-zero UUID, which carries no information).
func parseUUIDPtr(s string) *uuid.UUID {
	if s == "" {
		return nil
	}
	id, err := uuid.Parse(s)
	if err != nil || id == uuid.Nil {
		return nil
	}
	return &id
}
