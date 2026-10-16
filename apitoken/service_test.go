package apitoken

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func sqliteDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := fmt.Sprintf("file:apitoken_%s?mode=memory&cache=shared&_busy_timeout=5000", strings.ReplaceAll(uuid.NewString(), "-", ""))
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := db.DB()
	sqlDB.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = sqlDB.Close() })
	if err := Migrate(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	return db
}

// eachStore runs fn against the in-memory store and the GORM store (SQLite).
func eachStore(t *testing.T, fn func(t *testing.T, store Store)) {
	t.Run("Memory", func(t *testing.T) { fn(t, NewMemoryStore()) })
	t.Run("Gorm", func(t *testing.T) { fn(t, NewGormStore(sqliteDB(t))) })
}

var org = uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")

func newSvc(store Store, clk *clock, opts ...Option) *Service {
	opts = append([]Option{WithClock(clk.Now), WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil)))}, opts...)
	return NewService(store, opts...)
}

func newClock() *clock { return &clock{t: time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)} }

func TestIssueAndAuthenticate(t *testing.T) {
	eachStore(t, func(t *testing.T, store Store) {
		clk := newClock()
		svc := newSvc(store, clk)
		ctx := context.Background()
		user := uuid.New()
		plain, tok, err := svc.Issue(ctx, org, "SSR", KindSecret, []string{"orders:read", "catalog:read", "catalog:read"}, nil,
			WithRateLimit(120), WithCreatedBy(user))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(plain, "sk_") || len(plain) != 3+43 {
			t.Fatalf("plaintext shape: %q", plain)
		}
		if tok.Prefix != plain[:12] || tok.TokenHash != HashToken(plain) || tok.Org() != org || tok.Status != StatusActive {
			t.Fatalf("record wrong: %+v", tok)
		}
		if strings.Join(tok.Scopes, ",") != "catalog:read,orders:read" || tok.RateLimitPerMinute != 120 || *tok.CreatedBy != user {
			t.Fatalf("normalized fields wrong: %+v", tok)
		}

		got, err := svc.Authenticate(ctx, plain)
		if err != nil {
			t.Fatal(err)
		}
		if got.ID != tok.ID || got.LastUsedAt == nil || !got.LastUsedAt.Equal(clk.Now()) {
			t.Fatalf("authenticate: %+v", got)
		}
		stored, _ := store.Get(ctx, tok.ID)
		if stored.LastUsedAt == nil {
			t.Fatal("last_used_at not persisted")
		}

		// Throttled: a second use within the minute does not write.
		clk.Advance(30 * time.Second)
		if _, err := svc.Authenticate(ctx, plain); err != nil {
			t.Fatal(err)
		}
		stored, _ = store.Get(ctx, tok.ID)
		if !stored.LastUsedAt.Equal(clk.Now().Add(-30 * time.Second)) {
			t.Fatalf("last_used_at written within throttle window: %v", stored.LastUsedAt)
		}
		clk.Advance(31 * time.Second)
		if _, err := svc.Authenticate(ctx, plain); err != nil {
			t.Fatal(err)
		}
		stored, _ = store.Get(ctx, tok.ID)
		if !stored.LastUsedAt.Equal(clk.Now()) {
			t.Fatalf("last_used_at not refreshed after a minute: %v", stored.LastUsedAt)
		}

		for _, bad := range []string{"", "sk_short", "xx_" + plain[3:], plain + "x", "pk_" + plain[3:], "sk_" + strings.Repeat("A", 43)} {
			if _, err := svc.Authenticate(ctx, bad); !errors.Is(err, ErrInvalidToken) {
				t.Fatalf("%q: got %v", bad, err)
			}
		}

		list, err := svc.List(ctx, org)
		if err != nil || len(list) != 1 || list[0].ID != tok.ID {
			t.Fatalf("list: %+v %v", list, err)
		}
		if list, _ := svc.List(ctx, uuid.New()); len(list) != 0 {
			t.Fatalf("other org sees tokens: %+v", list)
		}
	})
}

