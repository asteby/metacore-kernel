// Package webhookin is the kernel-side inbound-webhook receiver (the v3
// `webhooks` block). The host (ops) mounts an HTTP route per addon+org and, on
// a request, calls Receiver.Dispatch with the path, headers and body. The
// receiver verifies the signature (per the route's Verify scheme, against the
// secret resolved from the route's SecretRef connector credential) and routes
// the body to the route's `do` handler through the same prefix-keyed dispatch
// mechanism the stage-machine hooks and the scheduler use.
//
// Back-compat: a host that mounts no routes — or an addon with no `webhooks`
// block — never reaches this package. A route with an empty Verify skips
// signature verification (use only for already-authenticated transports).
package webhookin

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"

	"github.com/google/uuid"
)

// Dispatcher routes a verified webhook body to its `do` handler backend. Mirrors
// dynamic.ActionDispatcher (one impl per wasm|webhook|compiled prefix) but kept
// local so the receiver does not depend on the dynamic engine.
type Dispatcher interface {
	Dispatch(ctx context.Context, orgID uuid.UUID, target string, payload map[string]any) error
}

// SecretResolver resolves a route's SecretRef ("<connector>.<credential>") to
// the signing secret for an org. connectors.Resolver satisfies this (its Secret
// method), so the host wires the same resolver it gives the wasm runtime.
type SecretResolver interface {
	Secret(ctx context.Context, orgID uuid.UUID, ref string) (string, error)
}

// Route is a resolved inbound webhook (mirrors manifest.InboundWebhookDef).
type Route struct {
	// Key identifies the webhook within the addon.
	Key string
	// Path is the route the host mounts (e.g. "/webhooks/github").
	Path string
	// Verify is the signature scheme: "" (none) | "hmac-sha256".
	Verify string
	// SecretRef resolves the signing secret as "<connector>.<credential>".
	SecretRef string
	// Do is the handler reference: "wasm:<export>" | "webhook:<key>" |
	// "compiled:<fn>".
	Do string
}

// Typed errors so the host HTTP layer can map to status codes (404 / 401 / 400 /
// 500) without string matching.
var (
	// ErrRouteNotFound — no route registered at the given path.
	ErrRouteNotFound = errors.New("webhookin: no route for path")
	// ErrSignatureMissing — the request carried no signature header to verify.
	ErrSignatureMissing = errors.New("webhookin: signature header missing")
	// ErrSignatureInvalid — the computed HMAC did not match the request's.
	ErrSignatureInvalid = errors.New("webhookin: signature mismatch")
	// ErrUnsupportedVerify — the route's Verify scheme is not implemented.
	ErrUnsupportedVerify = errors.New("webhookin: unsupported verify scheme")
	// ErrNoDispatcher — no dispatcher registered for the route's do prefix.
	ErrNoDispatcher = errors.New("webhookin: no dispatcher for do prefix")
	// ErrNoSecretResolver — the route requires verification but no SecretResolver
	// is wired.
	ErrNoSecretResolver = errors.New("webhookin: no secret resolver configured")
)

// signatureHeaders are the headers a hmac-sha256 signature may arrive in, in
// preference order. GitHub uses X-Hub-Signature-256; the others cover the common
// generic conventions. The value may be bare hex or "sha256=<hex>".
var signatureHeaders = []string{
	"X-Hub-Signature-256",
	"X-Signature-256",
	"X-Signature",
}

// Receiver holds the registered routes and routes verified bodies to handlers.
// Safe for concurrent use. Construct it with New.
type Receiver struct {
	mu          sync.RWMutex
	routes      map[string]Route // keyed by Path
	dispatchers map[string]Dispatcher
	secrets     SecretResolver
}

// New constructs a Receiver. dispatchers is keyed by the `do` prefix
// (wasm/webhook/compiled). secrets may be nil when no registered route declares
// a Verify scheme (Dispatch of a verifying route then returns ErrNoSecretResolver).
func New(dispatchers map[string]Dispatcher, secrets SecretResolver) *Receiver {
	if dispatchers == nil {
		dispatchers = map[string]Dispatcher{}
	}
	return &Receiver{
		routes:      map[string]Route{},
		dispatchers: dispatchers,
		secrets:     secrets,
	}
}

