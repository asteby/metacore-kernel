package metadata

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestCache_SetGet(t *testing.T) {
	c := newCache(time.Minute)

	c.Set("k", "v")
	got, ok := c.Get("k")
	if !ok {
		t.Fatalf("expected hit, got miss")
	}
	if got != "v" {
		t.Fatalf("expected %q, got %v", "v", got)
	}
}

func TestCache_Miss(t *testing.T) {
	c := newCache(time.Minute)
	if _, ok := c.Get("absent"); ok {
		t.Fatalf("expected miss on absent key")
	}
}

func TestCache_ZeroTTLDisables(t *testing.T) {
	c := newCache(0)
	c.Set("k", "v")
	if _, ok := c.Get("k"); ok {
		t.Fatalf("zero TTL must disable cache")
	}
	if n := c.len(); n != 0 {
		t.Fatalf("zero TTL must not store anything; len=%d", n)
	}
}

func TestCache_Expiration(t *testing.T) {
	c := newCache(time.Minute)

	base := time.Unix(1_700_000_000, 0)
	now := base
	c.now = func() time.Time { return now }

	c.Set("k", "v")
	if _, ok := c.Get("k"); !ok {
		t.Fatalf("expected hit immediately after set")
	}

	now = base.Add(2 * time.Minute)
	if _, ok := c.Get("k"); ok {
		t.Fatalf("expected miss after TTL expired")
	}
	if n := c.len(); n != 0 {
		t.Fatalf("expired entry should be dropped; len=%d", n)
	}
}

func TestCache_InvalidateKey(t *testing.T) {
	c := newCache(time.Minute)
	c.Set("a", 1)
	c.Set("b", 2)

	c.Invalidate("a")

	if _, ok := c.Get("a"); ok {
		t.Fatalf("expected miss for invalidated key")
	}
	if _, ok := c.Get("b"); !ok {
		t.Fatalf("sibling key b must still be cached")
	}
}

func TestCache_InvalidateAll(t *testing.T) {
	c := newCache(time.Minute)
	c.Set("a", 1)
	c.Set("b", 2)

	c.InvalidateAll()

	if n := c.len(); n != 0 {
		t.Fatalf("InvalidateAll must clear every entry; len=%d", n)
	}
}

// TestCache_Concurrency exercises the RWMutex under -race. Ten writers and
// ten readers hammer the cache; failure manifests as a race detector trip
// rather than a value check.
func TestCache_Concurrency(t *testing.T) {
	c := newCache(time.Minute)
	const workers = 10
	const iters = 500

	var wg sync.WaitGroup
	var hits int64

	for w := 0; w < workers; w++ {
		wg.Add(2)
		go func(id int) {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				c.Set("k", id*1000+i)
			}
		}(w)
		go func() {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				if _, ok := c.Get("k"); ok {
					atomic.AddInt64(&hits, 1)
				}
			}
		}()
	}

	wg.Wait()

	// Just sanity-check the cache is still usable afterwards.
	c.Set("final", "ok")
	if v, ok := c.Get("final"); !ok || v != "ok" {
		t.Fatalf("cache unusable after concurrent load")
	}
}
