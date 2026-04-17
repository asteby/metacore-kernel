package log_test

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/google/uuid"

	kernellog "github.com/asteby/metacore-kernel/log"
)

func TestNew_JSONDefault(t *testing.T) {
	l := kernellog.New(kernellog.Options{})
	if l == nil {
		t.Fatal("expected non-nil logger")
	}
}

func TestNew_Text(t *testing.T) {
	l := kernellog.New(kernellog.Options{Format: kernellog.FormatText})
	if l == nil {
		t.Fatal("expected non-nil logger")
	}
}

func TestDefault(t *testing.T) {
	l := kernellog.Default()
	if l == nil {
		t.Fatal("expected non-nil logger")
	}
}

func TestWithLogger_FromContext(t *testing.T) {
	buf := &bytes.Buffer{}
	handler := slog.NewJSONHandler(buf, nil)
	logger := slog.New(handler)

	ctx := kernellog.WithLogger(context.Background(), logger)
	got := kernellog.FromContext(ctx)
	if got == nil {
		t.Fatal("expected logger from context")
	}
	got.Info("test-message")
	if !strings.Contains(buf.String(), "test-message") {
		t.Errorf("expected log output to contain test-message, got: %s", buf.String())
	}
}

func TestFromContext_Fallback(t *testing.T) {
	// Empty context should fall back to slog.Default() without panic.
	l := kernellog.FromContext(context.Background())
	if l == nil {
		t.Fatal("expected fallback logger")
	}
}

func TestWithRequestID(t *testing.T) {
	buf := &bytes.Buffer{}
	base := slog.New(slog.NewJSONHandler(buf, nil))
	l := kernellog.WithRequestID(base, "req-123")
	l.Info("ping")
	if !strings.Contains(buf.String(), "req-123") {
		t.Errorf("expected request_id in output: %s", buf.String())
	}
}

func TestWithUserID(t *testing.T) {
	buf := &bytes.Buffer{}
	base := slog.New(slog.NewJSONHandler(buf, nil))
	uid := uuid.New()
	l := kernellog.WithUserID(base, uid)
	l.Info("ping")
	if !strings.Contains(buf.String(), uid.String()) {
		t.Errorf("expected user_id in output: %s", buf.String())
	}
}

func TestWithOrgID(t *testing.T) {
	buf := &bytes.Buffer{}
	base := slog.New(slog.NewJSONHandler(buf, nil))
	oid := uuid.New()
	l := kernellog.WithOrgID(base, oid)
	l.Info("ping")
	if !strings.Contains(buf.String(), oid.String()) {
		t.Errorf("expected org_id in output: %s", buf.String())
	}
}
