// Package dedupe provides a durable contract.DeliveryDeduper on top of the
// kernel's idempotency.GormStore. It owns no table: every remembered id is a
// row of metacore_idempotency_keys under a per-consumer key prefix.
//
// FirstSeen is one INSERT ... ON CONFLICT DO UPDATE ... WHERE expired
// (idempotency.GormStore.Reserve), so among any number of concurrent callers,
// in one process or across replicas, exactly one wins an id. Remembered ids
// survive restarts and expire after the TTL.
//
//	store, _ := idempotency.NewGormStore(db, idempotency.GormStoreOptions{})
//	dd, _ := dedupe.New(store, dedupe.Options{Namespace: "support-callback"})
//	meta, err := contract.AcceptCallback(ctx, dd, secret, body, header, time.Now(), 0)
//
// Another webhook receiver uses the same store with its own Namespace; ids of
// different namespaces never collide.
package dedupe

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/asteby/metacore-kernel/idempotency"
	"github.com/asteby/metacore-kernel/support/contract"
)

// KeyPrefix starts every key written by this package, keeping its rows apart
// from the HTTP idempotency middleware's keys in the shared table.
const KeyPrefix = "dedupe:"

// maxForgetTokens bounds the in-process map used by Forget.
const maxForgetTokens = 4096

// Store is the part of *idempotency.GormStore the deduper needs.
type Store interface {
	Reserve(ctx context.Context, key string, lease time.Duration) (idempotency.Reservation, error)
	Release(ctx context.Context, key, token string) error
	Cleanup(ctx context.Context) (int64, error)
	CountLive(ctx context.Context, prefix string) (int64, error)
}

var _ Store = (*idempotency.GormStore)(nil)

// Options configures a Deduper.
type Options struct {
	// Namespace identifies the consumer (for example "support-callback").
	// Required; letters, digits, '.', '_', '-' and ':' only.
	Namespace string
	// TTL, when positive, overrides the ttl passed to FirstSeen, for hosts
	// that want one retention regardless of the caller. Zero uses the
	// caller's ttl (contract.AcceptCallback passes contract.DedupeTTL, 7 days).
	TTL time.Duration
	// MaxEntries caps the live ids of this namespace. Zero means unbounded
	// (the table is durable, unlike the in-memory deduper). When positive and
	// reached, FirstSeen fails closed with contract.ErrDedupeFull. The cap is
	// enforced after the claim, so concurrent first-seen calls near the limit
	// may be rejected slightly early, never admitted over the limit for long.
	MaxEntries int
}

// Deduper implements contract.DeliveryDeduper over a Store.
type Deduper struct {
	store  Store
	prefix string
	ttl    time.Duration
	max    int

	mu     sync.Mutex
	tokens map[string]string // id -> reservation token, for Forget
}

var _ contract.DeliveryDeduper = (*Deduper)(nil)

// New builds a Deduper for one consumer namespace.
func New(store Store, opts Options) (*Deduper, error) {
	if store == nil {
		return nil, errors.New("dedupe: New requires a store")
	}
	if err := validNamespace(opts.Namespace); err != nil {
		return nil, err
	}
	if opts.TTL < 0 || opts.MaxEntries < 0 {
		return nil, errors.New("dedupe: TTL and MaxEntries must not be negative")
	}
	return &Deduper{
		store:  store,
		prefix: KeyPrefix + opts.Namespace + ":",
		ttl:    opts.TTL,
		max:    opts.MaxEntries,
		tokens: make(map[string]string),
	}, nil
}

func validNamespace(ns string) error {
	if ns == "" {
		return errors.New("dedupe: Namespace is required")
	}
	for _, r := range ns {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '.', r == '_', r == '-', r == ':':
		default:
			return fmt.Errorf("dedupe: invalid Namespace %q", ns)
		}
	}
	return nil
}

// FirstSeen implements contract.DeliveryDeduper. A non-positive ttl records
// nothing and reports true, like contract.MemoryDeduper.
func (d *Deduper) FirstSeen(ctx context.Context, id string, ttl time.Duration) (bool, error) {
	if d.ttl > 0 {
		ttl = d.ttl
	}
	if ttl <= 0 {
		return true, nil
	}
	key := d.prefix + id
	r, err := d.store.Reserve(ctx, key, ttl)
	if err != nil {
		return false, err
	}
	if !r.Acquired {
		return false, nil
	}
	if d.max > 0 {
		n, err := d.store.CountLive(ctx, d.prefix)
		if err == nil && n > int64(d.max) {
			err = contract.ErrDedupeFull
		}
		if err != nil {
			_ = d.store.Release(ctx, key, r.Token)
			return false, err
		}
	}
	d.mu.Lock()
	if len(d.tokens) >= maxForgetTokens {
		for k := range d.tokens {
			delete(d.tokens, k)
			break
		}
	}
	d.tokens[id] = r.Token
	d.mu.Unlock()
	return true, nil
}

// Forget releases the record of id so a redelivery is processed again. A
// receiver calls it when it failed to process an event it already accepted.
// If the process restarted in between, the token is gone and the record just
// expires with its TTL.
func (d *Deduper) Forget(ctx context.Context, id string) {
	d.mu.Lock()
	tok, ok := d.tokens[id]
	delete(d.tokens, id)
	d.mu.Unlock()
	if ok {
		_ = d.store.Release(ctx, d.prefix+id, tok)
	}
}

// Confirm drops the in-process token of a delivery that was processed, so the
// record stays until it expires and the token map stays small.
func (d *Deduper) Confirm(id string) {
	d.mu.Lock()
	delete(d.tokens, id)
	d.mu.Unlock()
}

// Purge deletes every expired row of the shared table (all namespaces) and
// returns how many it removed. GormStore already purges on its own interval;
// call Purge from a cron when that goroutine is disabled.
func (d *Deduper) Purge(ctx context.Context) (int64, error) {
	return d.store.Cleanup(ctx)
}
