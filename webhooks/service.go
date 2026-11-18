package webhooks

import (
	"context"
	crand "crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/asteby/metacore-kernel/runtime/webhookout"
)

// Config wires the service. DB is mandatory.
type Config struct {
	DB          *gorm.DB
	WorkerCount int
	// HTTPClient is used ONLY by classic webhooks (Declarative == nil). Default:
	// the webhookout egress client (dial-time IP guard, no redirects, 15 s). A
	// client you supply replaces that protection for classic webhooks: it is the
	// host's responsibility. Declarative subscriptions NEVER use it.
	HTTPClient *http.Client
	// Clock is overridable for tests; defaults to time.Now.
	Clock func() time.Time

	// --- Declarative subscriptions (RFC 0003). All optional: without them
	// classic webhooks behave exactly as before. ---

	// Schemas declares the payload paths of each event so a transform/filter
	// naming a path that does not exist fails AT SAVE. Without it, a
	// declarative subscription may only reference `event.*` paths.
	Schemas SchemaSource
	// Secrets resolves `secret_ref` / `key_ref` of signing profiles FOR AN OWNER.
	// Required to save a subscription that declares Signing.
	Secrets SecretResolver
	// AllowHosts, when non-empty, restricts public targets to these patterns
	// ("api.example.com", "*.example.com") on top of the netguard rules. Checked
	// at save AND before every send.
	AllowHosts []string
	// AllowInternalHosts are internal destinations ("host" or "host:port") the
	// host explicitly authorizes (e.g. a receiver on the private network). They
	// skip the public-domain rules; everything else stays denied.
	AllowInternalHosts []string
	// EgressTLS customizes TLS of the delivery client (private CA). Optional.
	EgressTLS *tls.Config
	// UnsafeAllowPrivateTargets disables target validation. TESTS ONLY.
	UnsafeAllowPrivateTargets bool
	// OnAutoDisable is called (once, after the row is saved) when a declarative
	// subscription switched itself off after too many consecutive failures. Use
	// it to raise the alert. It must not block.
	OnAutoDisable func(ctx context.Context, w Webhook)
	// HostID identifica a este host en las marcas anti-bucle del cuerpo saliente
	// (`_origin` = "<HostID>:sub:<key>", ver Declarative.PropagateOrigin). Vacio:
	// no se estampa nada (compatibilidad: el cuerpo saliente no cambia hasta que
	// el host lo configure).
	HostID string
	// Rand returns a value in [0,1) for retry jitter; default math/rand/v2.
	Rand func() float64

	// BatchSize is how many due deliveries a worker claims per tick (default 10).
	BatchSize int
	// PerWebhookConcurrency / PerOwnerConcurrency cap how many deliveries of one
	// subscription / one owner go out in the same tick, so a failing tenant
	// cannot monopolize the workers (defaults 2 / 4).
	PerWebhookConcurrency int
	PerOwnerConcurrency   int
	// MaxSubscriptionsPerOwner caps declarative subscriptions per owner (default 100).
	MaxSubscriptionsPerOwner int
	// HeldRetention / DeadRetention: how long the payload of a retained or
	// dead-lettered delivery is kept before it is erased (defaults 14 d / 30 d;
	// negative = keep forever).
	HeldRetention time.Duration
	DeadRetention time.Duration
}

// inFlightLease is how long a claimed delivery counts as in progress before it
// is considered orphaned (worker crash) and re-queued.
const inFlightLease = 5 * time.Minute

// maxPayloadBytes bounds the incoming payload and the rendered body.
const maxPayloadBytes = 256 << 10

// Service is the public API. It is transport-agnostic.
type Service struct {
	db      *gorm.DB
	workers int
	client  *http.Client
	egress  *http.Client
	clock   func() time.Time

	schemas       SchemaSource
	secrets       SecretResolver
	allowHosts    []string
	allowInternal []string
	allowPrivate  bool
	onAutoOff     func(ctx context.Context, w Webhook)
	hostID        string
	rnd           func() float64

	batch, perWebhook, perOwner, maxSubs int
	heldTTL, deadTTL                     time.Duration

	seqMu   sync.Mutex
	lastSeq int64

	mu        sync.Mutex
	running   bool
	cancelRun context.CancelFunc
	wg        sync.WaitGroup
}

// New constructs a Service with sensible defaults.
func New(cfg Config) *Service {
	if cfg.DB == nil {
		panic("webhooks: Config.DB is required")
	}
	if cfg.WorkerCount <= 0 {
		cfg.WorkerCount = 5
	}
	pol := webhookout.Policy{AllowHosts: cfg.AllowHosts, AllowInternal: cfg.AllowInternalHosts, AllowPathPrefixes: []string{"/"}}
	egress := pol.NewClientTLS(cfg.EgressTLS)
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = pol.NewClientTLS(cfg.EgressTLS)
		cfg.HTTPClient.Timeout = 15 * time.Second
	}
	if cfg.Clock == nil {
		cfg.Clock = time.Now
	}
	if cfg.Rand == nil {
		cfg.Rand = rand.Float64
	}
	def := func(v *int, d int) {
		if *v <= 0 {
			*v = d
		}
	}
	def(&cfg.BatchSize, 10)
	def(&cfg.PerWebhookConcurrency, 2)
	def(&cfg.PerOwnerConcurrency, 4)
	def(&cfg.MaxSubscriptionsPerOwner, 100)
	if cfg.HeldRetention == 0 {
		cfg.HeldRetention = 14 * 24 * time.Hour
	}
	if cfg.DeadRetention == 0 {
		cfg.DeadRetention = 30 * 24 * time.Hour
	}
	return &Service{
		onAutoOff:     cfg.OnAutoDisable,
		hostID:        cfg.HostID,
		rnd:           cfg.Rand,
		db:            cfg.DB,
		workers:       cfg.WorkerCount,
		client:        cfg.HTTPClient,
		egress:        egress,
		clock:         cfg.Clock,
		schemas:       cfg.Schemas,
		secrets:       cfg.Secrets,
		allowHosts:    cfg.AllowHosts,
		allowInternal: cfg.AllowInternalHosts,
		allowPrivate:  cfg.UnsafeAllowPrivateTargets,
		batch:         cfg.BatchSize, perWebhook: cfg.PerWebhookConcurrency, perOwner: cfg.PerOwnerConcurrency,
		maxSubs: cfg.MaxSubscriptionsPerOwner,
		heldTTL: cfg.HeldRetention, deadTTL: cfg.DeadRetention,
	}
}

