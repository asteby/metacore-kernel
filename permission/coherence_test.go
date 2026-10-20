package permission

import (
	"context"
	"testing"
	"time"

	"github.com/asteby/metacore-kernel/cachecoherence"
	"github.com/google/uuid"
)

// An addon install/upgrade changes role -> capability mappings: the next check
// after a coherence bump must read the store again, not the cached list.
func TestService_CoherenceBumpDropsCachedCapabilities(t *testing.T) {
	uid := uuid.New()
	user := &fakeUser{id: uid, role: "cashier"}
	store := NewInMemoryStore(map[Role][]Capability{"cashier": {"sales.view"}}, nil)
	svc := New(Config{Store: store, CacheTTL: time.Hour})

	if err := svc.Check(context.Background(), user, "sales.view"); err != nil {
		t.Fatal(err)
	}
	caps, _ := svc.GetUserCapabilities(context.Background(), user)
	if len(caps) != 1 {
		t.Fatalf("setup: %v", caps)
	}
	if _, ok := svc.cache.get(uid); !ok {
		t.Fatal("setup: entry must be cached")
	}
	cachecoherence.Bump("addon.upgrade", cachecoherence.WithAddon("sales"))
	if _, ok := svc.cache.get(uid); ok {
		t.Fatal("capability cache survived a coherence bump")
	}
}
