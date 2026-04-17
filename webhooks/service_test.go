package webhooks

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func setupTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{
		DisableForeignKeyConstraintWhenMigrating: true,
	})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	// SQLite does not support gen_random_uuid(), so we create tables manually
	// and rely on the BeforeCreate hook to generate UUIDs.
	db.Exec(`CREATE TABLE IF NOT EXISTS webhooks (
		id TEXT PRIMARY KEY,
		organization_id TEXT,
		created_by_id TEXT,
		created_at DATETIME,
		updated_at DATETIME,
		deleted_at DATETIME,
		name TEXT,
		url TEXT,
		events TEXT,
		secret TEXT,
		active INTEGER DEFAULT 1,
		retry_max INTEGER DEFAULT 3,
		timeout_sec INTEGER DEFAULT 15,
		owner_type TEXT,
		owner_id TEXT,
		last_triggered_at DATETIME,
		failure_count INTEGER DEFAULT 0,
		success_count INTEGER DEFAULT 0
	)`)
	db.Exec(`CREATE TABLE IF NOT EXISTS webhook_deliveries (
		id TEXT PRIMARY KEY,
		organization_id TEXT,
		created_by_id TEXT,
		created_at DATETIME,
		updated_at DATETIME,
		deleted_at DATETIME,
		webhook_id TEXT,
		event TEXT,
		payload TEXT,
		request_headers TEXT,
		response_status INTEGER DEFAULT 0,
		response_body TEXT,
		response_headers TEXT,
		attempt_count INTEGER DEFAULT 0,
		succeeded INTEGER DEFAULT 0,
		error_message TEXT,
		delivered_at DATETIME,
		next_attempt_at DATETIME
	)`)
	return db
}

func setupService(t *testing.T, db *gorm.DB, client *http.Client) *Service {
	t.Helper()
	cfg := Config{DB: db, Clock: time.Now}
	if client != nil {
		cfg.HTTPClient = client
	}
	return New(cfg)
}

