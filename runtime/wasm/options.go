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
)

const (
	defaultPoolSize = 1
	maxPoolSize     = 64
)

type hostConfig struct {
	cacheDir   string
	poolSize   int
	poolSizeFn func(addonKey string) int
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
	return cfg
}

// sizeFor resolves the pool size for addonKey.
func (h *Host) sizeFor(addonKey string) int {
	if h.poolSizeFn != nil {
		if n := h.poolSizeFn(addonKey); n > 0 {
			return clampPool(n)
		}
	}
	return h.poolSize
}