func TestPlaintextNeverStored(t *testing.T) {
	db := sqliteDB(t)
	svc := newSvc(NewGormStore(db), newClock())
	plain, _, err := svc.Issue(context.Background(), org, "SSR", KindSecret, []string{"catalog:read"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var rows []map[string]any
	if err := db.Table(TableName).Find(&rows).Error; err != nil {
		t.Fatal(err)
	}
	dump := fmt.Sprint(rows)
	if strings.Contains(dump, plain) || strings.Contains(dump, plain[3:]) {
		t.Fatal("plaintext found in the table")
	}
	if !strings.Contains(dump, HashToken(plain)) {
		t.Fatal("hash missing from the table")
	}
}

func TestRevokedAndExpired(t *testing.T) {
	eachStore(t, func(t *testing.T, store Store) {
		clk := newClock()
		svc := newSvc(store, clk)
		ctx := context.Background()

		plain, tok, _ := svc.Issue(ctx, org, "a", KindSecret, []string{"catalog:read"}, nil)
		if err := svc.Revoke(ctx, tok.ID); err != nil {
			t.Fatal(err)
		}
		if err := svc.Revoke(ctx, tok.ID); err != nil {
			t.Fatalf("revoke twice: %v", err)
		}
		if _, err := svc.Authenticate(ctx, plain); !errors.Is(err, ErrRevoked) {
			t.Fatalf("revoked: %v", err)
		}
		if err := svc.Revoke(ctx, uuid.New()); !errors.Is(err, ErrNotFound) {
			t.Fatalf("unknown id: %v", err)
		}

		exp := clk.Now().Add(time.Hour)
		plain, _, _ = svc.Issue(ctx, org, "b", KindSecret, []string{"catalog:read"}, &exp)
		if _, err := svc.Authenticate(ctx, plain); err != nil {
			t.Fatal(err)
		}
		clk.Advance(time.Hour)
		if _, err := svc.Authenticate(ctx, plain); !errors.Is(err, ErrExpired) {
			t.Fatalf("expired: %v", err)
		}
	})
}

func TestRotationGrace(t *testing.T) {
	eachStore(t, func(t *testing.T, store Store) {
		clk := newClock()
		svc := newSvc(store, clk)
		ctx := context.Background()
		oldPlain, old, _ := svc.Issue(ctx, org, "ssr", KindSecret, []string{"catalog:read", "cart:write"}, nil, WithRateLimit(30))

		newPlain, nt, err := svc.Rotate(ctx, old.ID, 10*time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if newPlain == oldPlain || nt.RotatedFromID == nil || *nt.RotatedFromID != old.ID || nt.Org() != org ||
			strings.Join(nt.Scopes, ",") != "cart:write,catalog:read" || nt.RateLimitPerMinute != 30 || nt.Name != "ssr" {
			t.Fatalf("rotated token wrong: %+v", nt)
		}
		// Both work during the grace window.
		for _, p := range []string{oldPlain, newPlain} {
			if _, err := svc.Authenticate(ctx, p); err != nil {
				t.Fatalf("during grace: %v", err)
			}
		}
		clk.Advance(10 * time.Minute)
		if _, err := svc.Authenticate(ctx, oldPlain); !errors.Is(err, ErrExpired) {
			t.Fatalf("old after grace: %v", err)
		}
		if _, err := svc.Authenticate(ctx, newPlain); err != nil {
			t.Fatalf("new after grace: %v", err)
		}

		// Zero grace revokes the old token at once.
		p2, _, err := svc.Rotate(ctx, nt.ID, 0)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := svc.Authenticate(ctx, newPlain); !errors.Is(err, ErrRevoked) {
			t.Fatalf("zero grace: %v", err)
		}
		if _, err := svc.Authenticate(ctx, p2); err != nil {
			t.Fatal(err)
		}

		// Grace never extends an earlier expiry.
		exp := clk.Now().Add(time.Minute)
		_, short, _ := svc.Issue(ctx, org, "short", KindSecret, []string{"catalog:read"}, &exp)
		if _, _, err := svc.Rotate(ctx, short.ID, time.Hour); err != nil {
			t.Fatal(err)
		}
		if got, _ := store.Get(ctx, short.ID); !got.ExpiresAt.Equal(exp) {
			t.Fatalf("grace extended expiry: %v", got.ExpiresAt)
		}
		// A revoked token cannot be rotated.
		if _, _, err := svc.Rotate(ctx, nt.ID, time.Hour); !errors.Is(err, ErrRevoked) {
			t.Fatalf("rotate revoked: %v", err)
		}
	})
}

func TestPublishableScopes(t *testing.T) {
	ctx := context.Background()
	svc := newSvc(NewMemoryStore(), newClock())
	plain, tok, err := svc.Issue(ctx, org, "browser", KindPublishable, []string{"catalog:read", "content:read", "cart:write"}, nil)
	if err != nil || !strings.HasPrefix(plain, "pk_") || tok.Kind != KindPublishable {
		t.Fatalf("publishable issue: %v %q", err, plain)
	}
	for _, bad := range []string{"checkout:write", "customer:write", "*", "catalog:*", "orders:write"} {
		if _, _, err := svc.Issue(ctx, org, "browser", KindPublishable, []string{"catalog:read", bad}, nil); !errors.Is(err, ErrInvalidScope) {
			t.Fatalf("publishable %q: %v", bad, err)
		}
	}
	// Secret tokens may hold anything well-formed.
	if _, _, err := svc.Issue(ctx, org, "ssr", KindSecret, []string{"checkout:write", "catalog:*", "*"}, nil); err != nil {
		t.Fatal(err)
	}
	// Custom allowlist.
	strict := newSvc(NewMemoryStore(), newClock(), WithPublishableScopes("catalog:read"))
	if _, _, err := strict.Issue(ctx, org, "b", KindPublishable, []string{"content:read"}, nil); !errors.Is(err, ErrInvalidScope) {
		t.Fatalf("custom allowlist: %v", err)
	}
	if _, _, err := strict.Issue(ctx, org, "b", KindPublishable, []string{"catalog:read"}, nil); err != nil {
		t.Fatal(err)
	}
}

func TestIssueValidation(t *testing.T) {
	ctx := context.Background()
	clk := newClock()
	svc := newSvc(NewMemoryStore(), clk)
	past := clk.Now().Add(-time.Second)
	cases := []struct {
		name   string
		org    uuid.UUID
		label  string
		kind   Kind
		scopes []string
		exp    *time.Time
		opts   []IssueOption
		want   error
	}{
		{"unknown kind", org, "x", "magic", []string{"a:read"}, nil, nil, ErrInvalidKind},
		{"secret without org", uuid.Nil, "x", KindSecret, []string{"a:read"}, nil, nil, ErrInvalidKind},
		{"platform with org", org, "x", KindPlatform, []string{"a:read"}, nil, nil, ErrInvalidKind},
		{"empty name", org, " ", KindSecret, []string{"a:read"}, nil, nil, ErrInvalidInput},
		{"long name", org, strings.Repeat("n", 121), KindSecret, []string{"a:read"}, nil, nil, ErrInvalidInput},
		{"no scopes", org, "x", KindSecret, nil, nil, nil, ErrInvalidScope},
		{"bad scope", org, "x", KindSecret, []string{"Catalog:Read"}, nil, nil, ErrInvalidScope},
		{"past expiry", org, "x", KindSecret, []string{"a:read"}, &past, nil, ErrInvalidInput},
		{"negative rate", org, "x", KindSecret, []string{"a:read"}, nil, []IssueOption{WithRateLimit(-1)}, ErrInvalidInput},
	}
	for _, tc := range cases {
		if _, _, err := svc.Issue(ctx, tc.org, tc.label, tc.kind, tc.scopes, tc.exp, tc.opts...); !errors.Is(err, tc.want) {
			t.Fatalf("%s: got %v, want %v", tc.name, err, tc.want)
		}
	}
}

func TestPlatformTokens(t *testing.T) {
	eachStore(t, func(t *testing.T, store Store) {
		ctx := context.Background()
		svc := newSvc(store, newClock())
		plain, tok, err := svc.Issue(ctx, uuid.Nil, "renderer", KindPlatform, []string{"catalog:read", "cart:write", "checkout:write"}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(plain, "pt_") || tok.OrganizationID != nil || tok.Org() != uuid.Nil {
			t.Fatalf("platform token: %q %+v", plain, tok)
		}
		got, err := svc.Authenticate(ctx, plain)
		if err != nil || got.OrganizationID != nil {
			t.Fatalf("authenticate platform: %+v %v", got, err)
		}
		if p := PrincipalOf(got); p.OrgID != uuid.Nil || p.TokenID != tok.ID {
			t.Fatalf("principal: %+v", p)
		}
		list, _ := svc.List(ctx, uuid.Nil)
		if len(list) != 1 || list[0].ID != tok.ID {
			t.Fatalf("platform list: %+v", list)
		}
		if list, _ := svc.List(ctx, org); len(list) != 0 {
			t.Fatalf("platform token listed under an org: %+v", list)
		}
		// Rotation keeps it platform-scoped.
		_, nt, err := svc.Rotate(ctx, tok.ID, time.Minute)
		if err != nil || nt.OrganizationID != nil || nt.Kind != KindPlatform {
			t.Fatalf("rotate platform: %+v %v", nt, err)
		}
	})
}

func TestHashUniqueness(t *testing.T) {
	eachStore(t, func(t *testing.T, store Store) {
		ctx := context.Background()
		now := time.Now().UTC()
		a := &APIToken{ID: uuid.New(), OrganizationID: &org, Name: "a", Kind: KindSecret, Prefix: "sk_x", TokenHash: HashToken("same"),
			Scopes: []string{"a:read"}, Status: StatusActive, CreatedAt: now, UpdatedAt: now}
		if err := store.Create(ctx, a); err != nil {
			t.Fatal(err)
		}
		b := *a
		b.ID = uuid.New()
		if err := store.Create(ctx, &b); !errors.Is(err, ErrDuplicateHash) {
			t.Fatalf("duplicate hash: %v", err)
		}
	})

	// A colliding entropy source is retried with fresh bytes.
	fixed := bytes.Repeat([]byte{7}, 32)
	src := io.MultiReader(bytes.NewReader(fixed), bytes.NewReader(fixed), bytes.NewReader(bytes.Repeat([]byte{9}, 32)))
	svc := newSvc(NewMemoryStore(), newClock(), WithRand(src))
	p1, _, err := svc.Issue(context.Background(), org, "a", KindSecret, []string{"a:read"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	p2, _, err := svc.Issue(context.Background(), org, "b", KindSecret, []string{"a:read"}, nil)
	if err != nil || p1 == p2 {
		t.Fatalf("collision not retried: %v", err)
	}
}

func TestKindOf(t *testing.T) {
	plain, _ := generate(KindPublishable, bytes.NewReader(make([]byte, 32)))
	if k, ok := KindOf(plain); !ok || k != KindPublishable {
		t.Fatalf("KindOf(%q) = %v %v", plain, k, ok)
	}
	if _, ok := KindOf("pk_" + strings.Repeat("!", 43)); ok {
		t.Fatal("non-base64url accepted")
	}
}