// LogsParams filters WebhookDelivery listings.
type LogsParams struct {
	Page    int
	PerPage int
	Event   string
	Success *bool
}

// IsEnabled reports whether a declarative subscription is sending: switched on
// AND not tripped by the health check. Classic webhooks are never "enabled" in
// this sense (they use Active).
func (w *Webhook) IsEnabled() bool {
	return w.Declarative != nil && w.Declarative.Enabled && w.AutoDisabledAt == nil
}

// --- CRUD ---------------------------------------------------------------

func (s *Service) Create(ctx context.Context, w *Webhook) error {
	if err := validateWebhook(w); err != nil {
		return err
	}
	if err := s.validateDeclarative(w); err != nil {
		return err
	}
	if w.Declarative != nil {
		var n int64
		if err := s.db.WithContext(ctx).Model(&Webhook{}).
			Where("owner_type = ? AND owner_id = ? AND declarative IS NOT NULL", w.OwnerType, w.OwnerID).
			Count(&n).Error; err != nil {
			return err
		}
		if int(n) >= s.maxSubs {
			return ErrTooManySubs
		}
	}
	// A declarative subscription that signs by profile never stores a secret.
	if w.Secret == "" && (w.Declarative == nil || w.Declarative.Signing == nil) {
		w.Secret = generateSecret()
	}
	return s.db.WithContext(ctx).Create(w).Error
}

func (s *Service) List(ctx context.Context, ownerType string, ownerID uuid.UUID) ([]Webhook, error) {
	var out []Webhook
	err := s.db.WithContext(ctx).
		Where("owner_type = ? AND owner_id = ?", ownerType, ownerID).
		Order("created_at DESC").
		Find(&out).Error
	return out, err
}

func (s *Service) Get(ctx context.Context, id uuid.UUID) (*Webhook, error) {
	var w Webhook
	if err := s.db.WithContext(ctx).First(&w, "id = ?", id).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			return nil, ErrWebhookNotFound
		}
		return nil, err
	}
	return &w, nil
}

// GetOwned is Get restricted to one owner: a webhook of another owner is
// ErrWebhookNotFound (indistinguishable from a missing one). Every operation
// reachable by an end user must start here.
func (s *Service) GetOwned(ctx context.Context, ownerType string, ownerID uuid.UUID, id uuid.UUID) (*Webhook, error) {
	var w Webhook
	if err := s.db.WithContext(ctx).First(&w, "id = ? AND owner_type = ? AND owner_id = ?", id, ownerType, ownerID).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			return nil, ErrWebhookNotFound
		}
		return nil, err
	}
	return &w, nil
}

// DeliveryOwned loads a delivery only if its webhook belongs to the owner.
func (s *Service) DeliveryOwned(ctx context.Context, ownerType string, ownerID uuid.UUID, id uuid.UUID) (*WebhookDelivery, error) {
	var d WebhookDelivery
	err := s.db.WithContext(ctx).
		Joins("JOIN webhooks ON webhooks.id = webhook_deliveries.webhook_id AND webhooks.deleted_at IS NULL").
		Where("webhook_deliveries.id = ? AND webhooks.owner_type = ? AND webhooks.owner_id = ?", id, ownerType, ownerID).
		First(&d).Error
	if err != nil {
		if err == gorm.ErrRecordNotFound {
			return nil, ErrDeliveryNotFound
		}
		return nil, err
	}
	return &d, nil
}

// Update saves the configuration columns of w (name, url, events, active,
// retry_max, timeout_sec, declarative) and NOTHING else: owner, id, counters and
// health are never overwritten. Turning a declarative subscription on (or
// resuming an auto-disabled one) clears its health and releases the retained
// events. A declarative subscription cannot be turned back into a classic one.
func (s *Service) Update(ctx context.Context, w *Webhook) error {
	if err := validateWebhook(w); err != nil {
		return err
	}
	var prev Webhook
	if err := s.db.WithContext(ctx).First(&prev, "id = ?", w.ID).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			return ErrWebhookNotFound
		}
		return err
	}
	// owner is immutable: validate secrets/targets against the stored owner
	w.OwnerType, w.OwnerID = prev.OwnerType, prev.OwnerID
	if err := s.validateDeclarative(w); err != nil {
		return err
	}
	if prev.Declarative != nil && w.Declarative == nil {
		return fmt.Errorf("%w: a declarative subscription cannot become a classic webhook", ErrInvalidDeclarative)
	}
	if err := s.db.WithContext(ctx).Model(&Webhook{}).Where("id = ?", w.ID).
		Select("Name", "URL", "Events", "Active", "RetryMax", "TimeoutSec", "Declarative").
		Updates(w).Error; err != nil {
		return err
	}
	if prev.Declarative != nil && !prev.IsEnabled() && w.Declarative.Enabled {
		return s.resume(ctx, w.ID)
	}
	return nil
}

