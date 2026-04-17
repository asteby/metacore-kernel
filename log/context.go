package log

import (
	"context"
	"log/slog"

	"github.com/google/uuid"
)

type contextKey struct{}

// WithLogger stores logger in ctx. Retrieve it with FromContext.
func WithLogger(ctx context.Context, logger *slog.Logger) context.Context {
	return context.WithValue(ctx, contextKey{}, logger)
}

// FromContext returns the logger stored in ctx by WithLogger.
// Falls back to the global slog default if none is found.
func FromContext(ctx context.Context) *slog.Logger {
	if l, ok := ctx.Value(contextKey{}).(*slog.Logger); ok && l != nil {
		return l
	}
	return slog.Default()
}

// WithRequestID returns a child logger with the request_id attribute pre-set.
func WithRequestID(logger *slog.Logger, requestID string) *slog.Logger {
	return logger.With(slog.String("request_id", requestID))
}

// WithUserID returns a child logger with the user_id attribute pre-set.
func WithUserID(logger *slog.Logger, userID uuid.UUID) *slog.Logger {
	return logger.With(slog.String("user_id", userID.String()))
}

// WithOrgID returns a child logger with the org_id attribute pre-set.
func WithOrgID(logger *slog.Logger, orgID uuid.UUID) *slog.Logger {
	return logger.With(slog.String("org_id", orgID.String()))
}
