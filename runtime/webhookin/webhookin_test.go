package webhookin

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"sync"
	"testing"

	"github.com/google/uuid"
)

// staticSecrets resolves any ref to a fixed secret (or an error).
type staticSecrets struct {
	secret string
	err    error
}

func (s staticSecrets) Secret(context.Context, uuid.UUID, string) (string, error) {
	return s.secret, s.err
}

// captureDispatcher records the routed invocation.
type captureDispatcher struct {
	mu      sync.Mutex
	target  string
	payload map[string]any
	calls   int
}

func (d *captureDispatcher) Dispatch(_ context.Context, _ uuid.UUID, target string, payload map[string]any) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.target = target
	d.payload = payload
	d.calls++
	return nil
}

func sign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

func githubRoute() Route {
	return Route{
		Key:       "github_push",
		Path:      "/webhooks/github",
		Verify:    "hmac-sha256",
		SecretRef: "github.webhook_secret",
		Do:        "wasm:ingest_webhook",
	}
}

func TestDispatchValidSignatureRoutes(t *testing.T) {
	secret := "shh"
	body := []byte(`{"action":"closed"}`)
	disp := &captureDispatcher{}
	r := New(map[string]Dispatcher{"wasm": disp}, staticSecrets{secret: secret})
	if err := r.Register(githubRoute()); err != nil {
		t.Fatalf("Register: %v", err)
	}

	h := http.Header{}
	h.Set("X-Hub-Signature-256", "sha256="+sign(secret, body))
	if err := r.Dispatch(context.Background(), uuid.New(), "/webhooks/github", h, body); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if disp.calls != 1 {
		t.Fatalf("want 1 dispatch, got %d", disp.calls)
	}
	if disp.target != "ingest_webhook" {
		t.Fatalf("want target ingest_webhook, got %q", disp.target)
	}
	if disp.payload["webhook"] != "github_push" {
		t.Fatalf("payload missing webhook key: %v", disp.payload)
	}
}

func TestDispatchInvalidSignatureRejected(t *testing.T) {
	body := []byte(`{"action":"closed"}`)
	disp := &captureDispatcher{}
	r := New(map[string]Dispatcher{"wasm": disp}, staticSecrets{secret: "shh"})
	_ = r.Register(githubRoute())

	h := http.Header{}
	h.Set("X-Hub-Signature-256", "sha256="+sign("wrong-secret", body))
	err := r.Dispatch(context.Background(), uuid.New(), "/webhooks/github", h, body)
	if !errors.Is(err, ErrSignatureInvalid) {
		t.Fatalf("want ErrSignatureInvalid, got %v", err)
	}
	if disp.calls != 0 {
		t.Fatalf("must not dispatch on bad signature, got %d calls", disp.calls)
	}
}

func TestDispatchMissingSignature(t *testing.T) {
	disp := &captureDispatcher{}
	r := New(map[string]Dispatcher{"wasm": disp}, staticSecrets{secret: "shh"})
	_ = r.Register(githubRoute())
	err := r.Dispatch(context.Background(), uuid.New(), "/webhooks/github", http.Header{}, []byte(`{}`))
	if !errors.Is(err, ErrSignatureMissing) {
		t.Fatalf("want ErrSignatureMissing, got %v", err)
	}
}

func TestDispatchUnknownRoute(t *testing.T) {
	r := New(map[string]Dispatcher{"wasm": &captureDispatcher{}}, staticSecrets{secret: "shh"})
	err := r.Dispatch(context.Background(), uuid.New(), "/nope", http.Header{}, nil)
	if !errors.Is(err, ErrRouteNotFound) {
		t.Fatalf("want ErrRouteNotFound, got %v", err)
	}
}

func TestDispatchNoVerifySkipsSignature(t *testing.T) {
	disp := &captureDispatcher{}
	r := New(map[string]Dispatcher{"wasm": disp}, nil)
	_ = r.Register(Route{Key: "k", Path: "/in", Do: "wasm:ingest"})
	if err := r.Dispatch(context.Background(), uuid.New(), "/in", http.Header{}, []byte(`{}`)); err != nil {
		t.Fatalf("Dispatch (no verify): %v", err)
	}
	if disp.calls != 1 {
		t.Fatalf("want 1 dispatch, got %d", disp.calls)
	}
}

func TestDispatchUnsupportedVerify(t *testing.T) {
	r := New(map[string]Dispatcher{"wasm": &captureDispatcher{}}, staticSecrets{secret: "x"})
	_ = r.Register(Route{Key: "k", Path: "/in", Verify: "rsa-sha1", SecretRef: "github.s", Do: "wasm:x"})
	err := r.Dispatch(context.Background(), uuid.New(), "/in", http.Header{}, []byte(`{}`))
	if !errors.Is(err, ErrUnsupportedVerify) {
		t.Fatalf("want ErrUnsupportedVerify, got %v", err)
	}
}

func TestDispatchNoSecretResolver(t *testing.T) {
	r := New(map[string]Dispatcher{"wasm": &captureDispatcher{}}, nil)
	_ = r.Register(githubRoute())
	body := []byte(`{}`)
	h := http.Header{}
	h.Set("X-Hub-Signature-256", sign("shh", body))
	err := r.Dispatch(context.Background(), uuid.New(), "/webhooks/github", h, body)
	if !errors.Is(err, ErrNoSecretResolver) {
		t.Fatalf("want ErrNoSecretResolver, got %v", err)
	}
}

func TestDispatchBareHexSignature(t *testing.T) {
	// A provider that sends bare hex (no "sha256=" prefix) still verifies.
	secret, body := "shh", []byte(`{"x":1}`)
	disp := &captureDispatcher{}
	r := New(map[string]Dispatcher{"wasm": disp}, staticSecrets{secret: secret})
	_ = r.Register(githubRoute())
	h := http.Header{}
	h.Set("X-Signature", sign(secret, body))
	if err := r.Dispatch(context.Background(), uuid.New(), "/webhooks/github", h, body); err != nil {
		t.Fatalf("Dispatch bare hex: %v", err)
	}
	if disp.calls != 1 {
		t.Fatalf("want 1 dispatch, got %d", disp.calls)
	}
}