// Delete removes the webhook AND cancels its pending deliveries, erasing their
// (personal) payloads: nothing is left queued for a subscription that is gone.
func (s *Service) Delete(ctx context.Context, id uuid.UUID) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&WebhookDelivery{}).Where("webhook_id = ? AND succeeded = ?", id, false).
			Updates(purgeFields("webhook deleted")).Error; err != nil {
			return err
		}
		return tx.Delete(&Webhook{}, "id = ?", id).Error
	})
}

// purgeFields cancels a delivery and erases its payload.
func purgeFields(reason string) map[string]any {
	return map[string]any{
		"payload": JSONMap{}, "payload_purged": true, "held": false, "in_flight": false,
		"next_attempt_at": nil, "error_message": reason,
	}
}

// SetEnabled turns a declarative subscription on or off. Turning it on clears
// the health trip and releases every event retained while it was off (returns
// how many); turning it off makes new events (and queued ones) be retained.
func (s *Service) SetEnabled(ctx context.Context, id uuid.UUID, enabled bool) (int64, error) {
	w, err := s.Get(ctx, id)
	if err != nil {
		return 0, err
	}
	if w.Declarative == nil {
		return 0, ErrNotDeclarative
	}
	w.Declarative.Enabled = enabled
	b, err := json.Marshal(w.Declarative)
	if err != nil {
		return 0, err
	}
	if err := s.db.WithContext(ctx).Model(&Webhook{}).Where("id = ?", id).Update("declarative", string(b)).Error; err != nil {
		return 0, err
	}
	if !enabled {
		return 0, nil
	}
	return s.releaseResume(ctx, id)
}

func (s *Service) resume(ctx context.Context, id uuid.UUID) error {
	_, err := s.releaseResume(ctx, id)
	return err
}

// releaseResume clears the health trip and releases the retained deliveries.
func (s *Service) releaseResume(ctx context.Context, id uuid.UUID) (int64, error) {
	if err := s.db.WithContext(ctx).Model(&Webhook{}).Where("id = ?", id).
		Updates(map[string]any{"consecutive_failures": 0, "auto_disabled_at": nil, "last_error": ""}).Error; err != nil {
		return 0, err
	}
	now := s.clock()
	res := s.db.WithContext(ctx).Model(&WebhookDelivery{}).
		Where("webhook_id = ? AND held = ? AND succeeded = ? AND payload_purged = ?", id, true, false, false).
		Updates(map[string]any{"held": false, "next_attempt_at": now})
	return res.RowsAffected, res.Error
}

// PurgeDeadPayloads erases the (personal) payload of deliveries that gave up
// (retries exhausted) before olderThan. Delivered ones are already purged.
func (s *Service) PurgeDeadPayloads(ctx context.Context, webhookID uuid.UUID, olderThan time.Time) (int64, error) {
	res := s.db.WithContext(ctx).Model(&WebhookDelivery{}).
		Where("webhook_id = ? AND succeeded = ? AND held = ? AND in_flight = ? AND next_attempt_at IS NULL AND payload_purged = ? AND created_at < ?",
			webhookID, false, false, false, false, olderThan).
		Updates(map[string]any{"payload": JSONMap{}, "payload_purged": true})
	return res.RowsAffected, res.Error
}

// PurgeExpired applies the retention policy: the payload of deliveries retained
// longer than HeldRetention is erased (and they stop waiting), and the payload
// of dead-lettered ones older than DeadRetention is erased. The worker pool runs
// it periodically; hosts without workers can schedule it.
func (s *Service) PurgeExpired(ctx context.Context) (int64, error) {
	now := s.clock()
	var total int64
	if s.heldTTL > 0 {
		res := s.db.WithContext(ctx).Model(&WebhookDelivery{}).
			Where("held = ? AND succeeded = ? AND created_at < ?", true, false, now.Add(-s.heldTTL)).
			Updates(purgeFields("retention expired while the subscription was off"))
		if res.Error != nil {
			return total, res.Error
		}
		total += res.RowsAffected
	}
	if s.deadTTL > 0 {
		res := s.db.WithContext(ctx).Model(&WebhookDelivery{}).
			Where("succeeded = ? AND held = ? AND in_flight = ? AND next_attempt_at IS NULL AND payload_purged = ? AND created_at < ?",
				false, false, false, false, now.Add(-s.deadTTL)).
			Updates(map[string]any{"payload": JSONMap{}, "payload_purged": true})
		if res.Error != nil {
			return total, res.Error
		}
		total += res.RowsAffected
	}
	return total, nil
}

// --- Operations ---------------------------------------------------------

// Test sends a test delivery synchronously. Returns the delivery record.
// (Also works on a disabled declarative subscription: it is an explicit action.
// A test never counts for the health check.)
func (s *Service) Test(ctx context.Context, webhookID uuid.UUID) (*WebhookDelivery, error) {
	w, err := s.Get(ctx, webhookID)
	if err != nil {
		return nil, err
	}
	payload := JSONMap{"event": "webhook.test", "webhook_id": w.ID.String(), "timestamp": s.clock().Unix()}
	d := &WebhookDelivery{WebhookID: w.ID, Event: "webhook.test", Payload: payload}
	if w.Declarative != nil {
		d.IdempotencyKey = "test-" + uuid.NewString()
	}
	if err := s.db.WithContext(ctx).Create(d).Error; err != nil {
		return nil, err
	}
	s.attemptDelivery(ctx, w, d)
	return d, nil
}

