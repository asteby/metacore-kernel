package metadata

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/asteby/metacore-kernel/modelbase"
)

// DefaultCacheTTL is the fallback TTL when Config.CacheTTL is not set.
// Metadata is cheap-to-recompute but still cached briefly to absorb bursts
// during frontend warm-up.
const DefaultCacheTTL = 5 * time.Minute

// Config configures a Service. All fields are optional.
type Config struct {
	// CacheTTL is the time-to-live for cached TableMetadata / ModalMetadata
	// responses. A negative value disables caching entirely. The zero value
	// falls back to DefaultCacheTTL — pass a small negative (e.g. -1) to
	// explicitly opt out.
	CacheTTL time.Duration
}

// TableTransformer mutates a TableMetadata after it has been produced by the
// underlying ModelDefiner. Apps use transformers to layer on concerns the
// kernel intentionally does not know about: i18n localisation, org-settings
// overlays, addon-owned columns, fiscal-data nesting, etc.
//
// Transformers run in the order they are registered. Returning a non-nil
// error aborts the chain and propagates to the caller; partial mutations on
// meta are visible to the caller, so transformers should avoid destructive
// edits on failure paths.
type TableTransformer func(ctx context.Context, modelKey string, meta *modelbase.TableMetadata) error

// ModalTransformer is the ModalMetadata analog of TableTransformer.
type ModalTransformer func(ctx context.Context, modelKey string, meta *modelbase.ModalMetadata) error

// Service is the framework-agnostic metadata facade. Construct with New and
// (optionally) add transformers via WithTableTransformer / WithModalTransformer.
//
// Service methods accept context.Context for cancellation, even though the
// default in-memory lookups never block; transformers are the likely place
// where honouring ctx matters.
type Service struct {
	cfg   Config
	cache *cache

	mu                sync.RWMutex
	tableTransformers []TableTransformer
	modalTransformers []ModalTransformer

	// cacheVersion monotonically increments on every global invalidation so
	// that AllMetadata.Version gives the frontend a stable cache-buster.
	cacheVersion uint64
	startedAt    time.Time
}

// AllMetadata is the wire payload of GetAll. It is a snapshot of every
// registered model's TableMetadata and ModalMetadata, plus a Version token
// callers can use as an ETag / cache key.
type AllMetadata struct {
	Version string                             `json:"version"`
	Tables  map[string]modelbase.TableMetadata `json:"tables"`
	Modals  map[string]modelbase.ModalMetadata `json:"modals"`
}

// New constructs a Service with the given configuration. The Config is
// applied eagerly: after New returns, mutations to the passed-in Config have
// no effect.
func New(cfg Config) *Service {
	ttl := cfg.CacheTTL
	if ttl == 0 {
		ttl = DefaultCacheTTL
	}
	if ttl < 0 {
		ttl = 0 // disable
	}
	return &Service{
		cfg:       Config{CacheTTL: ttl},
		cache:     newCache(ttl),
		startedAt: time.Now(),
	}
}

// WithTableTransformer appends a TableTransformer to the chain. Returns the
// receiver so calls can be chained fluently.
func (s *Service) WithTableTransformer(fn TableTransformer) *Service {
	if fn == nil {
		return s
	}
	s.mu.Lock()
	s.tableTransformers = append(s.tableTransformers, fn)
	s.mu.Unlock()
	return s
}

// WithModalTransformer appends a ModalTransformer to the chain.
func (s *Service) WithModalTransformer(fn ModalTransformer) *Service {
	if fn == nil {
		return s
	}
	s.mu.Lock()
	s.modalTransformers = append(s.modalTransformers, fn)
	s.mu.Unlock()
	return s
}

// GetTable returns the TableMetadata for modelKey. Transformers are applied
// on every cache-miss path; the cached value is the post-transform result.
// Apps that need transformer output to vary per-request (e.g. per-org
// overlays) should set CacheTTL negative or call InvalidateCache when the
// overlay context changes.
func (s *Service) GetTable(ctx context.Context, modelKey string) (*modelbase.TableMetadata, error) {
	if modelKey == "" {
		return nil, ErrModelNotFound
	}
	key := tableCacheKey(modelKey)
	if cached, ok := s.cache.Get(key); ok {
		if meta, ok := cached.(*modelbase.TableMetadata); ok {
			return meta, nil
		}
	}

	def, ok := modelbase.Get(modelKey)
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrModelNotFound, modelKey)
	}
	table := def.DefineTable()

	s.mu.RLock()
	transformers := append([]TableTransformer(nil), s.tableTransformers...)
	s.mu.RUnlock()
	for _, fn := range transformers {
		if err := fn(ctx, modelKey, &table); err != nil {
			return nil, fmt.Errorf("metadata: table transformer for %s: %w", modelKey, err)
		}
	}

	meta := &table
	s.cache.Set(key, meta)
	return meta, nil
}

