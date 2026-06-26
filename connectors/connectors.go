// Package connectors is the kernel-side runtime for addon credential providers
// (the v3 `connectors` block). The kernel never persists credentials: the host
// (ops) owns encrypted per-org storage and implements Store; this package
// resolves a connector_ref → credential map for wasm handlers (the connector_get
// capability) and resolves a webhook's secret_ref → a single credential value
// for the webhookin receiver.
//
// Back-compat: a nil *Resolver (or a Resolver over a nil Store) is the
// feature-off state — every lookup returns ErrNoStore and callers degrade
// gracefully (an addon without connectors never reaches this package).
package connectors

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
)

// Store is the host-implemented credential backend. The host persists a
// connector's credentials per org (encrypted at rest — the kernel sees only the
// decrypted map) and resolves them here. Get returns (creds, true, nil) when the
// org has authorised the connector, (nil, false, nil) when it has not, and a
// non-nil error only on an actual backend failure.
type Store interface {
	Get(ctx context.Context, orgID uuid.UUID, connectorKey string) (map[string]string, bool, error)
}

// Resolver resolves connector credentials over a host Store. Construct it with
// NewResolver; a nil *Resolver is a valid feature-off value (every method
// returns ErrNoStore).
type Resolver struct {
	store Store
}

// NewResolver wraps a host Store. Passing a nil Store yields a Resolver whose
// lookups all return ErrNoStore (the feature-off state).
func NewResolver(store Store) *Resolver {
	return &Resolver{store: store}
}

var (
	// ErrNoStore is returned when no credential Store is wired (the feature-off
	// state) — the host has not enabled the connectors runtime.
	ErrNoStore = errors.New("connectors: no credential store configured")
	// ErrConnectorNotFound is returned when the org has not authorised the
	// requested connector (Store.Get reported it absent).
	ErrConnectorNotFound = errors.New("connectors: connector not found for org")
	// ErrCredentialNotFound is returned when a resolved connector exists but
	// lacks the requested credential field.
	ErrCredentialNotFound = errors.New("connectors: credential not found on connector")
	// ErrInvalidRef is returned when a secret_ref is not the
	// "<connector>.<credential>" shape.
	ErrInvalidRef = errors.New("connectors: invalid secret_ref (want \"<connector>.<credential>\")")
)

// Get resolves the full decrypted credential map for (org, connectorKey). It
// returns ErrNoStore when the runtime is off and ErrConnectorNotFound when the
// org has not authorised the connector.
func (r *Resolver) Get(ctx context.Context, orgID uuid.UUID, connectorKey string) (map[string]string, error) {
	if r == nil || r.store == nil {
		return nil, ErrNoStore
	}
	creds, ok, err := r.store.Get(ctx, orgID, connectorKey)
	if err != nil {
		return nil, fmt.Errorf("connectors: store get %q: %w", connectorKey, err)
	}
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrConnectorNotFound, connectorKey)
	}
	return creds, nil
}

// Secret resolves a "<connector>.<credential>" reference (e.g.
// "github.webhook_secret") to a single credential value for the given org. The
// webhookin receiver uses it to resolve an InboundWebhook.SecretRef into the
// signing secret. Returns ErrInvalidRef / ErrConnectorNotFound /
// ErrCredentialNotFound as appropriate.
func (r *Resolver) Secret(ctx context.Context, orgID uuid.UUID, ref string) (string, error) {
	if r == nil || r.store == nil {
		return "", ErrNoStore
	}
	conn, cred, ok := strings.Cut(ref, ".")
	if !ok || conn == "" || cred == "" {
		return "", fmt.Errorf("%w: %q", ErrInvalidRef, ref)
	}
	creds, err := r.Get(ctx, orgID, conn)
	if err != nil {
		return "", err
	}
	val, ok := creds[cred]
	if !ok {
		return "", fmt.Errorf("%w: %q on %q", ErrCredentialNotFound, cred, conn)
	}
	return val, nil
}
