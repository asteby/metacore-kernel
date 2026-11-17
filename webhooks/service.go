package webhooks

import (
	"context"
	crand "crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/asteby/metacore-kernel/runtime/webhookout"
)

// Config wires the service. DB is mandatory.
type Config struct {
	DB          *gorm.DB
	WorkerCount int
	HTTPClient  *http.Client
	// Clock is overridable for tests; defaults to time.Now.
	Clock func() time.Time

	// --- Declarative subscriptions (RFC 0003). All optional: without them
	// classic webhooks behave exactly as before. ---

	// Schemas declares the payload paths of each event so a transform/filter
	// naming a path that does not exist fails AT SAVE. Without it, a
	// declarative subscription may only reference `event.*` paths.
	Schemas SchemaSource
	// Secrets resolves `secret_ref` / `key_ref` of signing profiles. Required
	// to save a subscription that declares Signing.
	Secrets SecretResolver
	// AllowHosts, when non-empty, restricts targets to these patterns
	// ("api.example.com", "*.example.com") on top of the netguard rules.
	AllowHosts []string
	// EgressClient is the client used for declarative deliveries. Default: the
	// HTTPClient if given, else the webhookout egress client (https/public hosts only,
	// checked on every connection; redirects are not followed).
	EgressClient *http.Client
	// UnsafeAllowPrivateTargets disables target validation. TESTS ONLY.
	UnsafeAllowPrivateTargets bool
	// OnAutoDisable is called (once, after the row is saved) when a declarative
	// subscription switched itself off after too many consecutive failures. Use
	// it to raise the alert. It must not block.
	OnAutoDisable func(ctx context.Context, w Webhook)
	// Rand returns a value in [0,1) for retry jitter; default math/rand/v2.
	Rand func() float64
}

// inFlightLease is how long a claimed delivery counts as in progress before it
// is considered orphaned (worker crash) and re-queued.
const inFlightLease = 5 * time.Minute

// Service is the public API. It is transport-agnostic.
type Service struct {
	db      *gorm.DB
	workers int
	client  *http.Client
	egress  *http.Client
	clock   func() time.Time

	schemas      SchemaSource
	secrets      SecretResolver
	allowHosts   []string
	allowPrivate bool
	onAutoOff    func(ctx context.Context, w Webhook)
	rnd          func() float64

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
	egress := cfg.EgressClient
	if egress == nil {
		egress = cfg.HTTPClient
	}
	if egress == nil {
		egress = webhookout.Policy{AllowHosts: cfg.AllowHosts, AllowPathPrefixes: []string{"/"}}.NewClient()
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 15 * time.Second}
	}
	if cfg.Clock == nil {
		cfg.Clock = time.Now
	}
	if cfg.Rand == nil {
		cfg.Rand = rand.Float64
	}
	return &Service{
		onAutoOff:    cfg.OnAutoDisable,
		rnd:          cfg.Rand,
		db:           cfg.DB,
		workers:      cfg.WorkerCount,
		client:       cfg.HTTPClient,
		egress:       egress,
		clock:        cfg.Clock,
		schemas:      cfg.Schemas,
		secrets:      cfg.Secrets,
		allowHosts:   cfg.AllowHosts,
		allowPrivate: cfg.UnsafeAllowPrivateTargets,
	}
}

// LogsParams filters WebhookDelivery listings.
type LogsParams struct {
	Page    int
	PerPage int
	Event   string
	Success *bool
}

// --- CRUD ---------------------------------------------------------------

