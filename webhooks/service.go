package webhooks

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// Config wires the service. DB is mandatory.
type Config struct {
	DB          *gorm.DB
	WorkerCount int
	HTTPClient  *http.Client
	// Clock is overridable for tests; defaults to time.Now.
	Clock func() time.Time
}

// Service is the public API. It is transport-agnostic.
type Service struct {
	db      *gorm.DB
	workers int
	client  *http.Client
	clock   func() time.Time

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
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 15 * time.Second}
	}
	if cfg.Clock == nil {
		cfg.Clock = time.Now
	}
	return &Service{
		db:      cfg.DB,
		workers: cfg.WorkerCount,
		client:  cfg.HTTPClient,
		clock:   cfg.Clock,
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
	if w.Secret == "" {
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

func (s *Service) Update(ctx context.Context, w *Webhook) error {
	if err := validateWebhook(w); err != nil {
		return err
	}
	return s.db.WithContext(ctx).Save(w).Error
}

func (s *Service) Delete(ctx context.Context, id uuid.UUID) error {
	return s.db.WithContext(ctx).Delete(&Webhook{}, "id = ?", id).Error
}

// --- Operations ---------------------------------------------------------

// Test sends a test delivery synchronously. Returns the delivery record.
func (s *Service) Test(ctx context.Context, webhookID uuid.UUID) (*WebhookDelivery, error) {
	w, err := s.Get(ctx, webhookID)
	if err != nil {
		return nil, err
	}
	payload := JSONMap{"event": "webhook.test", "webhook_id": w.ID.String(), "timestamp": s.clock().Unix()}
	d := &WebhookDelivery{WebhookID: w.ID, Event: "webhook.test", Payload: payload}
	if err := s.db.WithContext(ctx).Create(d).Error; err != nil {
		return nil, err
	}
	s.attemptDelivery(ctx, w, d)
	return d, nil
}

// Replay re-runs a prior delivery.
func (s *Service) Replay(ctx context.Context, deliveryID uuid.UUID) (*WebhookDelivery, error) {
	var orig WebhookDelivery
	if err := s.db.WithContext(ctx).First(&orig, "id = ?", deliveryID).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			return nil, ErrDeliveryNotFound
		}
		return nil, err
	}
	w, err := s.Get(ctx, orig.WebhookID)
	if err != nil {
		return nil, err
	}
	d := &WebhookDelivery{WebhookID: w.ID, Event: orig.Event, Payload: orig.Payload}
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
func (s *Service) Trigger(ctx context.Context, event string, ownerType string, ownerID uuid.UUID, payload JSONMap) error {
	var subs []Webhook
	if err := s.db.WithContext(ctx).
		Where("owner_type = ? AND owner_id = ? AND active = ?", ownerType, ownerID, true).
		Find(&subs).Error; err != nil {
		return err
	}
	now := s.clock()
	for i := range subs {
		if !containsEvent(subs[i].Events, event) {
			continue
		}
		d := &WebhookDelivery{WebhookID: subs[i].ID, Event: event, Payload: payload, NextAttemptAt: &now}
		if err := s.db.WithContext(ctx).Create(d).Error; err != nil {
			return err
		}
	}
	return nil
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

func (s *Service) drainOnce(ctx context.Context) {
	now := s.clock()
	var d WebhookDelivery
	err := s.db.WithContext(ctx).
		Where("succeeded = ? AND next_attempt_at IS NOT NULL AND next_attempt_at <= ?", false, now).
		Order("next_attempt_at ASC").
		First(&d).Error
	if err != nil {
		return
	}

	// lock: clear NextAttemptAt so siblings skip it
	s.db.WithContext(ctx).Model(&d).Update("next_attempt_at", nil)

	var w Webhook
	if err := s.db.WithContext(ctx).First(&w, "id = ?", d.WebhookID).Error; err != nil {
		return
	}
	s.attemptDelivery(ctx, &w, &d)
}

// attemptDelivery performs one HTTP attempt. On failure, schedules the next
// attempt with exponential backoff until RetryMax.
func (s *Service) attemptDelivery(ctx context.Context, w *Webhook, d *WebhookDelivery) {
	d.AttemptCount++
	payloadBytes, _ := jsonMarshal(d.Payload)
	req, err := newRequest(ctx, w, d.Event, payloadBytes, s.clock())
	if err != nil {
		s.persistFailure(ctx, w, d, 0, err.Error())
		return
	}

	resp, err := s.client.Do(req)
	if err != nil {
		s.persistFailure(ctx, w, d, 0, err.Error())
		return
	}
	defer resp.Body.Close()

	status := resp.StatusCode
	body := readLimited(resp.Body, 8*1024)

	if status >= 200 && status < 300 {
		now := s.clock()
		d.ResponseStatus = status
		d.ResponseBody = body
		d.Succeeded = true
		d.DeliveredAt = &now
		d.NextAttemptAt = nil
		_ = s.db.WithContext(ctx).Save(d).Error
		_ = s.db.WithContext(ctx).Model(w).Updates(map[string]any{
			"success_count":     gorm.Expr("success_count + 1"),
			"last_triggered_at": now,
		}).Error
		return
	}

	s.persistFailure(ctx, w, d, status, body)
}

func (s *Service) persistFailure(ctx context.Context, w *Webhook, d *WebhookDelivery, status int, msg string) {
	d.ResponseStatus = status
	d.ErrorMessage = msg
	if d.AttemptCount < w.RetryMax {
		// exponential backoff: 1s, 4s, 16s, ...
		backoff := time.Duration(1<<uint(2*(d.AttemptCount))) * time.Second
		next := s.clock().Add(backoff)
		d.NextAttemptAt = &next
	} else {
		d.NextAttemptAt = nil
	}
	_ = s.db.WithContext(ctx).Save(d).Error
	_ = s.db.WithContext(ctx).Model(w).UpdateColumn("failure_count", gorm.Expr("failure_count + 1")).Error
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
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
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
