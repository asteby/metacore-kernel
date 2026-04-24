package push

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

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
	// SQLite does not support gen_random_uuid(), so we create the table manually
	// and rely on the BeforeCreate hook to generate UUIDs.
	db.Exec(`CREATE TABLE IF NOT EXISTS push_subscriptions (
		id TEXT PRIMARY KEY,
		organization_id TEXT,
		created_by_id TEXT,
		created_at DATETIME,
		updated_at DATETIME,
		deleted_at DATETIME,
		user_id TEXT,
		endpoint TEXT UNIQUE,
		p256_dh TEXT,
		auth TEXT,
		device_type TEXT,
		user_agent TEXT,
		last_used_at DATETIME
	)`)
	return db
}

func setupService(t *testing.T, db *gorm.DB, client *http.Client) *Service {
	t.Helper()
	cfg := Config{DB: db}
	if client != nil {
		cfg.HTTPClient = client
	}
	return New(cfg)
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestSubscribe(t *testing.T) {
	db := setupTestDB(t)
	svc := setupService(t, db, nil)
	userID := uuid.New()

	sub, err := svc.Subscribe(context.Background(), userID, SubscriptionInput{
		Endpoint:   "https://push.example.com/sub1",
		P256DH:     "key1",
		Auth:       "auth1",
		DeviceType: "desktop",
	})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if sub.ID == uuid.Nil {
		t.Fatal("expected non-nil ID")
	}
	if sub.Endpoint != "https://push.example.com/sub1" {
		t.Fatalf("endpoint = %q", sub.Endpoint)
	}

	var count int64
	db.Model(&PushSubscription{}).Count(&count)
	if count != 1 {
		t.Fatalf("count = %d, want 1", count)
	}
}

func TestSubscribeUpsert(t *testing.T) {
	db := setupTestDB(t)
	svc := setupService(t, db, nil)
	userID := uuid.New()
	endpoint := "https://push.example.com/upsert"

	_, err := svc.Subscribe(context.Background(), userID, SubscriptionInput{Endpoint: endpoint, P256DH: "a", Auth: "a"})
	if err != nil {
		t.Fatalf("first subscribe: %v", err)
	}

	_, err = svc.Subscribe(context.Background(), userID, SubscriptionInput{Endpoint: endpoint, P256DH: "b", Auth: "b"})
	if err != nil {
		t.Fatalf("second subscribe: %v", err)
	}

	var count int64
	db.Model(&PushSubscription{}).Count(&count)
	if count != 1 {
		t.Fatalf("count = %d, want 1 (upsert should not create duplicate)", count)
	}
}

func TestUnsubscribe(t *testing.T) {
	db := setupTestDB(t)
	svc := setupService(t, db, nil)
	userID := uuid.New()
	endpoint := "https://push.example.com/unsub"

	_, err := svc.Subscribe(context.Background(), userID, SubscriptionInput{Endpoint: endpoint, P256DH: "k", Auth: "a"})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	if err := svc.Unsubscribe(context.Background(), endpoint); err != nil {
		t.Fatalf("unsubscribe: %v", err)
	}

	var count int64
	db.Model(&PushSubscription{}).Count(&count)
	if count != 0 {
		t.Fatalf("count = %d, want 0 after unsubscribe", count)
	}
}

func TestSend(t *testing.T) {
	var received atomic.Value
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		received.Store(body)
		w.WriteHeader(http.StatusCreated)
	}))
	defer ts.Close()

	db := setupTestDB(t)
	svc := setupService(t, db, ts.Client())
	userID := uuid.New()

	sub, err := svc.Subscribe(context.Background(), userID, SubscriptionInput{
		Endpoint: ts.URL + "/push",
		P256DH:   "k",
		Auth:     "a",
	})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	status, err := svc.Send(context.Background(), sub, Payload{Title: "Hello", Body: "World"})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if status != http.StatusCreated {
		t.Fatalf("status = %d, want 201", status)
	}

	raw, ok := received.Load().([]byte)
	if !ok || raw == nil {
		t.Fatal("server did not receive payload")
	}
	var p Payload
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if p.Title != "Hello" {
		t.Fatalf("title = %q, want Hello", p.Title)
	}
}

func TestAutoCleanup410(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusGone)
	}))
	defer ts.Close()

	db := setupTestDB(t)
	svc := setupService(t, db, ts.Client())
	userID := uuid.New()

	sub, err := svc.Subscribe(context.Background(), userID, SubscriptionInput{
		Endpoint: ts.URL + "/gone",
		P256DH:   "k",
		Auth:     "a",
	})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	status, err := svc.Send(context.Background(), sub, Payload{Title: "Bye"})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if status != http.StatusGone {
		t.Fatalf("status = %d, want 410", status)
	}

	var count int64
	db.Model(&PushSubscription{}).Count(&count)
	if count != 0 {
		t.Fatalf("count = %d, want 0 (410 should auto-delete subscription)", count)
	}
}