func (s *Service) Create(ctx context.Context, w *Webhook) error {
	if err := validateWebhook(w); err != nil {
		return err
	}
	if err := s.validateDeclarative(w); err != nil {
		return err
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

// Update saves w. Turning a declarative subscription on (Enabled false -> true)
// releases the events retained while it was off.
func (s *Service) Update(ctx context.Context, w *Webhook) error {
	if err := validateWebhook(w); err != nil {
		return err
	}
	if err := s.validateDeclarative(w); err != nil {
		return err
	}
	var prev Webhook
	hadHeld := false
	if err := s.db.WithContext(ctx).First(&prev, "id = ?", w.ID).Error; err == nil {
		hadHeld = prev.Declarative != nil && !prev.Declarative.Enabled
	}
	if err := s.db.WithContext(ctx).Save(w).Error; err != nil {
		return err
	}
	if hadHeld && (w.Declarative == nil || w.Declarative.Enabled) {
		_, err := s.releaseHeld(ctx, w.ID)
		return err
	}
	return nil
}

func (s *Service) Delete(ctx context.Context, id uuid.UUID) error {
	return s.db.WithContext(ctx).Delete(&Webhook{}, "id = ?", id).Error
}

// SetEnabled turns a declarative subscription on or off. Turning it on releases
// every event retained while it was off (returns how many); turning it off makes
// new events (and queued ones) be retained instead of sent.
func (s *Service) SetEnabled(ctx context.Context, id uuid.UUID, enabled bool) (int64, error) {
	w, err := s.Get(ctx, id)
	if err != nil {
		return 0, err
	}
	if w.Declarative == nil {
		return 0, ErrNotDeclarative
	}
	w.Declarative.Enabled = enabled
	if enabled { // manual resume clears the health trip
		w.ConsecutiveFailures, w.AutoDisabledAt, w.LastError = 0, nil, ""
	}
	if err := s.db.WithContext(ctx).Save(w).Error; err != nil {
		return 0, err
	}
	if !enabled {
		return 0, nil
	}
	return s.releaseHeld(ctx, id)
}

func (s *Service) releaseHeld(ctx context.Context, id uuid.UUID) (int64, error) {
	now := s.clock()
	res := s.db.WithContext(ctx).Model(&WebhookDelivery{}).
		Where("webhook_id = ? AND held = ? AND succeeded = ?", id, true, false).
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

// --- Operations ---------------------------------------------------------

// Test sends a test delivery synchronously. Returns the delivery record.
// (Also works on a disabled declarative subscription: it is an explicit action.)
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
// "replayed" instead of duplicating. A delivery whose payload was purged after
// being delivered cannot be replayed (ErrPayloadPurged).
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
	d := &WebhookDelivery{WebhookID: w.ID, Event: orig.Event, Payload: orig.Payload,
		EventID: orig.EventID, IdempotencyKey: orig.IdempotencyKey}
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
	if err := s.db.WithContext(ctx).
		Where("owner_type = ? AND owner_id = ? AND active = ?", ownerType, ownerID, true).
		Find(&subs).Error; err != nil {
		return 0, err
	}
	var (
		root    map[string]any
		dedupe  = ev.ID != ""
		created int
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
		if root == nil {
			if ev.ID == "" {
				ev.ID = uuid.NewString()
			}
			norm, err := normalizeJSON(ev.Payload)
			if err != nil {
				return created, fmt.Errorf("webhooks: event payload is not JSON: %w", err)
			}
			m, _ := norm.(map[string]any)
			if m == nil {
				m = map[string]any{}
			}
			ev.Payload = m
			root = ev.view()
		}
		ok, err := s.enqueueDeclarative(ctx, w, ev, root, dedupe, now)
		if err != nil {
			return created, err
		}
		if ok {
			created++
		}
	}
	return created, nil
}

// enqueueDeclarative filters, transforms and stores one delivery. Returns true
// when a row was created (sent or retained).
func (s *Service) enqueueDeclarative(ctx context.Context, w *Webhook, ev *Event, root map[string]any, dedupe bool, now time.Time) (bool, error) {
	d := w.Declarative
	c, cerr := d.compile()
	if cerr == nil && !c.filter.match(root) {
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
	row := &WebhookDelivery{
		WebhookID: w.ID, Event: ev.Name, EventID: ev.ID, Seq: s.nextSeq(),
	}
	var body map[string]any
	var err error
	if cerr != nil {
		err = cerr
	} else if c.transform != nil {
		body, err = c.transform.render(root)
	} else {
		body = map[string]any{"event": root["event"], "payload": root["payload"]}
	}
	if err == nil {
		row.IdempotencyKey, err = renderString(c.idem, root)
		if err == nil && row.IdempotencyKey == "" {
			row.IdempotencyKey = ev.ID
		}
	}
	if err != nil {
		// Not retryable: record it so the operator sees it, never lose it silently.
		row.Payload = JSONMap{}
		row.ErrorMessage = truncate("render: "+err.Error(), 500)
		row.IdempotencyKey = ev.ID
		return true, s.db.WithContext(ctx).Create(row).Error
	}
	row.Payload = JSONMap(body)
	if d.Ordering == OrderingPerAggregate && ev.AggregateID != "" {
		row.OrderingKey = ev.AggregateType + ":" + ev.AggregateID
	}
	if d.Enabled {
		row.NextAttemptAt = &now
	} else {
		row.Held = true // retained: leaves when the subscription is enabled
	}
	return true, s.db.WithContext(ctx).Create(row).Error
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
		go s.loop(runCtx)
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

func (s *Service) loop(ctx context.Context) {
	defer s.wg.Done()
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.drainOnce(ctx)
		}
	}
}

// drainOnce claims and attempts at most one due delivery.
func (s *Service) drainOnce(ctx context.Context) {
	now := s.clock()
	s.reapOrphans(ctx, now)

	var cands []WebhookDelivery
	err := s.db.WithContext(ctx).
		Where("succeeded = ? AND (held = ? OR held IS NULL) AND next_attempt_at IS NOT NULL AND next_attempt_at <= ?", false, false, now).
		Order("next_attempt_at ASC, seq ASC").
		Limit(25).
		Find(&cands).Error
	if err != nil {
		return
	}
	for i := range cands {
		d := cands[i]
		if d.OrderingKey != "" && s.hasLivePredecessor(ctx, &d, now) {
			continue
		}
		// lock: atomic claim so sibling workers skip it
		res := s.db.WithContext(ctx).Model(&WebhookDelivery{}).
			Where("id = ? AND next_attempt_at IS NOT NULL AND (held = ? OR held IS NULL)", d.ID, false).
			Updates(map[string]any{"next_attempt_at": nil, "in_flight": true})
		if res.Error != nil || res.RowsAffected != 1 {
			continue
		}
		var w Webhook
		if err := s.db.WithContext(ctx).First(&w, "id = ?", d.WebhookID).Error; err != nil {
			return
		}
		if w.Declarative != nil && !w.Declarative.Enabled {
			// switched off while queued: retain instead of sending
			s.db.WithContext(ctx).Model(&WebhookDelivery{}).Where("id = ?", d.ID).
				Updates(map[string]any{"held": true, "in_flight": false})
			continue
		}
		d.InFlight = false
		d.NextAttemptAt = nil
		s.attemptDelivery(ctx, &w, &d)
		return
	}
}

// hasLivePredecessor: an earlier delivery of the same aggregate on the same
// subscription that is still queued, retained or in flight blocks this one.
// A delivery that gave up (retries exhausted) no longer blocks.
func (s *Service) hasLivePredecessor(ctx context.Context, d *WebhookDelivery, now time.Time) bool {
	var n int64
	s.db.WithContext(ctx).Model(&WebhookDelivery{}).
		Where("webhook_id = ? AND ordering_key = ? AND seq < ? AND id <> ? AND succeeded = ?",
			d.WebhookID, d.OrderingKey, d.Seq, d.ID, false).
		Where("(held = ? OR next_attempt_at IS NOT NULL OR (in_flight = ? AND updated_at > ?))",
			true, true, now.Add(-inFlightLease)).
		Count(&n)
	return n > 0
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
		s.persistFailure(ctx, w, d, 0, err.Error(), false)
		return
	}
	// Stable across retries of the same delivery so receivers can dedupe.
	req.Header.Set("X-Metacore-Delivery", d.ID.String())

	resp, err := s.client.Do(req)
	if err != nil {
		s.persistFailure(ctx, w, d, 0, err.Error(), false)
		return
	}
	defer resp.Body.Close()

	status := resp.StatusCode
	body := readLimited(resp.Body, 8*1024)

	if status >= 200 && status < 300 {
		s.persistSuccess(ctx, w, d, status, body, false)
		return
	}

	s.persistFailure(ctx, w, d, status, body, false)
}

func (s *Service) persistSuccess(ctx context.Context, w *Webhook, d *WebhookDelivery, status int, body string, purge bool) {
	now := s.clock()
	d.ResponseStatus = status
	d.ResponseBody = body
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
	_ = s.db.WithContext(ctx).Save(d).Error
	upd := map[string]any{
		"success_count":     gorm.Expr("success_count + 1"),
		"last_triggered_at": now,
	}
	if w.Declarative != nil {
		upd["consecutive_failures"] = 0
		upd["last_http_status"] = status
		upd["last_error"] = ""
	}
	_ = s.db.WithContext(ctx).Model(w).Updates(upd).Error
}

func (s *Service) persistFailure(ctx context.Context, w *Webhook, d *WebhookDelivery, status int, msg string, permanent bool) {
	d.ResponseStatus = status
	d.ErrorMessage = truncate(msg, 500)
	d.InFlight = false
	if !permanent && d.AttemptCount < w.RetryMax {
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
	_ = s.db.WithContext(ctx).Save(d).Error
	if w.Declarative == nil {
		_ = s.db.WithContext(ctx).Model(w).UpdateColumn("failure_count", gorm.Expr("failure_count + 1")).Error
		return
	}
	s.recordFailureHealth(ctx, w, status, d.ErrorMessage)
}

// defaultAutoDisableThreshold is the consecutive-failure limit when a
// subscription does not set its own.
const defaultAutoDisableThreshold = 10

// recordFailureHealth updates the counters of a declarative subscription and
// switches it off (events are then retained, not lost) once consecutive
// failures reach its threshold. Resuming is manual: SetEnabled(true).
func (s *Service) recordFailureHealth(ctx context.Context, w *Webhook, status int, msg string) {
	now := s.clock()
	cons := w.ConsecutiveFailures + 1
	// re-read: another delivery may have moved the counter meanwhile
	var cur Webhook
	if err := s.db.WithContext(ctx).Select("consecutive_failures").First(&cur, "id = ?", w.ID).Error; err == nil {
		cons = cur.ConsecutiveFailures + 1
	}
	w.ConsecutiveFailures = cons
	upd := map[string]any{
		"failure_count":        gorm.Expr("failure_count + 1"),
		"consecutive_failures": cons,
		"last_failed_at":       now,
		"last_http_status":     status,
		"last_error":           truncate(msg, 500),
	}
	thr := w.Declarative.AutoDisableThreshold
	if thr == 0 {
		thr = defaultAutoDisableThreshold
	}
	tripped := thr > 0 && cons >= thr && w.Declarative.Enabled
	if tripped {
		w.Declarative.Enabled = false
		if b, err := json.Marshal(w.Declarative); err == nil {
			upd["declarative"] = string(b)
		}
		upd["auto_disabled_at"] = now
	}
	if err := s.db.WithContext(ctx).Model(&Webhook{}).Where("id = ?", w.ID).Updates(upd).Error; err != nil {
		return
	}
	if tripped && s.onAutoOff != nil {
		cp := *w
		s.onAutoOff(ctx, cp)
	}
}

// Requeue puts a dead-lettered delivery (retries exhausted or rejected with a
// 4xx) back in the queue, keeping its id, idempotency key and ordering, with a
// fresh attempt budget. It is the manual "repeat from dead-letter".
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

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

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