// Register adds (or replaces) a route by its Path. Idempotent: re-registering
// the same path overwrites, so the host's boot loop never duplicates routes.
func (r *Receiver) Register(route Route) error {
	if route.Path == "" {
		return fmt.Errorf("webhookin: route %q has empty path", route.Key)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.routes[route.Path] = route
	return nil
}

// Unregister removes the route at path. Unknown paths are a no-op.
func (r *Receiver) Unregister(path string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.routes, path)
}

// Dispatch verifies and routes an inbound webhook for an org. It looks up the
// route by path, verifies the signature (when the route declares a Verify
// scheme) using the secret resolved from the route's SecretRef, and dispatches
// the body to the route's `do` handler. The orgID scopes the secret resolution
// and the dispatch; the host derives it from the mount (addon+org namespace).
//
// Returns a typed error (ErrRouteNotFound / ErrSignatureMissing /
// ErrSignatureInvalid / ErrUnsupportedVerify / ...) so the caller maps the HTTP
// status; nil means the handler was dispatched.
func (r *Receiver) Dispatch(ctx context.Context, orgID uuid.UUID, path string, headers http.Header, body []byte) error {
	r.mu.RLock()
	route, ok := r.routes[path]
	dispatcher, hasDisp := r.dispatchers[prefixOf(route.Do)]
	secrets := r.secrets
	r.mu.RUnlock()
	if !ok {
		return fmt.Errorf("%w: %q", ErrRouteNotFound, path)
	}

	if route.Verify != "" {
		if err := verifySignature(ctx, route, orgID, headers, body, secrets); err != nil {
			return err
		}
	}

	if !hasDisp {
		return fmt.Errorf("%w: %q (route %q)", ErrNoDispatcher, route.Do, route.Key)
	}
	_, target, _ := strings.Cut(route.Do, ":")
	payload := map[string]any{
		"org":     orgID.String(),
		"webhook": route.Key,
		"path":    path,
		"body":    bodyValue(body),
	}
	return dispatcher.Dispatch(ctx, orgID, target, payload)
}

// verifySignature checks the request signature against the secret resolved from
// the route's SecretRef. Only hmac-sha256 is implemented.
func verifySignature(ctx context.Context, route Route, orgID uuid.UUID, headers http.Header, body []byte, secrets SecretResolver) error {
	if route.Verify != "hmac-sha256" {
		return fmt.Errorf("%w: %q", ErrUnsupportedVerify, route.Verify)
	}
	if secrets == nil {
		return ErrNoSecretResolver
	}
	secret, err := secrets.Secret(ctx, orgID, route.SecretRef)
	if err != nil {
		return fmt.Errorf("webhookin: resolve secret_ref %q: %w", route.SecretRef, err)
	}
	provided := extractSignature(headers)
	if provided == "" {
		return ErrSignatureMissing
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	expected := hex.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(expected), []byte(provided)) {
		return ErrSignatureInvalid
	}
	return nil
}

// extractSignature reads the first present signature header and normalises it to
// bare lowercase hex (stripping a "sha256=" prefix and surrounding space).
func extractSignature(headers http.Header) string {
	for _, h := range signatureHeaders {
		v := strings.TrimSpace(headers.Get(h))
		if v == "" {
			continue
		}
		v = strings.TrimPrefix(v, "sha256=")
		return strings.ToLower(strings.TrimSpace(v))
	}
	return ""
}

// prefixOf returns the dispatch prefix of a `do` reference ("" when none).
func prefixOf(do string) string {
	prefix, _, found := strings.Cut(do, ":")
	if !found {
		return ""
	}
	return prefix
}

// bodyValue passes the body to the handler as parsed JSON when it is valid JSON,
// else as a raw string — the handler decides how to read it.
func bodyValue(body []byte) any {
	if json.Valid(body) {
		return json.RawMessage(body)
	}
	return string(body)
}