// Replay re-runs a prior delivery. For a declarative subscription it reuses the
// original idempotency key, so a target that already processed it replies
// "replayed" instead of duplicating; if the subscription is off, the replay is
// retained like any other event. A delivery whose payload was purged (delivered,
// expired, deleted, or never rendered) cannot be replayed (ErrPayloadPurged).
func (s *Service) Replay(ctx context.Context, deliveryID uuid.UUID) (*WebhookDelivery, error) {
	var orig WebhookDelivery
	if err := s.db.WithContext(ctx).First(&orig, "id = ?", deliveryID).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			return nil, ErrDeliveryNotFound
		}
		return nil, err
	}
	if orig.PayloadPurged {
		return nil, ErrPayloadPurged
	}
	w, err := s.Get(ctx, orig.WebhookID)
	if err != nil {
		return nil, err
	}
	// EventID is not copied: (webhook, event_id) is unique per occurrence.
	d := &WebhookDelivery{WebhookID: w.ID, Event: orig.Event, Payload: orig.Payload, IdempotencyKey: orig.IdempotencyKey}
	if w.Declarative != nil && !w.IsEnabled() {
		d.Held = true
		return d, s.db.WithContext(ctx).Create(d).Error
	}
	if err := s.db.WithContext(ctx).Create(d).Error; err != nil {
		return nil, err
	}
	s.attemptDelivery(ctx, w, d)
	return d, nil
}

// Logs lists deliveries for a webhook.
func (s *Service) Logs(ctx context.Context, webhookID uuid.UUID, p LogsParams) ([]WebhookDelivery, int64, error) {
	if p.Page <= 0 {
		p.Page = 1
	}
	if p.PerPage <= 0 || p.PerPage > 200 {
		p.PerPage = 50
	}

	q := s.db.WithContext(ctx).Model(&WebhookDelivery{}).Where("webhook_id = ?", webhookID)
	if p.Event != "" {
		q = q.Where("event = ?", p.Event)
	}
	if p.Success != nil {
		q = q.Where("succeeded = ?", *p.Success)
	}

	var total int64
	if err := q.Count(&total).Error; err != nil {
		return nil, 0, err
	}

	var out []WebhookDelivery
	err := q.Order("created_at DESC").
		Limit(p.PerPage).Offset((p.Page - 1) * p.PerPage).
		Find(&out).Error
	return out, total, err
}

// Trigger queues a delivery for every active webhook subscribed to event.
// It creates rows in the DB; the dispatcher worker pool picks them up.
//
// Classic webhooks receive payload as the body, as always. Declarative
// subscriptions (RFC 0003) that match also get it, filtered and transformed.
// The bus does not call Trigger: see Bridge, which feeds declarative
// subscriptions only.
func (s *Service) Trigger(ctx context.Context, event string, ownerType string, ownerID uuid.UUID, payload JSONMap) error {
	ev := &Event{Name: event, Payload: map[string]any(payload), OccurredAt: s.clock()}
	if ownerType == "organization" {
		ev.OrgID = ownerID
	}
	_, err := s.dispatchEvent(ctx, ownerType, ownerID, ev, false, payload)
	return err
}

// TriggerEvent queues the event for the declarative subscriptions of the owner
// (classic webhooks are not touched). It returns how many deliveries were
// created or retained. A republished event (same Event.ID) is deduplicated per
// subscription, so it is safe under the bus's at-least-once delivery.
func (s *Service) TriggerEvent(ctx context.Context, ownerType string, ownerID uuid.UUID, ev Event) (int, error) {
	return s.dispatchEvent(ctx, ownerType, ownerID, &ev, true, nil)
}

func (s *Service) dispatchEvent(ctx context.Context, ownerType string, ownerID uuid.UUID, ev *Event, declarativeOnly bool, raw JSONMap) (int, error) {
	var subs []Webhook
	q := s.db.WithContext(ctx).Where("owner_type = ? AND owner_id = ? AND active = ?", ownerType, ownerID, true)
	if declarativeOnly {
		q = q.Where("declarative IS NOT NULL")
	}
	if err := q.Find(&subs).Error; err != nil {
		return 0, err
	}
	var (
		root       map[string]any
		payloadErr error
		dedupe     = ev.ID != ""
		created    int
	)
	now := s.clock()
	for i := range subs {
		w := &subs[i]
		if !containsEvent(w.Events, ev.Name) {
			continue
		}
		if w.Declarative == nil {
			if declarativeOnly {
				continue
			}
			d := &WebhookDelivery{WebhookID: w.ID, Event: ev.Name, Payload: raw, NextAttemptAt: &now}
			if err := s.db.WithContext(ctx).Create(d).Error; err != nil {
				return created, err
			}
			created++
			continue
		}
		if root == nil && payloadErr == nil {
			if ev.ID == "" {
				ev.ID = uuid.NewString()
			}
			root, payloadErr = s.normalizeEvent(ev)
		}
		ok, err := s.enqueueDeclarative(ctx, w, ev, root, payloadErr, dedupe, now)
		if err != nil {
			return created, err
		}
		if ok {
			created++
		}
	}
	return created, nil
}

