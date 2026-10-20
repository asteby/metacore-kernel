package metadata

import (
	"context"
	"testing"
	"time"

	"github.com/asteby/metacore-kernel/cachecoherence"
)

func TestService_CoherenceBumpInvalidatesMetadataAndMovesVersion(t *testing.T) {
	key := registerFakeModel(t, "CoherenceModel")
	svc := New(Config{CacheTTL: time.Hour})
	if _, err := svc.GetTable(context.Background(), key); err != nil {
		t.Fatal(err)
	}
	if svc.cache.len() == 0 {
		t.Fatal("setup: nothing cached")
	}
	v0 := svc.versionToken()
	cachecoherence.Bump("addon.install", cachecoherence.WithAddon("crm"))
	if svc.cache.len() != 0 {
		t.Fatal("metadata cache survived a coherence bump")
	}
	if svc.versionToken() == v0 {
		t.Fatal("metadata version token did not move: clients would not revalidate")
	}
}
