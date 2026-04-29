// Package log provides structured slog-based logging utilities and HTTP/Fiber
// middleware for the metacore kernel. Both Fiber and net/http middlewares are
// supported and can coexist in the same binary (e.g. Fiber for the main API,
// net/http for an admin server or test helpers).
package log

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
)

const headerRequestID = "X-Request-ID"

// statusRecorder wraps http.ResponseWriter to capture the written status code.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// HTTPMiddleware returns a net/http middleware that:
//   - Reads or generates a request_id (UUID) from/for the X-Request-ID header.
//   - Injects a child logger (with request_id) into the request context via
//     WithLogger, so downstream handlers can retrieve it with FromContext.
//   - Logs every request after completion: method, path, status, duration,
//     and request_id.
func HTTPMiddleware(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()

			// Resolve or generate request_id.
			requestID := r.Header.Get(headerRequestID)
			if requestID == "" {
				requestID = uuid.New().String()
			}
			w.Header().Set(headerRequestID, requestID)

			// Build a child logger for this request.
			reqLogger := WithRequestID(logger, requestID)

			// Inject into context so handlers can call FromContext(r.Context()).
			ctx := WithLogger(r.Context(), reqLogger)
			r = r.WithContext(ctx)

			// Wrap the writer to capture the status code.
			rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}

			next.ServeHTTP(rec, r)

			duration := time.Since(start)
			reqLogger.Info("request",
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.Int("status", rec.status),
				slog.Duration("duration", duration),
				slog.String("request_id", requestID),
			)
		})
	}
}

// FiberMiddleware returns a Fiber middleware that:
//   - Reads or generates a request_id (UUID) from/for the X-Request-ID header.
//   - Injects a child logger (with request_id) into the Fiber context locals
//     and into the standard context.Context stored in c.
//   - Logs every request after completion: method, path, status, duration, request_id.
func FiberMiddleware(logger *slog.Logger) fiber.Handler {
	return func(c fiber.Ctx) error {
		start := time.Now()

		// Resolve or generate request_id.
		requestID := c.Get(headerRequestID)
		if requestID == "" {
			requestID = uuid.New().String()
		}
		c.Set(headerRequestID, requestID)

		// Build a child logger for this request.
		reqLogger := WithRequestID(logger, requestID)

		// Store in Fiber locals for handlers that use c.Locals.
		c.Locals("logger", reqLogger)

		// Store in the standard context so downstream service calls can use
		// FromContext(c).
		ctx := WithLogger(context.Background(), reqLogger)
		c.SetContext(ctx)

		// Process the rest of the chain.
		err := c.Next()

		// Log after the response is written.
		duration := time.Since(start)
		status := c.Response().StatusCode()

		reqLogger.Info("request",
			slog.String("method", c.Method()),
			slog.String("path", c.Path()),
			slog.Int("status", status),
			slog.Duration("duration", duration),
			slog.String("request_id", requestID),
		)

		return err
	}
}

// FromFiberCtx returns the request-scoped logger stored by FiberMiddleware.
// Falls back to the provided base logger if none was injected (e.g. in tests).
func FromFiberCtx(c fiber.Ctx, fallback *slog.Logger) *slog.Logger {
	if l, ok := c.Locals("logger").(*slog.Logger); ok && l != nil {
		return l
	}
	if fallback != nil {
		return fallback
	}
	return slog.Default()
}
