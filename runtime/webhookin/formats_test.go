package webhookin

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net/http"
	"testing"

	"github.com/google/uuid"
)

func signB64(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

func wooRoute() Route {
	return Route{
		Key:       "woo_orders",
		Path:      "/webhooks/woocommerce",
		Verify:    "hmac-sha256",
		SecretRef: "woocommerce.webhook_secret",
		Do:        "wasm:ingest_webhook",
	}
}

// QA-EXP-ROLES / EXP-ROL-16: WooCommerce firma en X-WC-Webhook-Signature con
// base64(HMAC-SHA256(body)); el receptor solo leía hex, así que toda entrega
// real de Woo daba 401.
func TestDispatchWooCommerceBase64Signature(t *testing.T) {
	secret := "woo-secret"
	body := []byte(`{"id":123,"status":"processing"}`)
	disp := &captureDispatcher{}
	r := New(map[string]Dispatcher{"wasm": disp}, staticSecrets{secret: secret})
	_ = r.Register(wooRoute())
	ctx, org := context.Background(), uuid.New()

	h := http.Header{}
	h.Set("X-WC-Webhook-Signature", signB64(secret, body))
	if err := r.Dispatch(ctx, org, "/webhooks/woocommerce", h, body); err != nil {
		t.Fatalf("firma base64 válida de Woo: %v", err)
	}

	bad := http.Header{}
	bad.Set("X-WC-Webhook-Signature", signB64("otro", body))
	if err := r.Dispatch(ctx, org, "/webhooks/woocommerce", bad, body); !errors.Is(err, ErrSignatureInvalid) {
		t.Fatalf("base64 con otro secreto: want ErrSignatureInvalid, got %v", err)
	}
	tampered := http.Header{}
	tampered.Set("X-WC-Webhook-Signature", signB64(secret, body))
	if err := r.Dispatch(ctx, org, "/webhooks/woocommerce", tampered, append(body, ' ')); !errors.Is(err, ErrSignatureInvalid) {
		t.Fatalf("cuerpo alterado: want ErrSignatureInvalid, got %v", err)
	}
	garbage := http.Header{}
	garbage.Set("X-WC-Webhook-Signature", "not-a-digest")
	if err := r.Dispatch(ctx, org, "/webhooks/woocommerce", garbage, body); !errors.Is(err, ErrSignatureInvalid) {
		t.Fatalf("firma basura: want ErrSignatureInvalid, got %v", err)
	}
	if disp.calls != 1 {
		t.Fatalf("dispatch calls = %d, want 1", disp.calls)
	}
}

func TestDispatchHexSignatureStillAccepted(t *testing.T) {
	secret := "shh"
	body := []byte(`{}`)
	disp := &captureDispatcher{}
	r := New(map[string]Dispatcher{"wasm": disp}, staticSecrets{secret: secret})
	_ = r.Register(githubRoute())
	for _, v := range []string{sign(secret, body), "sha256=" + sign(secret, body), "SHA256=" + sign(secret, body)} {
		h := http.Header{}
		h.Set("X-Signature-256", v)
		if err := r.Dispatch(context.Background(), uuid.New(), "/webhooks/github", h, body); err != nil {
			t.Fatalf("hex %q: %v", v, err)
		}
	}
}

// Un secreto vacío dejaba pasar un HMAC con clave "" que cualquiera calcula.
func TestDispatchEmptySecretRejected(t *testing.T) {
	body := []byte(`{"id":1}`)
	disp := &captureDispatcher{}
	for _, secret := range []string{"", "   "} {
		r := New(map[string]Dispatcher{"wasm": disp}, staticSecrets{secret: secret})
		_ = r.Register(wooRoute())
		h := http.Header{}
		h.Set("X-WC-Webhook-Signature", signB64(secret, body))
		h.Set("X-Hub-Signature-256", "sha256="+sign(secret, body))
		err := r.Dispatch(context.Background(), uuid.New(), "/webhooks/woocommerce", h, body)
		if !errors.Is(err, ErrSecretNotConfigured) || !errors.Is(err, ErrSignatureInvalid) {
			t.Fatalf("secreto %q: want ErrSecretNotConfigured+ErrSignatureInvalid, got %v", secret, err)
		}
	}
	if disp.calls != 0 {
		t.Fatalf("no debe despachar con secreto vacío, calls=%d", disp.calls)
	}
}
