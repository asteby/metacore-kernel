package push

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"time"

	"github.com/golang-jwt/jwt/v5"
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

	// OnExpiredEndpoint, if non-nil, is called instead of the default hard-delete
	// when the push provider returns 404 Not Found or 410 Gone for a subscription.
	// Apps that store their own subscription rows (e.g. with an is_active column
	// or org-scoped soft-delete) can use this hook to flip their own state and
	// skip the kernel's own Unsubscribe.
	OnExpiredEndpoint func(ctx context.Context, sub *PushSubscription) error
}

// Service is the transport-agnostic Push API.
type Service struct {
	db         *gorm.DB
	pub        string
	vapidPriv  *ecdh.PrivateKey  // for payload encryption
	vapidECDSA *ecdsa.PrivateKey // for JWT signing
	subject    string
	client     *http.Client
	onExpired  func(ctx context.Context, sub *PushSubscription) error
}

// New returns a Service. If VAPIDPrivate is set it is parsed; if empty, Send
// will still work but payloads are delivered unencrypted (useful in tests /
// push services that don't require encryption).
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
	svc := &Service{
		db:        cfg.DB,
		pub:       cfg.VAPIDPublic,
		subject:   cfg.VAPIDSubject,
		client:    cfg.HTTPClient,
		onExpired: cfg.OnExpiredEndpoint,
	}
	if cfg.VAPIDPrivate != "" {
		privBytes, err := base64.RawURLEncoding.DecodeString(cfg.VAPIDPrivate)
		if err == nil {
			curve := ecdh.P256()
			privKey, err := curve.NewPrivateKey(privBytes)
			if err == nil {
				svc.vapidPriv = privKey
				svc.vapidECDSA, _ = ecdhToECDSA(privKey)
			}
		}
	}
	return svc
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
	Title    string         `json:"title"`
	Body     string         `json:"body"`
	Icon     string         `json:"icon,omitempty"`
	Badge    string         `json:"badge,omitempty"`
	Image    string         `json:"image,omitempty"`
	URL      string         `json:"url,omitempty"`
	Tag      string         `json:"tag,omitempty"`
	Data     map[string]any `json:"data,omitempty"`
	Actions  []Action       `json:"actions,omitempty"`
	Vibrate  []int          `json:"vibrate,omitempty"`
	Silent   bool           `json:"silent,omitempty"`
	Renotify bool           `json:"renotify,omitempty"`
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

// Send delivers to a single subscription using AES128GCM payload encryption
// and a proper VAPID JWT (RFC 8292). Returns the HTTP status observed.
func (s *Service) Send(ctx context.Context, sub *PushSubscription, p Payload) (int, error) {
	plaintext, err := json.Marshal(p)
	if err != nil {
		return 0, err
	}

	var body []byte
	contentType := "application/json"
	contentEncoding := ""

	if s.vapidPriv != nil && sub.P256DH != "" && sub.Auth != "" {
		enc, err := encryptPayload(sub.P256DH, sub.Auth, plaintext)
		if err != nil {
			return 0, fmt.Errorf("push: encrypt: %w", err)
		}
		body = enc.ciphertext
		contentType = "application/octet-stream"
		contentEncoding = "aes128gcm"
	} else {
		body = plaintext
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, sub.Endpoint, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("TTL", "86400")
	req.Header.Set("Urgency", "high")
	if contentEncoding != "" {
		req.Header.Set("Content-Encoding", contentEncoding)
	}

	if s.vapidECDSA != nil && s.pub != "" {
		token, err := s.createVAPIDToken(sub.Endpoint)
		if err != nil {
			return 0, fmt.Errorf("push: vapid jwt: %w", err)
		}
		req.Header.Set("Authorization", fmt.Sprintf("vapid t=%s, k=%s", token, s.pub))
	}

	resp, err := s.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	status := resp.StatusCode
	if IsExpiredStatus(status) {
		if s.onExpired != nil {
			_ = s.onExpired(ctx, sub)
		} else {
			_ = s.Unsubscribe(ctx, sub.Endpoint)
		}
	}
	return status, nil
}

// IsExpiredStatus reports whether an HTTP response status from a push endpoint
// means the subscription is permanently unavailable (404 Not Found or 410 Gone),
// per RFC 8030. Useful for apps that call Send directly and want to react
// without wiring the OnExpiredEndpoint hook.
func IsExpiredStatus(status int) bool {
	return status == http.StatusNotFound || status == http.StatusGone
}

// createVAPIDToken produces a signed ES256 JWT for the push endpoint's origin.
func (s *Service) createVAPIDToken(endpoint string) (string, error) {
	audience := endpoint
	slashCount := 0
	for i, c := range endpoint {
		if c == '/' {
			slashCount++
			if slashCount == 3 {
				audience = endpoint[:i]
				break
			}
		}
	}
	claims := jwt.MapClaims{
		"aud": audience,
		"exp": time.Now().Add(12 * time.Hour).Unix(),
		"sub": s.subject,
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodES256, claims)
	return tok.SignedString(s.vapidECDSA)
}

// ecdhToECDSA converts a P-256 ECDH private key to an ECDSA key for JWT signing.
func ecdhToECDSA(key *ecdh.PrivateKey) (*ecdsa.PrivateKey, error) {
	privBytes := key.Bytes()
	pubBytes := key.PublicKey().Bytes()
	if len(pubBytes) != 65 || pubBytes[0] != 0x04 {
		return nil, errors.New("push: unexpected public key format")
	}
	return &ecdsa.PrivateKey{
		PublicKey: ecdsa.PublicKey{
			Curve: elliptic.P256(),
			X:     new(big.Int).SetBytes(pubBytes[1:33]),
			Y:     new(big.Int).SetBytes(pubBytes[33:65]),
		},
		D: new(big.Int).SetBytes(privBytes),
	}, nil
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

