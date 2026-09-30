package dynamic

import (
	"context"
	"errors"
	"fmt"
)

// ErrAppendOnly is returned by Service.Update / Service.Delete when the model is
// declared append-only (manifest Model.append_only): a ledger row is written
// once and never modified or removed — a mistake is fixed with a reversing row.
// The handler maps it to HTTP 409 Conflict with code "append_only". The wasm
// data_mutate / data_batch imports report the same stable code.
var ErrAppendOnly = errors.New("append_only")

// AppendOnlyError is the typed form of ErrAppendOnly: it names the model and
// the refused operation so the message is actionable. errors.Is(err,
// ErrAppendOnly) matches it.
type AppendOnlyError struct {
	Model string
	// Op is "update" or "delete".
	Op string
}

func (e *AppendOnlyError) Error() string {
	return fmt.Sprintf("append_only: %s is an append-only ledger, %s is not allowed (post a reversing row instead)", e.Model, e.Op)
}

// Unwrap ties the typed error to the ErrAppendOnly sentinel.
func (e *AppendOnlyError) Unwrap() error { return ErrAppendOnly }

// AppendOnlyResolver reports whether a model is an append-only ledger. Hosts
// wire it from their addon registry, like ConstraintResolver: the kernel owns
// no global model index. nil (or false) leaves the model writable — the legacy
// behaviour.
type AppendOnlyResolver func(ctx context.Context, model string) bool

// refuseIfAppendOnly returns an *AppendOnlyError when model is an append-only
// ledger, nil otherwise. op is "update" or "delete".
func (s *Service) refuseIfAppendOnly(ctx context.Context, model, op string) error {
	if s.appendOnly == nil || !s.appendOnly(ctx, model) {
		return nil
	}
	return &AppendOnlyError{Model: model, Op: op}
}
