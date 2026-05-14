package guest

import "testing"

func TestLogLevel_String(t *testing.T) {
	cases := []struct {
		in   LogLevel
		want string
	}{
		{LevelDebug, "debug"},
		{LevelInfo, "info"},
		{LevelWarn, "warn"},
		{LevelError, "error"},
		// Out-of-range values fall back to "info" to keep wire stable
		// when a future enum bump leaks across an SDK/host skew.
		{LogLevel(99), "info"},
		{LogLevel(-1), "info"},
	}
	for _, tc := range cases {
		if got := tc.in.String(); got != tc.want {
			t.Errorf("LogLevel(%d).String() = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestFormatLogLine(t *testing.T) {
	got := formatLogLine(LevelWarn, "retry exhausted")
	want := "[warn] retry exhausted"
	if got != want {
		t.Errorf("formatLogLine = %q, want %q", got, want)
	}
}

func TestFormatLogLine_EmptyMessageKeepsPrefix(t *testing.T) {
	// Empty message is a valid trace marker — must keep the level
	// prefix so the host sees a non-empty buffer (Log() drops zero-
	// length sends defensively, but the formatter itself never emits
	// an empty string).
	got := formatLogLine(LevelDebug, "")
	want := "[debug] "
	if got != want {
		t.Errorf("formatLogLine(empty) = %q, want %q", got, want)
	}
}

func TestFormatLogLine_UnknownLevelFallsBackToInfo(t *testing.T) {
	got := formatLogLine(LogLevel(42), "hello")
	want := "[info] hello"
	if got != want {
		t.Errorf("formatLogLine(unknown) = %q, want %q", got, want)
	}
}
