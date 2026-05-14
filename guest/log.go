package guest

import "fmt"

// LogLevel enumerates the structured log levels the guest may emit
// through `metacore_host.log`. The host import is fire-and-forget — it
// never returns an error envelope (see docs/wasm-abi.md § 3) — but the
// helper still prepends a level prefix so log aggregators can branch on
// severity without touching the import signature.
//
// Levels mirror the typical slog / zap quartet. Unknown values render
// as `info` via String() to keep the wire stable.
type LogLevel int

const (
	// LevelDebug is for verbose developer output. Hosts may drop it in
	// production deployments depending on log configuration.
	LevelDebug LogLevel = iota
	// LevelInfo is the default level for routine guest messages.
	LevelInfo
	// LevelWarn flags a recoverable anomaly worth surfacing to the
	// operator (e.g. retry consumed, fallback applied).
	LevelWarn
	// LevelError flags an unrecoverable condition in the guest.
	// Surfaces to the kernel log alongside any panic / abort.
	LevelError
)

// String returns the lowercase token used on the wire. Unknown values
// fall back to "info" so a future enum bump cannot corrupt the host's
// structured log fields.
func (l LogLevel) String() string {
	switch l {
	case LevelDebug:
		return "debug"
	case LevelInfo:
		return "info"
	case LevelWarn:
		return "warn"
	case LevelError:
		return "error"
	default:
		return "info"
	}
}

// formatLogLine renders the wire format the host receives. We prepend
// `[level] ` so the host's existing single-string `log` import surfaces
// the severity without an ABI change. Exposed for tests; the wasm-side
// caller is the only production consumer.
func formatLogLine(level LogLevel, msg string) string {
	return fmt.Sprintf("[%s] %s", level.String(), msg)
}