func TestOnExpiredEndpointHook(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusGone)
	}))
	defer ts.Close()

	db := setupTestDB(t)

	var hookCalled atomic.Bool
	var capturedEndpoint atomic.Value
	cfg := Config{
		DB:         db,
		HTTPClient: ts.Client(),
		OnExpiredEndpoint: func(ctx context.Context, sub *PushSubscription) error {
			hookCalled.Store(true)
			capturedEndpoint.Store(sub.Endpoint)
			return nil
		},
	}
	svc := New(cfg)
	userID := uuid.New()

	sub, err := svc.Subscribe(context.Background(), userID, SubscriptionInput{
		Endpoint: ts.URL + "/gone",
		P256DH:   "k",
		Auth:     "a",
	})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	status, err := svc.Send(context.Background(), sub, Payload{Title: "Bye"})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if status != http.StatusGone {
		t.Fatalf("status = %d, want 410", status)
	}

	if !hookCalled.Load() {
		t.Fatal("OnExpiredEndpoint hook was not called")
	}
	if got := capturedEndpoint.Load(); got != ts.URL+"/gone" {
		t.Fatalf("hook got endpoint = %v, want %q", got, ts.URL+"/gone")
	}

	// With hook set, the kernel must NOT perform its default Unsubscribe.
	var count int64
	db.Model(&PushSubscription{}).Count(&count)
	if count != 1 {
		t.Fatalf("count = %d, want 1 (hook owns the cleanup, kernel must not hard-delete)", count)
	}
}

func TestIsExpiredStatus(t *testing.T) {
	cases := []struct {
		status int
		want   bool
	}{
		{http.StatusOK, false},
		{http.StatusCreated, false},
		{http.StatusNotFound, true},
		{http.StatusGone, true},
		{http.StatusInternalServerError, false},
		{http.StatusTooManyRequests, false},
	}
	for _, c := range cases {
		if got := IsExpiredStatus(c.status); got != c.want {
			t.Errorf("IsExpiredStatus(%d) = %v, want %v", c.status, got, c.want)
		}
	}
}

func TestBroadcastToOrg(t *testing.T) {
	var hits atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusCreated)
	}))
	defer ts.Close()

	db := setupTestDB(t)
	svc := setupService(t, db, ts.Client())

	orgID := uuid.New()
	endpoints := []string{ts.URL + "/a", ts.URL + "/b", ts.URL + "/c"}
	orgSubs := make([]PushSubscription, 0, len(endpoints))
	for _, ep := range endpoints {
		sub, err := svc.Subscribe(context.Background(), uuid.New(), SubscriptionInput{Endpoint: ep, P256DH: "k", Auth: "a"})
		if err != nil {
			t.Fatalf("subscribe %s: %v", ep, err)
		}
		orgSubs = append(orgSubs, *sub)
	}

	var resolverCalls atomic.Int32
	resolver := func(ctx context.Context, got uuid.UUID) ([]PushSubscription, error) {
		resolverCalls.Add(1)
		if got != orgID {
			t.Errorf("resolver got tenantID = %s, want %s", got, orgID)
		}
		return orgSubs, nil
	}

	if err := svc.BroadcastToOrg(context.Background(), orgID, resolver, Payload{Title: "Org broadcast"}); err != nil {
		t.Fatalf("broadcast: %v", err)
	}
	if resolverCalls.Load() != 1 {
		t.Errorf("resolver called %d times, want 1", resolverCalls.Load())
	}
	if got := hits.Load(); got != int32(len(endpoints)) {
		t.Errorf("server hits = %d, want %d", got, len(endpoints))
	}
}

func TestBroadcastToOrgEmptyAndResolverError(t *testing.T) {
	db := setupTestDB(t)
	svc := setupService(t, db, nil)

	// Empty resolver result must not error.
	err := svc.BroadcastToOrg(context.Background(), uuid.New(),
		func(ctx context.Context, id uuid.UUID) ([]PushSubscription, error) { return nil, nil },
		Payload{Title: "nobody"})
	if err != nil {
		t.Fatalf("empty broadcast: %v", err)
	}

	// Resolver error propagates.
	sentinel := errors.New("db down")
	err = svc.BroadcastToOrg(context.Background(), uuid.New(),
		func(ctx context.Context, id uuid.UUID) ([]PushSubscription, error) { return nil, sentinel },
		Payload{})
	if !errors.Is(err, sentinel) {
		t.Fatalf("want sentinel error, got %v", err)
	}

	// Nil resolver must error.
	err = svc.BroadcastToOrg(context.Background(), uuid.New(), nil, Payload{})
	if err == nil {
		t.Fatal("nil resolver: expected error")
	}
}

func TestSubscribeEmptyEndpoint(t *testing.T) {
	db := setupTestDB(t)
	svc := setupService(t, db, nil)

	_, err := svc.Subscribe(context.Background(), uuid.New(), SubscriptionInput{Endpoint: ""})
	if err == nil {
		t.Fatal("expected error for empty endpoint")
	}
}
