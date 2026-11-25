// Package contracttest holds reusable conformance suites for the ports of
// support/contract. An implementation of a port runs the suite from its own
// tests, so every implementation shares one definition of the semantics.
package contracttest

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/asteby/metacore-kernel/support/contract"
)

// Clock is a controllable time source shared between the suite and the
// implementation under test. The factory must wire Now into the deduper.
type Clock struct{ ns atomic.Int64 }

// NewClock starts at the current wall time.
func NewClock() *Clock {
	c := &Clock{}
	c.ns.Store(time.Now().UnixNano())
	return c
}

// Now returns the current fake time.
func (c *Clock) Now() time.Time { return time.Unix(0, c.ns.Load()) }

// Advance moves the fake time forward.
func (c *Clock) Advance(d time.Duration) { c.ns.Add(int64(d)) }

// DedupeFactory builds a fresh, empty deduper that reads time from clock and
// remembers at most maxEntries live ids (0 means the implementation default).
// Each call must return an isolated store: ids from a previous call are not
// visible.
type DedupeFactory func(t *testing.T, clock *Clock, maxEntries int) contract.DeliveryDeduper

// RunDedupe runs the DeliveryDeduper conformance suite against newDeduper.
func RunDedupe(t *testing.T, newDeduper DedupeFactory) {
	ctx := context.Background()
	const ttl = time.Hour

	t.Run("FirstThenDuplicate", func(t *testing.T) {
		d := newDeduper(t, NewClock(), 0)
		for i, want := range []bool{true, false, false} {
			got, err := d.FirstSeen(ctx, "d1", ttl)
			if err != nil || got != want {
				t.Fatalf("call %d = %v, %v; want %v", i, got, err, want)
			}
		}
	})

	t.Run("DistinctIDsAreIndependent", func(t *testing.T) {
		d := newDeduper(t, NewClock(), 0)
		for _, id := range []string{"a", "b", "c", "A"} {
			if got, err := d.FirstSeen(ctx, id, ttl); err != nil || !got {
				t.Fatalf("id %q = %v, %v; want first", id, got, err)
			}
		}
	})

	t.Run("ExpiresAfterTTL", func(t *testing.T) {
		clock := NewClock()
		d := newDeduper(t, clock, 0)
		if got, _ := d.FirstSeen(ctx, "x", ttl); !got {
			t.Fatal("first call must win")
		}
		clock.Advance(ttl - time.Second)
		if got, err := d.FirstSeen(ctx, "x", ttl); err != nil || got {
			t.Fatalf("before expiry = %v, %v; want duplicate", got, err)
		}
		clock.Advance(2 * time.Second)
		if got, err := d.FirstSeen(ctx, "x", ttl); err != nil || !got {
			t.Fatalf("after expiry = %v, %v; want first again", got, err)
		}
		if got, _ := d.FirstSeen(ctx, "x", ttl); got {
			t.Fatal("the re-recorded id must be a duplicate again")
		}
	})

	t.Run("NonPositiveTTLIsNeverRemembered", func(t *testing.T) {
		d := newDeduper(t, NewClock(), 0)
		for i := 0; i < 3; i++ {
			if got, err := d.FirstSeen(ctx, "z", 0); err != nil || !got {
				t.Fatalf("call %d = %v, %v; want first", i, got, err)
			}
		}
	})

	t.Run("ConcurrentSingleWinner", func(t *testing.T) {
		d := newDeduper(t, NewClock(), 0)
		const ids, workers = 20, 16
		wins := make([]atomic.Int32, ids)
		var wg sync.WaitGroup
		start := make(chan struct{})
		errs := make(chan error, ids*workers)
		for w := 0; w < workers; w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				for i := 0; i < ids; i++ {
					first, err := d.FirstSeen(ctx, fmt.Sprintf("c-%d", i), ttl)
					if err != nil {
						errs <- err
						return
					}
					if first {
						wins[i].Add(1)
					}
				}
			}()
		}
		close(start)
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Fatalf("FirstSeen: %v", err)
		}
		for i := range wins {
			if n := wins[i].Load(); n != 1 {
				t.Fatalf("id c-%d had %d winners; want exactly 1", i, n)
			}
		}
	})

	t.Run("FullFailsClosed", func(t *testing.T) {
		clock := NewClock()
		d := newDeduper(t, clock, 3)
		for i := 0; i < 3; i++ {
			if got, err := d.FirstSeen(ctx, fmt.Sprintf("f-%d", i), ttl); err != nil || !got {
				t.Fatalf("fill %d = %v, %v", i, got, err)
			}
		}
		if _, err := d.FirstSeen(ctx, "overflow", ttl); !errors.Is(err, contract.ErrDedupeFull) {
			t.Fatalf("full store error = %v; want ErrDedupeFull", err)
		}
		// A known id is still reported as a duplicate, not as an error.
		if got, err := d.FirstSeen(ctx, "f-0", ttl); err != nil || got {
			t.Fatalf("duplicate on a full store = %v, %v; want false, nil", got, err)
		}
		// The rejected id was not recorded: once room appears it is first.
		clock.Advance(ttl + time.Second)
		if got, err := d.FirstSeen(ctx, "overflow", ttl); err != nil || !got {
			t.Fatalf("after expiry = %v, %v; want first", got, err)
		}
	})
}