func newWebhook(url string, events ...string) *Webhook {
	return &Webhook{
		Name:      "test",
		URL:       url,
		Events:    StringSlice(events),
		Active:    true,
		OwnerType: "org",
		OwnerID:   uuid.New(),
	}
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestCreateWebhook(t *testing.T) {
	db := setupTestDB(t)
	svc := setupService(t, db, nil)

	w := newWebhook("https://example.com/hook", "order.created")
	if err := svc.Create(context.Background(), w); err != nil {
		t.Fatalf("create: %v", err)
	}

	if w.Secret == "" {
		t.Fatal("expected auto-generated secret")
	}
	if w.ID == uuid.Nil {
		t.Fatal("expected non-nil ID")
	}
	if !w.Active {
		t.Fatal("expected active = true")
	}
}

func TestCreateValidation(t *testing.T) {
	db := setupTestDB(t)
	svc := setupService(t, db, nil)

	// Empty URL
	w := newWebhook("", "order.created")
	if err := svc.Create(context.Background(), w); err != ErrInvalidURL {
		t.Fatalf("expected ErrInvalidURL for empty URL, got %v", err)
	}

	// No events
	w2 := newWebhook("https://example.com/hook")
	if err := svc.Create(context.Background(), w2); err != ErrNoEvents {
		t.Fatalf("expected ErrNoEvents for empty events, got %v", err)
	}
}

func TestList(t *testing.T) {
	db := setupTestDB(t)
	svc := setupService(t, db, nil)

	ownerType := "org"
	ownerID := uuid.New()

	for i := 0; i < 2; i++ {
		w := &Webhook{
			Name:      "hook",
			URL:       "https://example.com/hook",
			Events:    StringSlice{"order.created"},
			Active:    true,
			OwnerType: ownerType,
			OwnerID:   ownerID,
		}
		if err := svc.Create(context.Background(), w); err != nil {
			t.Fatalf("create %d: %v", i, err)
		}
	}

	list, err := svc.List(context.Background(), ownerType, ownerID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("len = %d, want 2", len(list))
	}
}

func TestTrigger(t *testing.T) {
	db := setupTestDB(t)
	svc := setupService(t, db, nil)

	ownerType := "org"
	ownerID := uuid.New()

	w := &Webhook{
		Name:      "trigger-hook",
		URL:       "https://example.com/hook",
		Events:    StringSlice{"order.created"},
		Active:    true,
		OwnerType: ownerType,
		OwnerID:   ownerID,
	}
	if err := svc.Create(context.Background(), w); err != nil {
		t.Fatalf("create: %v", err)
	}

	payload := JSONMap{"order_id": "123"}
	if err := svc.Trigger(context.Background(), "order.created", ownerType, ownerID, payload); err != nil {
		t.Fatalf("trigger: %v", err)
	}

	var deliveries []WebhookDelivery
	if err := db.Find(&deliveries).Error; err != nil {
		t.Fatalf("find deliveries: %v", err)
	}
	if len(deliveries) != 1 {
		t.Fatalf("deliveries = %d, want 1", len(deliveries))
	}
	if deliveries[0].Event != "order.created" {
		t.Fatalf("event = %q, want order.created", deliveries[0].Event)
	}
}

func TestTriggerIgnoresUnsubscribedEvent(t *testing.T) {
	db := setupTestDB(t)
	svc := setupService(t, db, nil)

	ownerType := "org"
	ownerID := uuid.New()

	w := &Webhook{
		Name:      "hook",
		URL:       "https://example.com/hook",
		Events:    StringSlice{"order.created"},
		Active:    true,
		OwnerType: ownerType,
		OwnerID:   ownerID,
	}
	if err := svc.Create(context.Background(), w); err != nil {
		t.Fatalf("create: %v", err)
	}

	// Trigger a different event
	if err := svc.Trigger(context.Background(), "order.deleted", ownerType, ownerID, JSONMap{}); err != nil {
		t.Fatalf("trigger: %v", err)
	}

	var count int64
	db.Model(&WebhookDelivery{}).Count(&count)
	if count != 0 {
		t.Fatalf("deliveries = %d, want 0 for unsubscribed event", count)
	}
}

func TestTest(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	db := setupTestDB(t)
	svc := setupService(t, db, ts.Client())

	w := newWebhook(ts.URL+"/hook", "webhook.test")
	if err := svc.Create(context.Background(), w); err != nil {
		t.Fatalf("create: %v", err)
	}

	d, err := svc.Test(context.Background(), w.ID)
	if err != nil {
		t.Fatalf("test: %v", err)
	}
	if !d.Succeeded {
		t.Fatalf("delivery should have succeeded, error: %s", d.ErrorMessage)
	}
	if d.Event != "webhook.test" {
		t.Fatalf("event = %q, want webhook.test", d.Event)
	}
}

func TestHMACSignature(t *testing.T) {
	var mu sync.Mutex
	var capturedHeaders http.Header

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		capturedHeaders = r.Header.Clone()
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	db := setupTestDB(t)
	svc := setupService(t, db, ts.Client())

	w := newWebhook(ts.URL+"/hook", "webhook.test")
	if err := svc.Create(context.Background(), w); err != nil {
		t.Fatalf("create: %v", err)
	}

	d, err := svc.Test(context.Background(), w.ID)
	if err != nil {
		t.Fatalf("test: %v", err)
	}
	if !d.Succeeded {
		t.Fatalf("delivery failed: %s", d.ErrorMessage)
	}

	mu.Lock()
	headers := capturedHeaders
	mu.Unlock()

	sig := headers.Get("X-Metacore-Signature")
	if sig == "" {
		t.Fatal("X-Metacore-Signature header missing")
	}
	ts2 := headers.Get("X-Metacore-Timestamp")
	if ts2 == "" {
		t.Fatal("X-Metacore-Timestamp header missing")
	}

	// Recompute HMAC and verify
	payloadBytes, _ := json.Marshal(d.Payload)
	mac := hmac.New(sha256.New, []byte(w.Secret))
	mac.Write([]byte(ts2))
	mac.Write([]byte("."))
	mac.Write(payloadBytes)
	expected := hex.EncodeToString(mac.Sum(nil))

	if sig != expected {
		t.Fatalf("signature mismatch:\n  got:  %s\n  want: %s", sig, expected)
	}
}

func TestDeleteWebhook(t *testing.T) {
	db := setupTestDB(t)
	svc := setupService(t, db, nil)

	w := newWebhook("https://example.com/hook", "test.event")
	if err := svc.Create(context.Background(), w); err != nil {
		t.Fatalf("create: %v", err)
	}

	if err := svc.Delete(context.Background(), w.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}

	_, err := svc.Get(context.Background(), w.ID)
	if err != ErrWebhookNotFound {
		t.Fatalf("expected ErrWebhookNotFound after delete, got %v", err)
	}
}

func TestGetWebhook(t *testing.T) {
	db := setupTestDB(t)
	svc := setupService(t, db, nil)

	w := newWebhook("https://example.com/hook", "test.event")
	if err := svc.Create(context.Background(), w); err != nil {
		t.Fatalf("create: %v", err)
	}

	got, err := svc.Get(context.Background(), w.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.URL != "https://example.com/hook" {
		t.Fatalf("url = %q", got.URL)
	}
}

func TestReplay(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = body
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	db := setupTestDB(t)
	svc := setupService(t, db, ts.Client())

	w := newWebhook(ts.URL+"/hook", "webhook.test")
	if err := svc.Create(context.Background(), w); err != nil {
		t.Fatalf("create: %v", err)
	}

	orig, err := svc.Test(context.Background(), w.ID)
	if err != nil {
		t.Fatalf("test: %v", err)
	}

	replayed, err := svc.Replay(context.Background(), orig.ID)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if !replayed.Succeeded {
		t.Fatal("replayed delivery should have succeeded")
	}
	if replayed.ID == orig.ID {
		t.Fatal("replayed delivery should have a new ID")
	}
}
