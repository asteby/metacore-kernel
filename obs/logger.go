// Package obs provides a minimal structured logger with correlation-ID
// propagation through context.Context. It wraps log/slog and is intentionally
// small: it acts as the seam through which apps thread correlation IDs
// (request_id, user_id, tenant_id, …) without touching every call site.
//
// Usage:
//
//	ctx = obs.With(ctx,
//	    slog.String("request_id", reqID),
//	    slog.String("user_id", userID),
//	)
//	obs.Info(ctx, "request.processing", slog.Int("payload_bytes", n))
//
//	// Optional timing helper
//	defer obs.Timer(ctx, "request.handle")()
//
// Environment:
//
//	LOG_LEVEL  = debug | info | warn | error   (default: info)
//	LOG_FORMAT = json | text                   (default: text)
//
// Relationship with kernel/log: the log package exposes a builder-style API
// (log.New(opts) -> *slog.Logger) and is the right choice when an app wants
// to construct its own logger and inject it explicitly. obs is the
// opinionated counterpart: a process-global default logger plus
// context-attached attribute accumulation, suited to apps that prefer
// package-level helpers (obs.Info, obs.Timer) over wiring a *slog.Logger
// through every layer.
package obs

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"sync/atomic"
	"time"
)

// levelVar is a dynamic slog.Level reference so SetLevel can change the
// threshold at runtime without rebuilding the handler.
var levelVar = new(slog.LevelVar)

// defaultLogger is the package-level logger used by shortcut functions.
// Swapped atomically so init() and SetFormat are race-free.
var defaultLogger atomic.Pointer[slog.Logger]

func init() {
	levelVar.Set(parseLevel(os.Getenv("LOG_LEVEL")))
	defaultLogger.Store(buildLogger(os.Getenv("LOG_FORMAT")))
}

// parseLevel maps LOG_LEVEL env to slog.Level. Unknown/empty → info.
func parseLevel(s string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// buildLogger constructs a logger honoring LOG_FORMAT. JSON is grepeable via
// jq; text is friendlier during local development.
func buildLogger(format string) *slog.Logger {
	opts := &slog.HandlerOptions{Level: levelVar}
	if strings.EqualFold(strings.TrimSpace(format), "json") {
		return slog.New(slog.NewJSONHandler(os.Stdout, opts))
	}
	return slog.New(slog.NewTextHandler(os.Stdout, opts))
}

// SetLevel lets callers override the log threshold programmatically. Mostly
// useful for tests; production should use the LOG_LEVEL env var.
func SetLevel(lvl slog.Level) { levelVar.Set(lvl) }

// SetFormat rebuilds the default logger using the supplied format ("json" or
// "text"). Useful for tests or apps that defer format selection past init().
func SetFormat(format string) { defaultLogger.Store(buildLogger(format)) }

// SetDefault swaps the package-level logger atomically. Pass nil to no-op.
// Useful to plug an app-built *slog.Logger (e.g. one from kernel/log) into
// the obs helpers.
func SetDefault(l *slog.Logger) {
	if l == nil {
		return
	}
	defaultLogger.Store(l)
}

// ctxKey is unexported so external packages cannot collide with our key.
type ctxKey struct{}

// fieldsKey is the single context key we use to carry accumulated attrs.
var fieldsKey = ctxKey{}

// With returns a new context that carries attrs in addition to any fields
// already present. Fields accumulate across nested calls so downstream code
// can keep adding correlation bits (turn_id, tool_name, …) without losing
// the upstream ones (request_id, user_id, …).
func With(ctx context.Context, attrs ...slog.Attr) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if len(attrs) == 0 {
		return ctx
	}
	existing, _ := ctx.Value(fieldsKey).([]slog.Attr)
	merged := make([]slog.Attr, 0, len(existing)+len(attrs))
	merged = append(merged, existing...)
	merged = append(merged, attrs...)
	return context.WithValue(ctx, fieldsKey, merged)
}

// FromContext returns a *slog.Logger pre-populated with all fields stored in
// ctx via With. If ctx is nil or carries no fields, the bare default logger
// is returned.
func FromContext(ctx context.Context) *slog.Logger {
	base := defaultLogger.Load()
	if ctx == nil {
		return base
	}
	fields, _ := ctx.Value(fieldsKey).([]slog.Attr)
	if len(fields) == 0 {
		return base
	}
	// slog.Logger.With accepts ...any; convert the Attrs.
	args := make([]any, 0, len(fields))
	for _, a := range fields {
		args = append(args, a)
	}
	return base.With(args...)
}

// Info logs at INFO level with the ctx-attached correlation fields.
func Info(ctx context.Context, msg string, attrs ...any) {
	FromContext(ctx).Info(msg, attrs...)
}

// Warn logs at WARN level.
func Warn(ctx context.Context, msg string, attrs ...any) {
	FromContext(ctx).Warn(msg, attrs...)
}

// Error logs at ERROR level.
func Error(ctx context.Context, msg string, attrs ...any) {
	FromContext(ctx).Error(msg, attrs...)
}

// Debug logs at DEBUG level. Gated by LOG_LEVEL=debug.
func Debug(ctx context.Context, msg string, attrs ...any) {
	FromContext(ctx).Debug(msg, attrs...)
}

// Timer returns a deferred callback that logs the elapsed duration in
// milliseconds at INFO level under the given event name. Intended usage:
//
//	defer obs.Timer(ctx, "agent.loop")()
//
// The returned function captures `name` and the start time so the caller
// doesn't have to track a time.Time.
func Timer(ctx context.Context, name string) func() {
	start := time.Now()
	return func() {
		Info(ctx, name+".done", slog.Int64("duration_ms", time.Since(start).Milliseconds()))
	}
}
