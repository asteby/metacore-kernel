//go:build wasm || wasip1

package guest

// hostLog is the raw host import. The host signature is
// `log(msgPtr, msgLen)` — single string, no return value — so the
// helper composes its `[level] ` prefix on the guest side before
// handing the buffer over.
//
//go:wasmimport metacore_host log
func hostLog(msgPtr, msgLen uint32)

// Log writes a structured log line tagged with `level`. The helper
// prepends `[level] ` to `msg` so the host's existing single-string
// log import surfaces severity without an ABI change.
//
// The call is fire-and-forget: the host logs the line under the addon
// key and returns immediately. There is no error envelope to decode
// and Log never returns an error. Pass an empty `msg` to emit just the
// level marker (e.g. `Log(LevelDebug, "")` for "I am here" tracing).
//
// Memory contract: the formatted buffer is created on the guest's
// heap; the host reads it synchronously before returning so the
// underlying memory stays valid for the duration of the call.
func Log(level LogLevel, msg string) {
	line := formatLogLine(level, msg)
	ptr, n := stringRef(line)
	if n == 0 {
		// Defensive: stringRef of an empty string returns (0,0).
		// formatLogLine always produces at least "[info] " so this
		// branch is unreachable in production, but we keep it to
		// avoid handing a zero pointer to the host on a future
		// refactor.
		return
	}
	hostLog(ptr, n)
}