// normalizeEvent turns the payload into its JSON shape and bounds its size.
func (s *Service) normalizeEvent(ev *Event) (map[string]any, error) {
	b, err := json.Marshal(ev.Payload)
	if err != nil {
		return nil, fmt.Errorf("event payload is not JSON: %w", err)
	}
	if len(b) > maxPayloadBytes {
		return nil, fmt.Errorf("%w (%d bytes, max %d)", ErrPayloadTooLarge, len(b), maxPayloadBytes)
	}
	var m map[string]any
	if string(b) != "null" {
		if err := json.Unmarshal(b, &m); err != nil {
			return nil, fmt.Errorf("event payload is not a JSON object: %w", err)
		}
	}
	if m == nil {
		m = map[string]any{}
	}
	ev.Payload = m
	return ev.view(), nil
}

// validIdempotencyKey: printable ASCII, no spaces, at most 255 bytes. The key
// goes into an HTTP header and a log column; anything else is refused up front
// instead of failing at every attempt.
func validIdempotencyKey(k string) bool {
	if k == "" || len(k) > 255 {
		return false
	}
	for i := 0; i < len(k); i++ {
		if k[i] < 0x21 || k[i] > 0x7e {
			return false
		}
	}
	return true
}

// enqueueDeclarative filters, transforms and stores one delivery. Returns true
// when a row was created (sent, retained, or recorded as not renderable).
func (s *Service) enqueueDeclarative(ctx context.Context, w *Webhook, ev *Event, root map[string]any, payloadErr error, dedupe bool, now time.Time) (bool, error) {
	d := w.Declarative
	c, cerr := d.compile()
	if cerr == nil && payloadErr == nil && !c.filter.match(root) {
		return false, nil
	}
	if dedupe {
		var n int64
		if err := s.db.WithContext(ctx).Model(&WebhookDelivery{}).
			Where("webhook_id = ? AND event_id = ?", w.ID, ev.ID).Count(&n).Error; err != nil {
			return false, err
		}
		if n > 0 {
			return false, nil
		}
	}
	row := &WebhookDelivery{WebhookID: w.ID, Event: ev.Name, EventID: ev.ID, Seq: s.nextSeq()}
	var (
		body map[string]any
		err  error
	)
	switch {
	case payloadErr != nil:
		err = payloadErr
	case cerr != nil:
		err = cerr
	case c.transform != nil:
		body, err = c.transform.render(root)
	default:
		body = c.fieldsBody(root)
	}
	if err == nil && body != nil {
		if d.propagateOrigin() && s.hostID != "" {
			stampOrigin(body, s.hostID, subKey(w), incomingHop(root))
		}
		if corr := correlationOf(ev); corr != "" {
			if e, ok := body["event"].(map[string]any); ok {
				// Copy: root["event"] is shared by every subscription of the event.
				cp := make(map[string]any, len(e)+1)
				for k, v := range e {
					cp[k] = v
				}
				if _, has := cp["correlation"]; !has {
					cp["correlation"] = corr
				}
				body["event"] = cp
			}
			row.RequestHeaders = JSONMap{CorrelationHeader: corr}
		}
	}
	if err == nil {
		if b, merr := json.Marshal(body); merr != nil {
			err = merr
		} else if len(b) > maxPayloadBytes {
			err = fmt.Errorf("%w: rendered body is %d bytes", ErrPayloadTooLarge, len(b))
		}
	}
	if err == nil {
		row.IdempotencyKey, err = renderString(c.idem, root)
		if err == nil && row.IdempotencyKey == "" {
			row.IdempotencyKey = ev.ID
		}
		if err == nil && !validIdempotencyKey(row.IdempotencyKey) {
			err = errors.New("idempotency key is not printable ASCII (1-255 bytes)")
		}
	}
	if err != nil {
		// Not renderable: record it (visible, never silently lost) as a dead,
		// payload-less row that cannot be replayed or requeued, and that does
		// not count against the subscription's health.
		row.Payload = JSONMap{}
		row.PayloadPurged = true
		row.ErrorMessage = "render: " + cleanText(err.Error(), 480)
		if !validIdempotencyKey(row.IdempotencyKey) {
			row.IdempotencyKey = ""
		}
		return s.createDelivery(ctx, row)
	}
	row.Payload = JSONMap(body)
	if d.Ordering == OrderingPerAggregate && ev.AggregateID != "" {
		row.OrderingKey = cleanText(ev.AggregateType+":"+ev.AggregateID, 250)
	}
	if w.IsEnabled() {
		row.NextAttemptAt = &now
	} else {
		row.Held = true // retained: leaves when the subscription is enabled
	}
	return s.createDelivery(ctx, row)
}

// createDelivery inserts the row. With the unique index of migration 0012
// ((webhook_id, event_id) where event_id <> ”) a concurrent duplicate is
// absorbed by ON CONFLICT DO NOTHING and reported as "not created".
func (s *Service) createDelivery(ctx context.Context, row *WebhookDelivery) (bool, error) {
	res := s.db.WithContext(ctx).Clauses(clause.OnConflict{DoNothing: true}).Create(row)
	if res.Error != nil {
		return false, res.Error
	}
	return res.RowsAffected == 1, nil
}

// fieldsBody builds the payload_fields body: the envelope plus ONLY the listed
// scalar fields, rebuilt under their original nesting.
func (c *compiled) fieldsBody(root map[string]any) map[string]any {
	out := map[string]any{}
	for _, f := range c.fields {
		v, ok := f.lookup(root)
		if !ok {
			continue
		}
		cur := out
		for i, seg := range f.segs {
			if i == len(f.segs)-1 {
				cur[seg] = v
				break
			}
			nxt, _ := cur[seg].(map[string]any)
			if nxt == nil {
				nxt = map[string]any{}
				cur[seg] = nxt
			}
			cur = nxt
		}
	}
	return map[string]any{"event": root["event"], "payload": out}
}

