package wasm

import (
	"log"
	"os"
	"strconv"
	"strings"

	"github.com/asteby/metacore-kernel/metrics"
)

// Environment variables read by NewHost. Explicit Options win over them.
const (
	// EnvCompileCacheDir points the wazero compilation cache at a directory.
	// Replicas / processes that mount the same volume share compiled native
	// code, so a restart or a scale-out does not recompile every addon.
	EnvCompileCacheDir = "WASM_COMPILE_CACHE_DIR"
	// EnvPoolSize is the default number of instances per (addon, installation).
	EnvPoolSize = "WASM_POOL_SIZE"
	// EnvPoolMemoryMB is the memory budget, in MiB, one (addon, installation)
	// pool may commit in the worst case: the pool size is capped at
	// budget / the addon's memory ceiling (memory_limit_mb, else the global
	// ceiling), never below 1.
	EnvPoolMemoryMB = "WASM_POOL_MEMORY_MB"
)

const (
	defaultPoolSize = 1
	maxPoolSize     = 64
	// defaultPoolMemoryMB lets an addon without a declared memory_limit_mb (256
	// MiB ceiling) run 4 instances, and one declared at 64 MiB run 16.
	defaultPoolMemoryMB = 1024
)

type hostConfig struct {
	cacheDir   string
	poolSize   int
	poolSizeFn func(addonKey string) int
	poolMemMB  int
	metrics    *metrics.Registry
}

// Option customises NewHost.
type Option func(*hostConfig)

// WithCompileCacheDir persists wazero's compiled native code in dir (created
// with mode 0700 if missing). It overrides WASM_COMPILE_CACHE_DIR. The
// directory MUST be writable only by the service account: its content is
// executed as native code, so whoever can write there can run code in the
// process. An empty dir keeps the in-memory cache.
func WithCompileCacheDir(dir string) Option {
	return func(c *hostConfig) { c.cacheDir = dir }
}

// WithPoolSize sets the default instance-pool size per (addon, installation).
// n < 1 means 1 (the historical single serialised instance); n is clamped to
// 64. Overrides WASM_POOL_SIZE. See docs/wasm-abi.md § 22 for when >1 is safe.
func WithPoolSize(n int) Option {
	return func(c *hostConfig) { c.poolSize = n }
}

// WithAddonPoolSize overrides the pool size for specific addons. The function
// returns <= 0 to fall back to the default size.
func WithAddonPoolSize(fn func(addonKey string) int) Option {
	return func(c *hostConfig) { c.poolSizeFn = fn }
}

// WithPoolMemoryBudget caps, in MiB, the worst-case memory one (addon,
// installation) pool may commit: its size never exceeds budget divided by the
// addon's memory ceiling (memory_limit_mb, else the global 256 MiB), and never
// drops below 1. n <= 0 keeps the default (1024). Overrides WASM_POOL_MEMORY_MB.
// It bounds a pool, not the host: instances are created only under concurrent
// demand, per installation.
func WithPoolMemoryBudget(n int) Option {
	return func(c *hostConfig) { c.poolMemMB = n }
}

// WithMetrics registers the wasm runtime collectors on reg.Prometheus.
func WithMetrics(reg *metrics.Registry) Option {
	return func(c *hostConfig) { c.metrics = reg }
}

func clampPool(n int) int {
	if n < 1 {
		return 1
	}
	if n > maxPoolSize {
		return maxPoolSize
	}
	return n
}

func resolveConfig(logger *log.Logger, opts []Option) hostConfig {
	cfg := hostConfig{poolSize: 0}
	for _, o := range opts {
		if o != nil {
			o(&cfg)
		}
	}
	if cfg.cacheDir == "" {
		cfg.cacheDir = strings.TrimSpace(os.Getenv(EnvCompileCacheDir))
	}
	if cfg.poolSize == 0 {
		if v := strings.TrimSpace(os.Getenv(EnvPoolSize)); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil {
				logger.Printf("wasm: invalid %s=%q, using %d", EnvPoolSize, v, defaultPoolSize)
			} else {
				cfg.poolSize = n
			}
		}
	}
	cfg.poolSize = clampPool(cfg.poolSize)
	if cfg.poolMemMB <= 0 {
		cfg.poolMemMB = defaultPoolMemoryMB
		if v := strings.TrimSpace(os.Getenv(EnvPoolMemoryMB)); v != "" {
			if n, err := strconv.Atoi(v); err != nil || n <= 0 {
				logger.Printf("wasm: invalid %s=%q, using %d", EnvPoolMemoryMB, v, defaultPoolMemoryMB)
			} else {
				cfg.poolMemMB = n
			}
		}
	}
	return cfg
}

// sizeFor resolves the operator-level pool size for addonKey: the per-addon
// override, else the host default.
func (h *Host) sizeFor(addonKey string) int {
	if h.poolSizeFn != nil {
		if n := h.poolSizeFn(addonKey); n > 0 {
			return clampPool(n)
		}
	}
	return h.poolSize
}

// poolSizeFor resolves the instance-pool size of one (addon, installation)
// pool. Precedence: operator override (WithAddonPoolSize) > the addon's
// manifest (backend.concurrency / runtime.wasm.concurrency) > host default
// (WithPoolSize / WASM_POOL_SIZE, 1). The result is then capped so the pool's
// worst-case memory (size x memory ceiling) stays within the pool memory
// budget. The pool belongs to one installation, so tenants never share
// instances whatever the size.
func (h *Host) poolSizeFor(addonKey string, entry *compiledEntry) int {
	n := h.poolSize
	if spec := entry.spec.Load(); spec != nil && spec.Concurrency > 0 {
		n = clampPool(spec.Concurrency)
	}
	if h.poolSizeFn != nil {
		if o := h.poolSizeFn(addonKey); o > 0 {
			n = clampPool(o)
		}
	}
	if n > 1 && h.poolMemMB > 0 {
		perInstanceMB := max(1, int(uint64(entry.pages)*pageBytes/(1024*1024)))
		n = min(n, max(1, h.poolMemMB/perInstanceMB))
	}
	return n
}
