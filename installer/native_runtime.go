package installer

import (
	"context"
	"errors"

	"github.com/asteby/metacore-kernel/bundle"
)

// ErrNativeRuntimeUnavailable fails closed when an addon requires a native
// service but the host has not installed a supervisor adapter.
var ErrNativeRuntimeUnavailable = errors.New("installer: native service runtime is unavailable on this host")

// NativeServiceRuntime materialises a signed bundle's long-lived sidecar.
// Ensure must be idempotent and health-gated: on upgrade, the previous healthy
// revision remains authoritative until the replacement reports healthy.
type NativeServiceRuntime interface {
	Ensure(ctx context.Context, b *bundle.Bundle, installation Installation) error
}

type UnsupportedNativeServiceRuntime struct{}

func (UnsupportedNativeServiceRuntime) Ensure(context.Context, *bundle.Bundle, Installation) error {
	return ErrNativeRuntimeUnavailable
}