// nextSeq is a process-monotonic sequence (unix nanos, bumped on collision).
func (s *Service) nextSeq() int64 {
	s.seqMu.Lock()
	defer s.seqMu.Unlock()
	n := s.clock().UnixNano()
	if n <= s.lastSeq {
		n = s.lastSeq + 1
	}
	s.lastSeq = n
	return n
}

// --- Dispatcher lifecycle -----------------------------------------------

// Start spawns the worker pool. Safe to call multiple times (second call is a
// no-op). Stop() to shut down gracefully.
func (s *Service) Start(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running {
		return nil
	}
	runCtx, cancel := context.WithCancel(ctx)
	s.cancelRun = cancel
	s.running = true
	for i := 0; i < s.workers; i++ {
		s.wg.Add(1)
		go s.loop(runCtx, i == 0)
	}
	return nil
}

// Stop signals workers and waits for them to finish.
func (s *Service) Stop() error {
	s.mu.Lock()
	cancel := s.cancelRun
	running := s.running
	s.mu.Unlock()
	if !running {
		return nil
	}
	cancel()
	s.wg.Wait()
	s.mu.Lock()
	s.running = false
	s.mu.Unlock()
	return nil
}

func (s *Service) loop(ctx context.Context, janitor bool) {
	defer s.wg.Done()
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	ticks := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.drainOnce(ctx)
			ticks++
			if janitor && ticks%150 == 0 { // ~ every 5 minutes
				if _, err := s.PurgeExpired(ctx); err != nil {
					slog.Warn("webhooks: retention purge failed", "err", err)
				}
			}
		}
	}
}

// notBlockedSQL excludes, IN SQL, the deliveries that must wait for an earlier
// one of the same aggregate that is still queued, retained or in flight: they
// never occupy the candidate window, so a failing tenant with ordering cannot
// starve the others. (A predecessor that gave up no longer blocks.)
const notBlockedSQL = `(ordering_key IS NULL OR ordering_key = '' OR NOT EXISTS (
	SELECT 1 FROM webhook_deliveries p
	WHERE p.webhook_id = webhook_deliveries.webhook_id AND p.ordering_key = webhook_deliveries.ordering_key
	  AND p.seq < webhook_deliveries.seq AND p.succeeded = ? AND p.deleted_at IS NULL
	  AND (p.held = ? OR p.next_attempt_at IS NOT NULL OR (p.in_flight = ? AND p.updated_at > ?))))`

type claimed struct {
	d WebhookDelivery
	w Webhook
}

// drainOnce claims up to BatchSize due deliveries (at most PerWebhookConcurrency
// per subscription and PerOwnerConcurrency per owner) and attempts them
// concurrently.
func (s *Service) drainOnce(ctx context.Context) {
	now := s.clock()
	s.reapOrphans(ctx, now)

	window := s.batch * 8
	if window < 50 {
		window = 50
	}
	var cands []WebhookDelivery
	err := s.db.WithContext(ctx).
		Where("succeeded = ? AND (held = ? OR held IS NULL) AND next_attempt_at IS NOT NULL AND next_attempt_at <= ?", false, false, now).
		Where(notBlockedSQL, false, true, true, now.Add(-inFlightLease)).
		Order("next_attempt_at ASC, seq ASC").
		Limit(window).
		Find(&cands).Error
	if err != nil || len(cands) == 0 {
		return
	}
	ids := make([]uuid.UUID, 0, len(cands))
	seen := map[uuid.UUID]bool{}
	for _, c := range cands {
		if !seen[c.WebhookID] {
			seen[c.WebhookID] = true
			ids = append(ids, c.WebhookID)
		}
	}
	var ws []Webhook
	if err := s.db.WithContext(ctx).Where("id IN ?", ids).Find(&ws).Error; err != nil {
		return // transient: nothing claimed yet, nothing to release
	}
	byID := make(map[uuid.UUID]*Webhook, len(ws))
	for i := range ws {
		byID[ws[i].ID] = &ws[i]
	}

	perWH, perOwner := map[uuid.UUID]int{}, map[string]int{}
	var batch []claimed
	for i := range cands {
		if len(batch) >= s.batch {
			break
		}
		d := cands[i]
		w := byID[d.WebhookID]
		if w == nil {
			// The webhook is gone: nothing may stay queued (or hold its payload).
			s.db.WithContext(ctx).Model(&WebhookDelivery{}).Where("id = ? AND succeeded = ?", d.ID, false).
				Updates(purgeFields("webhook no longer exists"))
			continue
		}
		if w.Declarative != nil && !w.IsEnabled() {
			// switched off while queued: retain instead of sending
			s.db.WithContext(ctx).Model(&WebhookDelivery{}).Where("id = ? AND next_attempt_at IS NOT NULL", d.ID).
				Updates(map[string]any{"held": true, "next_attempt_at": nil})
			continue
		}
		ok := w.OwnerType + "|" + w.OwnerID.String()
		if perWH[w.ID] >= s.perWebhook || perOwner[ok] >= s.perOwner {
			continue
		}
		// atomic claim so sibling workers skip it
		res := s.db.WithContext(ctx).Model(&WebhookDelivery{}).
			Where("id = ? AND next_attempt_at IS NOT NULL AND (held = ? OR held IS NULL)", d.ID, false).
			Updates(map[string]any{"next_attempt_at": nil, "in_flight": true})
		if res.Error != nil || res.RowsAffected != 1 {
			continue
		}
		perWH[w.ID]++
		perOwner[ok]++
		d.InFlight = false
		d.NextAttemptAt = nil
		batch = append(batch, claimed{d: d, w: *w})
	}
	var wg sync.WaitGroup
	for i := range batch {
		wg.Add(1)
		go func(c *claimed) {
			defer wg.Done()
			s.attemptDelivery(ctx, &c.w, &c.d)
		}(&batch[i])
	}
	wg.Wait()
}

