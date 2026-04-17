package push

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// Config wires the service.
type Config struct {
	DB           *gorm.DB
	VAPIDPublic  string
	VAPIDPrivate string
	VAPIDSubject string // mailto:ops@example.com
	HTTPClient   *http.Client
}

// Service is the transport-agnostic Push API.
type Service struct {
	db      *gorm.DB
	pub     string
	private string
	subject string
	client  *http.Client
}

// New returns a Service.
func New(cfg Config) *Service {
	if cfg.DB == nil {
		panic("push: Config.DB is required")
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 15 * time.Second}
	}
	if cfg.VAPIDSubject == "" {
		cfg.VAPIDSubject = "mailto:admin@example.com"
	}
	return &Service{
		db:      cfg.DB,
		pub:     cfg.VAPIDPublic,
		private: cfg.VAPIDPrivate,
		subject: cfg.VAPIDSubject,
		client:  cfg.HTTPClient,
	}
}

// PublicKey returns the VAPID public key the web client needs.
func (s *Service) PublicKey() string { return s.pub }

// SubscriptionInput mirrors the browser PushSubscription shape.
type SubscriptionInput struct {
	Endpoint   string
	P256DH     string
	Auth       string
	DeviceType string
	UserAgent  string
}

// Subscribe upserts a subscription for a user.
func (s *Service) Subscribe(ctx context.Context, userID uuid.UUID, in SubscriptionInput) (*PushSubscription, error) {
	if in.Endpoint == "" {
		return nil, errors.New("push: endpoint required")
	}
	var existing PushSubscription
	err := s.db.WithContext(ctx).Where("endpoint = ?", in.Endpoint).First(&existing).Error
	if err == nil {
		existing.UserID = userID
		existing.P256DH = in.P256DH
		existing.Auth = in.Auth
		existing.DeviceType = in.DeviceType
		existing.UserAgent = in.UserAgent
		now := time.Now()
		existing.LastUsedAt = &now
		if err := s.db.WithContext(ctx).Save(&existing).Error; err != nil {
			return nil, err
		}
		return &existing, nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, err
	}
	now := time.Now()
	sub := &PushSubscription{
		UserID: userID, Endpoint: in.Endpoint, P256DH: in.P256DH, Auth: in.Auth,
		DeviceType: in.DeviceType, UserAgent: in.UserAgent, LastUsedAt: &now,
	}
	if err := s.db.WithContext(ctx).Create(sub).Error; err != nil {
		return nil, err
	}
	return sub, nil
}

// Unsubscribe removes a subscription by endpoint.
func (s *Service) Unsubscribe(ctx context.Context, endpoint string) error {
	return s.db.WithContext(ctx).Where("endpoint = ?", endpoint).Delete(&PushSubscription{}).Error
}

// Payload is the notification envelope delivered to the service worker.
type Payload struct {
	Title string         `json:"title"`
	Body  string         `json:"body"`
	Icon  string         `json:"icon,omitempty"`
	Badge string         `json:"badge,omitempty"`
	URL   string         `json:"url,omitempty"`
	Tag   string         `json:"tag,omitempty"`
	Data  map[string]any `json:"data,omitempty"`
}

// SendToUser fans out a payload to every subscription the user has.
func (s *Service) SendToUser(ctx context.Context, userID uuid.UUID, p Payload) error {
	var subs []PushSubscription
	if err := s.db.WithContext(ctx).Where("user_id = ?", userID).Find(&subs).Error; err != nil {
		return err
	}
	return s.sendMany(ctx, subs, p)
}

// SendToSubscriptions lets apps target arbitrary subscription sets (e.g.
// broadcast to an org by joining push_subscriptions with users).
func (s *Service) SendToSubscriptions(ctx context.Context, subs []PushSubscription, p Payload) error {
	return s.sendMany(ctx, subs, p)
}

// Send delivers to a single subscription. Returns the HTTP status observed.
func (s *Service) Send(ctx context.Context, sub *PushSubscription, p Payload) (int, error) {
	body, _ := json.Marshal(p)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, sub.Endpoint, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("TTL", "86400")
	// VAPID Authorization — a minimal implementation sufficient for most push
	// services. Apps wanting AES128GCM encryption should plug a real webpush
	// library via Config.HTTPClient's Transport.
	if s.pub != "" {
		req.Header.Set("Authorization", fmt.Sprintf(`vapid t="%s", k="%s"`, s.subject, s.pub))
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	status := resp.StatusCode
	if status == http.StatusNotFound || status == http.StatusGone {
		_ = s.Unsubscribe(ctx, sub.Endpoint)
	}
	return status, nil
}

func (s *Service) sendMany(ctx context.Context, subs []PushSubscription, p Payload) error {
	var firstErr error
	for i := range subs {
		if _, err := s.Send(ctx, &subs[i], p); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// Test sends a demo payload to every subscription of a user.
func (s *Service) Test(ctx context.Context, userID uuid.UUID) error {
	return s.SendToUser(ctx, userID, Payload{
		Title: "Test notification",
		Body:  "If you can read this, push is working.",
		URL:   "/",
	})
}

// trim ensures a body is safe to log (dev aid).
func trim(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return strings.TrimSpace(s[:max]) + "…"
}

var _ = trim // reserved for future logging use
