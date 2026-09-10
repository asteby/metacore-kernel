package installer

import (
	"context"
	"errors"
	"fmt"

	"github.com/asteby/metacore-kernel/bundle"
	"github.com/asteby/metacore-kernel/runtime/native"
)

// ErrNativeRuntimeUnavailable fails closed when an addon requires a native
// service but the host has not installed a supervisor adapter.
var ErrNativeRuntimeUnavailable = errors.New("installer: native service runtime is unavailable on this host")

// NativeServiceRuntime materialises a signed bundle's long-lived sidecar.
// Ensure must be idempotent and health-gated: on upgrade, the previous healthy
// revision remains authoritative until the replacement reports healthy.
type NativeServiceRuntime interface {
	Platform() (os, arch string)
	Ensure(ctx context.Context, b *bundle.Bundle, installation Installation) error
}

type UnsupportedNativeServiceRuntime struct{}

func (UnsupportedNativeServiceRuntime) Platform() (string, string) { return "", "" }

func (UnsupportedNativeServiceRuntime) Ensure(context.Context, *bundle.Bundle, Installation) error {
	return ErrNativeRuntimeUnavailable
}

func verifyNativeBundle(b *bundle.Bundle, runtime NativeServiceRuntime) error {
	os, arch := runtime.Platform()
	if os == "" || arch == "" {
		return ErrNativeRuntimeUnavailable
	}
	artifact, ok := b.Manifest.NativeService.ArtifactFor(os, arch)
	if !ok {
		return fmt.Errorf("installer: native artifact unavailable for %s/%s", os, arch)
	}
	return native.VerifyArtifact(artifact, b.Backend)
}