// reapOrphans re-queues deliveries claimed by a worker that never reported back.
func (s *Service) reapOrphans(ctx context.Context, now time.Time) {
	s.db.WithContext(ctx).Model(&WebhookDelivery{}).
		Where("in_flight = ? AND succeeded = ? AND updated_at < ?", true, false, now.Add(-inFlightLease)).
		Updates(map[string]any{"in_flight": false, "next_attempt_at": now})
}

// attemptDelivery performs one HTTP attempt. On failure, schedules the next
// attempt with exponential backoff until RetryMax.
func (s *Service) attemptDelivery(ctx context.Context, w *Webhook, d *WebhookDelivery) {
	if w.Declarative != nil {
		s.attemptDeclarative(ctx, w, d)
		return
	}
	d.AttemptCount++
	d.InFlight = false
	payloadBytes, _ := jsonMarshal(d.Payload)
	req, err := newRequest(ctx, w, d.Event, payloadBytes, s.clock())
	if err != nil {
		s.persistFailure(ctx, w, d, 0, err.Error(), failOpts{})
		return
	}
	// Stable across retries of the same delivery so receivers can dedupe.
	req.Header.Set("X-Metacore-Delivery", d.ID.String())

	resp, err := s.client.Do(req)
	if err != nil {
		s.persistFailure(ctx, w, d, 0, err.Error(), failOpts{})
		return
	}
	defer resp.Body.Close()

	status := resp.StatusCode
	body := readLimited(resp.Body, 8*1024)

	if status >= 200 && status < 300 {
		s.persistSuccess(ctx, w, d, status, body, false)
		return
	}

	s.persistFailure(ctx, w, d, status, body, failOpts{})
}

// saveDelivery stores d. A response body the database refuses (invalid UTF-8,
// NUL) must never leave the delivery in flight with its attempt unrecorded and
// resend forever: retry without the offending text, then with the bare minimum.
func (s *Service) saveDelivery(ctx context.Context, d *WebhookDelivery) {
	db := s.db.WithContext(ctx)
	if err := db.Save(d).Error; err == nil {
		return
	} else {
		slog.Warn("webhooks: saving delivery failed, retrying without response text", "delivery", d.ID, "err", err)
	}
	d.ResponseBody, d.ErrorMessage = "", "response text could not be stored"
	if err := db.Save(d).Error; err == nil {
		return
	}
	next := s.clock().Add(time.Minute)
	if err := db.Model(&WebhookDelivery{}).Where("id = ?", d.ID).
		Updates(map[string]any{"in_flight": false, "attempt_count": d.AttemptCount, "succeeded": d.Succeeded, "next_attempt_at": nextOrNil(d, next)}).Error; err != nil {
		slog.Error("webhooks: could not persist delivery state", "delivery", d.ID, "err", err)
	}
}

func nextOrNil(d *WebhookDelivery, fallback time.Time) any {
	if d.Succeeded {
		return nil
	}
	if d.NextAttemptAt != nil {
		return *d.NextAttemptAt
	}
	return fallback
}

func (s *Service) persistSuccess(ctx context.Context, w *Webhook, d *WebhookDelivery, status int, body string, purge bool) {
	now := s.clock()
	d.ResponseStatus = status
	d.ResponseBody = cleanText(body, 8*1024)
	d.Succeeded = true
	d.DeliveredAt = &now
	d.NextAttemptAt = nil
	d.InFlight = false
	d.ErrorMessage = ""
	if purge {
		// The retained payload is personal data: erase it once delivered.
		d.Payload = JSONMap{}
		d.PayloadPurged = true
	}
	s.saveDelivery(ctx, d)
	upd := map[string]any{
		"success_count":     gorm.Expr("success_count + 1"),
		"last_triggered_at": now,
	}
	if w.Declarative != nil && d.Event != "webhook.test" {
		upd["consecutive_failures"] = 0
		upd["last_http_status"] = status
		upd["last_error"] = ""
	}
	_ = s.db.WithContext(ctx).Model(&Webhook{}).Where("id = ?", w.ID).Updates(upd).Error
}

// failOpts classifies a failed attempt.
type failOpts struct {
	// permanent: do not retry (dead-letter).
	permanent bool
	// noHealth: does not count as a consecutive failure (the destination is not
	// at fault: bad request data, unresolvable secret, a blocked target...).
	noHealth bool
}

