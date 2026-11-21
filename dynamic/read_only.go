package dynamic

import (
	"context"
	"errors"
	"fmt"
)

// ErrReadOnly is returned by every write of Service (Create, Update, Delete,
// Restore, bulk delete) on a read-only model: one backed by a SQL view
// (manifest Model.view). The handler maps it to HTTP 405 with code "read_only".
var ErrReadOnly = errors.New("read_only")

// ReadOnlyError is the typed form of ErrReadOnly. errors.Is(err, ErrReadOnly)
// matches it.
type ReadOnlyError struct {
	Model string
	// Op is "create", "update" or "delete".
	Op string
}

func (e *ReadOnlyError) Error() string {
	return fmt.Sprintf("read_only: %s is a read-only view, %s is not allowed", e.Model, e.Op)
}

// Unwrap ties the typed error to the ErrReadOnly sentinel.
func (e *ReadOnlyError) Unwrap() error { return ErrReadOnly }

// ReadOnlyResolver reports whether a model is read-only (view-backed). Hosts
// wire it from their addon registry, like AppendOnlyResolver. nil leaves every
// model writable.
type ReadOnlyResolver func(ctx context.Context, model string) bool

func (s *Service) refuseIfReadOnly(ctx context.Context, model, op string) error {
	if s.readOnly == nil || !s.readOnly(ctx, model) {
		return nil
	}
	return &ReadOnlyError{Model: model, Op: op}
}
