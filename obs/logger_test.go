package obs

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

// withCapturedLogger swaps the package default for a JSON logger writing to
// buf, runs fn, and restores the original. Returns the captured bytes.
func withCapturedLogger(t *testing.T, level slog.Level, fn func()) []byte {
	t.Helper()
	var buf bytes.Buffer
	prev := defaultLogger.Load()
	prevLevel := levelVar.Level()
	t.Cleanup(func() {
		defaultLogger.Store(prev)
		levelVar.Set(prevLevel)
	})
	levelVar.Set(level)
	defaultLogger.Store(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: levelVar})))
	fn()
	return buf.Bytes()
}

func TestWith_AccumulatesAttrsAcrossCalls(t *testing.T) {
	ctx := context.Background()
	ctx = With(ctx, slog.String("request_id", "req-1"))
	ctx = With(ctx, slog.String("user_id", "user-7"))

	out := withCapturedLogger(t, slog.LevelInfo, func() {
		Info(ctx, "hello")
	})

	var rec map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(out), &rec); err != nil {
		t.Fatalf("invalid JSON output: %v — raw: %s", err, string(out))
	}
	if rec["request_id"] != "req-1" {
		t.Errorf("expected request_id=req-1, got %v", rec["request_id"])
	}
	if rec["user_id"] != "user-7" {
		t.Errorf("expected user_id=user-7, got %v", rec["user_id"])
	}
	if rec["msg"] != "hello" {
		t.Errorf("expected msg=hello, got %v", rec["msg"])
	}
}

func TestLevelFiltering_DebugSuppressedAtInfo(t *testing.T) {
	out := withCapturedLogger(t, slog.LevelInfo, func() {
		Debug(context.Background(), "should-not-appear")
		Info(context.Background(), "should-appear")
	})
	s := string(out)
	if strings.Contains(s, "should-not-appear") {
		t.Errorf("Debug leaked at INFO level: %s", s)
	}
	if !strings.Contains(s, "should-appear") {
		t.Errorf("Info was dropped: %s", s)
	}
}

func TestLevelFiltering_DebugAllowedAtDebug(t *testing.T) {
	out := withCapturedLogger(t, slog.LevelDebug, func() {
		Debug(context.Background(), "debug-visible")
	})
	if !strings.Contains(string(out), "debug-visible") {
		t.Errorf("Debug was dropped at DEBUG level: %s", string(out))
	}
}

func TestFromContext_NilCtxReturnsDefault(t *testing.T) {
	//nolint:staticcheck // deliberately testing nil ctx handling
	l := FromContext(nil)
	if l == nil {
		t.Fatal("FromContext(nil) returned nil logger")
	}
}

func TestFromContext_EmptyCtxReturnsDefault(t *testing.T) {
	l := FromContext(context.Background())
	if l == nil {
		t.Fatal("FromContext(ctx) returned nil logger")
	}
}

func TestTimer_EmitsDurationAttr(t *testing.T) {
	out := withCapturedLogger(t, slog.LevelInfo, func() {
		done := Timer(context.Background(), "job")
		done()
	})
	var rec map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(out), &rec); err != nil {
		t.Fatalf("invalid JSON: %v — raw: %s", err, string(out))
	}
	if rec["msg"] != "job.done" {
		t.Errorf("expected msg=job.done, got %v", rec["msg"])
	}
	if _, ok := rec["duration_ms"]; !ok {
		t.Errorf("expected duration_ms attr, got: %v", rec)
	}
}

func TestParseLevel(t *testing.T) {
	cases := map[string]slog.Level{
		"debug":   slog.LevelDebug,
		"DEBUG":   slog.LevelDebug,
		"info":    slog.LevelInfo,
		"":        slog.LevelInfo,
		"warn":    slog.LevelWarn,
		"warning": slog.LevelWarn,
		"error":   slog.LevelError,
		"garbage": slog.LevelInfo,
	}
	for in, want := range cases {
		if got := parseLevel(in); got != want {
			t.Errorf("parseLevel(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestBuildLogger_JSONFormat(t *testing.T) {
	var buf bytes.Buffer
	l := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: levelVar}))
	l.Info("probe", slog.String("k", "v"))
	if !strings.Contains(buf.String(), `"k":"v"`) {
		t.Errorf("expected JSON output to contain k:v, got: %s", buf.String())
	}
	// Sanity check on the real builder for "json"
	if _, ok := buildLogger("json").Handler().(*slog.JSONHandler); !ok {
		t.Errorf("buildLogger(json) did not return a JSON handler")
	}
	if _, ok := buildLogger("text").Handler().(*slog.TextHandler); !ok {
		t.Errorf("buildLogger(text) did not return a Text handler")
	}
}

func TestSetDefault_NilIsNoOp(t *testing.T) {
	prev := defaultLogger.Load()
	SetDefault(nil)
	if defaultLogger.Load() != prev {
		t.Errorf("SetDefault(nil) mutated the default logger")
	}
}