func (s *Service) persistFailure(ctx context.Context, w *Webhook, d *WebhookDelivery, status int, msg string, o failOpts) {
	d.ResponseStatus = status
	d.ErrorMessage = cleanText(msg, 500)
	d.InFlight = false
	if !o.permanent && d.AttemptCount < w.RetryMax {
		// exponential backoff: 1s, 4s, 16s, ...
		shift := uint(2 * d.AttemptCount)
		backoff := time.Duration(1<<shift) * time.Second
		if w.Declarative != nil {
			if shift > 12 || backoff > time.Hour {
				backoff = time.Hour // bounded (RetryMax can be up to 50)
			}
			// +-20% jitter so a target that comes back is not hit by every
			// queued delivery at the same second.
			backoff = time.Duration(float64(backoff) * (0.8 + 0.4*s.rnd()))
		}
		next := s.clock().Add(backoff)
		d.NextAttemptAt = &next
	} else {
		d.NextAttemptAt = nil
	}
	s.saveDelivery(ctx, d)
	if w.Declarative == nil {
		_ = s.db.WithContext(ctx).Model(&Webhook{}).Where("id = ?", w.ID).UpdateColumn("failure_count", gorm.Expr("failure_count + 1")).Error
		return
	}
	s.recordFailureHealth(ctx, w, d, status, o)
}

// defaultAutoDisableThreshold is the consecutive-failure limit when a
// subscription does not set its own.
const defaultAutoDisableThreshold = 10

// recordFailureHealth updates the counters of a declarative subscription with
// ATOMIC statements (no read-modify-write of the row, so a concurrent edit is
// never reverted) and trips the subscription off once consecutive failures reach
// its threshold. Tripping only sets auto_disabled_at: events are then retained,
// not lost; resuming is manual (SetEnabled(true) / Update enabled=true).
func (s *Service) recordFailureHealth(ctx context.Context, w *Webhook, d *WebhookDelivery, status int, o failOpts) {
	now := s.clock()
	db := s.db.WithContext(ctx)
	upd := map[string]any{
		"failure_count":    gorm.Expr("failure_count + 1"),
		"last_failed_at":   now,
		"last_http_status": status,
		"last_error":       cleanText(d.ErrorMessage, 500),
	}
	counts := !o.noHealth && d.Event != "webhook.test"
	if counts {
		upd["consecutive_failures"] = gorm.Expr("consecutive_failures + 1")
	}
	if err := db.Model(&Webhook{}).Where("id = ?", w.ID).Updates(upd).Error; err != nil || !counts {
		return
	}
	thr := w.Declarative.AutoDisableThreshold
	if thr == 0 {
		thr = defaultAutoDisableThreshold
	}
	if thr < 0 {
		return
	}
	res := db.Model(&Webhook{}).
		Where("id = ? AND auto_disabled_at IS NULL AND consecutive_failures >= ?", w.ID, thr).
		Update("auto_disabled_at", now)
	if res.Error != nil || res.RowsAffected != 1 || s.onAutoOff == nil {
		return
	}
	var cur Webhook
	if err := db.First(&cur, "id = ?", w.ID).Error; err == nil {
		s.onAutoOff(ctx, cur)
	}
}

// Requeue puts a dead-lettered delivery (retries exhausted or rejected with a
// 4xx) back in the queue, keeping its id, idempotency key and ordering, with a
// fresh attempt budget. It is the manual "repeat from dead-letter". A delivery
// that was never rendered or whose payload was purged cannot be requeued.
func (s *Service) Requeue(ctx context.Context, deliveryID uuid.UUID) error {
	var d WebhookDelivery
	if err := s.db.WithContext(ctx).First(&d, "id = ?", deliveryID).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			return ErrDeliveryNotFound
		}
		return err
	}
	if d.Succeeded {
		return ErrNotDead
	}
	if d.PayloadPurged {
		return ErrPayloadPurged
	}
	if d.Held || d.NextAttemptAt != nil || d.InFlight {
		return ErrNotDead // still queued, retained or in flight
	}
	now := s.clock()
	return s.db.WithContext(ctx).Model(&WebhookDelivery{}).Where("id = ?", d.ID).
		Updates(map[string]any{"attempt_count": 0, "next_attempt_at": now, "error_message": ""}).Error
}

// --- helpers ------------------------------------------------------------

func validateWebhook(w *Webhook) error {
	if w.URL == "" {
		return ErrInvalidURL
	}
	u, err := url.Parse(w.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return ErrInvalidURL
	}
	if len(w.Events) == 0 {
		return ErrNoEvents
	}
	if w.RetryMax <= 0 {
		w.RetryMax = 3
	}
	if w.TimeoutSec <= 0 {
		w.TimeoutSec = 15
	}
	return nil
}

func generateSecret() string {
	b := make([]byte, 32)
	_, _ = crand.Read(b)
	return hex.EncodeToString(b)
}

// cleanText makes s safe to store: valid UTF-8, no NUL bytes, at most max BYTES
// (cut on a rune boundary).
func cleanText(s string, max int) string {
	s = strings.ToValidUTF8(s, "")
	if strings.IndexByte(s, 0) >= 0 {
		s = strings.ReplaceAll(s, "\x00", "")
	}
	if len(s) <= max {
		return s
	}
	s = s[:max]
	for len(s) > 0 && !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}

func truncate(s string, n int) string { return cleanText(s, n) }

func containsEvent(events StringSlice, e string) bool {
	for _, ev := range events {
		if ev == e || ev == "*" || matchesPattern(ev, e) {
			return true
		}
	}
	return false
}

// matchesPattern: "order.*" matches "order.created".
func matchesPattern(pattern, value string) bool {
	if !strings.HasSuffix(pattern, ".*") {
		return false
	}
	prefix := strings.TrimSuffix(pattern, ".*")
	return strings.HasPrefix(value, prefix+".")
}

func readLimited(r interface{ Read(p []byte) (int, error) }, max int) string {
	buf := make([]byte, max)
	n, _ := r.Read(buf)
	return string(buf[:n])
}

func jsonMarshal(v any) ([]byte, error) { return marshalJSON(v) }
