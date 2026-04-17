// Package log provides structured logging helpers built on top of [log/slog].
// It supports JSON (production) and text (development) handlers selected via
// the format option, and integrates with context propagation and Fiber middleware.
package log

import (
	"log/slog"
	"os"
)

// Format controls the output encoding of the logger.
type Format string

const (
	// FormatJSON emits newline-delimited JSON — recommended for production.
	FormatJSON Format = "json"
	// FormatText emits human-readable key=value pairs — recommended for development.
	FormatText Format = "text"
)

// Options configures the logger returned by New.
type Options struct {
	// Format selects JSON or text output. Defaults to FormatJSON.
	Format Format
	// Level is the minimum log level. Defaults to slog.LevelInfo.
	Level slog.Level
	// AddSource adds file/line information to every log record.
	AddSource bool
}

// New returns a *slog.Logger configured according to opts.
// Writes to os.Stdout. An empty Options{} is valid and produces a JSON logger
// at Info level without source attribution.
func New(opts Options) *slog.Logger {
	if opts.Format == "" {
		opts.Format = FormatJSON
	}

	ho := &slog.HandlerOptions{
		Level:     opts.Level,
		AddSource: opts.AddSource,
	}

	var handler slog.Handler
	if opts.Format == FormatText {
		handler = slog.NewTextHandler(os.Stdout, ho)
	} else {
		handler = slog.NewJSONHandler(os.Stdout, ho)
	}

	return slog.New(handler)
}

// Default returns a production-ready JSON logger at Info level.
func Default() *slog.Logger {
	return New(Options{})
}
