package log_test

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	kernellog "github.com/asteby/metacore-kernel/log"
)

func TestHTTPMiddleware_SetsRequestIDHeader(t *testing.T) {
	buf := &bytes.Buffer{}
	logger := slog.New(slog.NewJSONHandler(buf, nil))

	handler := kernellog.HTTPMiddleware(logger)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	rid := rec.Header().Get("X-Request-ID")
	if rid == "" {
		t.Fatal("expected X-Request-ID header to be set")
	}
	if !strings.Contains(buf.String(), rid) {
		t.Errorf("expected request_id %q in log output: %s", rid, buf.String())
	}
}

func TestHTTPMiddleware_ReusesExistingRequestID(t *testing.T) {
	buf := &bytes.Buffer{}
	logger := slog.New(slog.NewJSONHandler(buf, nil))

	handler := kernellog.HTTPMiddleware(logger)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	const existingID = "my-request-id-123"
	req := httptest.NewRequest(http.MethodPost, "/api/v1/foo", nil)
	req.Header.Set("X-Request-ID", existingID)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if got := rec.Header().Get("X-Request-ID"); got != existingID {
		t.Errorf("expected request_id %q, got %q", existingID, got)
	}
	if !strings.Contains(buf.String(), existingID) {
		t.Errorf("expected %q in log output: %s", existingID, buf.String())
	}
}

func TestHTTPMiddleware_LogsStatusAndMethod(t *testing.T) {
	buf := &bytes.Buffer{}
	logger := slog.New(slog.NewJSONHandler(buf, nil))

	handler := kernellog.HTTPMiddleware(logger)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))

	req := httptest.NewRequest(http.MethodDelete, "/things/42", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	out := buf.String()
	for _, want := range []string{"DELETE", "/things/42", "404"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in log output: %s", want, out)
		}
	}
}

func TestHTTPMiddleware_InjectsLoggerIntoContext(t *testing.T) {
	buf := &bytes.Buffer{}
	logger := slog.New(slog.NewJSONHandler(buf, nil))

	var ctxLogger *slog.Logger
	handler := kernellog.HTTPMiddleware(logger)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctxLogger = kernellog.FromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/ctx", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if ctxLogger == nil {
		t.Fatal("expected logger in request context")
	}
}