// GetModal returns the ModalMetadata for modelKey. See GetTable for caching
// semantics — they are identical.
func (s *Service) GetModal(ctx context.Context, modelKey string) (*modelbase.ModalMetadata, error) {
	if modelKey == "" {
		return nil, ErrModelNotFound
	}
	key := modalCacheKey(modelKey)
	if cached, ok := s.cache.Get(key); ok {
		if meta, ok := cached.(*modelbase.ModalMetadata); ok {
			return meta, nil
		}
	}

	def, ok := modelbase.Get(modelKey)
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrModelNotFound, modelKey)
	}
	modal := def.DefineModal()

	s.mu.RLock()
	transformers := append([]ModalTransformer(nil), s.modalTransformers...)
	s.mu.RUnlock()
	for _, fn := range transformers {
		if err := fn(ctx, modelKey, &modal); err != nil {
			return nil, fmt.Errorf("metadata: modal transformer for %s: %w", modelKey, err)
		}
	}

	meta := &modal
	s.cache.Set(key, meta)
	return meta, nil
}

// GetAll returns every registered model's TableMetadata and ModalMetadata in
// one payload. Frontends typically call this once at startup to warm their
// local cache; afterwards they call GetTable/GetModal for per-route refreshes.
//
// A model whose transformer chain returns an error is logged-through as a
// hard error for the whole call: GetAll is all-or-nothing by design, so that
// partial warming never leaves the frontend in a half-configured state.
func (s *Service) GetAll(ctx context.Context) (*AllMetadata, error) {
	if cached, ok := s.cache.Get(cacheKeyAll); ok {
		if all, ok := cached.(*AllMetadata); ok {
			return all, nil
		}
	}

	keys := modelbase.Keys()
	tables := make(map[string]modelbase.TableMetadata, len(keys))
	modals := make(map[string]modelbase.ModalMetadata, len(keys))

	for _, k := range keys {
		t, err := s.GetTable(ctx, k)
		if err != nil {
			return nil, err
		}
		m, err := s.GetModal(ctx, k)
		if err != nil {
			return nil, err
		}
		tables[k] = *t
		modals[k] = *m
	}

	all := &AllMetadata{
		Version: s.versionToken(),
		Tables:  tables,
		Modals:  modals,
	}
	s.cache.Set(cacheKeyAll, all)
	return all, nil
}

// InvalidateCache drops every cached entry and bumps the cache version. Call
// this when something upstream of transformers changed (e.g. an addon was
// installed, an org setting was toggled).
func (s *Service) InvalidateCache() {
	s.cache.InvalidateAll()
	s.mu.Lock()
	s.cacheVersion++
	s.mu.Unlock()
}

// InvalidateModel drops only the entries for modelKey, plus the GetAll
// aggregate (which transitively depends on modelKey).
func (s *Service) InvalidateModel(modelKey string) {
	if modelKey == "" {
		return
	}
	s.cache.Invalidate(tableCacheKey(modelKey))
	s.cache.Invalidate(modalCacheKey(modelKey))
	s.cache.Invalidate(cacheKeyAll)
}

// Config returns a copy of the effective configuration. Useful for tests
// and diagnostics; mutating the returned value has no effect on the Service.
func (s *Service) Config() Config {
	return s.cfg
}

// versionToken returns a string that changes whenever the cache is globally
// invalidated. It encodes both startup time and the invalidation counter so
// separate Service instances never accidentally share a token.
func (s *Service) versionToken() string {
	s.mu.RLock()
	v := s.cacheVersion
	s.mu.RUnlock()
	return strconv.FormatInt(s.startedAt.UnixNano(), 36) + "." + strconv.FormatUint(v, 36)
}

func tableCacheKey(modelKey string) string { return "table:" + modelKey }
func modalCacheKey(modelKey string) string { return "modal:" + modelKey }
