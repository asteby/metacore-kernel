package security_test

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/asteby/metacore-kernel/security"
	"github.com/google/uuid"
)

// TestWithHostContext_EmitsHeaders asserts the dispatcher forwards every
// header a host attached via WithHostContext, in addition to the baseline
// Metacore headers. This is the contract addons rely on to resolve "who is
// calling me" without the addon having to hard-code ops-vs-link knowledge.
func TestWithHostContext_EmitsHeaders(t *testing.T) {
	var got http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	secret := []byte("round-trip-secret-any-length-is-fine")
	install := uuid.New()
	disp := &security.WebhookDispatcher{
		HTTPClient: srv.Client(),
		SignerLookup: func(_ uuid.UUID) (*security.Signer, error) {
			return security.NewSigner(secret), nil
		},
	}

	ctx := security.WithHostContext(context.Background(), security.HostContext{
		Host:   "link",
		Tenant: "aaaaaaaa-0000-0000-0000-000000000001",
		Extras: map[string]string{
			"X-Metacore-Invocation":    "tool",
			"X-Link-Phone":             "+521234",
			"X-Link-Conversation-ID":   "conv-42",
		},
	})
	ctx = security.WithEvent(ctx, "order.create")

	_, err := disp.Dispatch(ctx, install, srv.URL+"/webhooks/x", http.MethodPost,
		bytes.NewBufferString(`{"ok":true}`).Bytes())
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}

	cases := map[string]string{
		"X-Metacore-Host":           "link",
		"X-Metacore-Tenant":         "aaaaaaaa-0000-0000-0000-000000000001",
		"X-Metacore-Installation-Id": install.String(),
		"X-Metacore-Event":          "order.create",
		"X-Metacore-Invocation":     "tool",
		"X-Link-Phone":              "+521234",
		"X-Link-Conversation-Id":    "conv-42",
	}
	for k, want := range cases {
		if got.Get(k) != want {
			t.Errorf("header %s = %q, want %q", k, got.Get(k), want)
		}
	}
	if got.Get("X-Metacore-Signature") == "" {
		t.Error("expected signature header, got empty")
	}
	// Ops-specific header must NOT appear when the host tagged itself as link.
	if got.Get("X-Ops-Branch-Id") != "" {
		t.Error("cross-host header leakage: X-Ops-Branch-ID set")
	}
}

// TestWithHostContext_OpsExtras mirrors the above from an ops perspective.
func TestWithHostContext_OpsExtras(t *testing.T) {
	var got http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	disp := &security.WebhookDispatcher{
		HTTPClient: srv.Client(),
	}
	ctx := security.WithHostContext(context.Background(), security.HostContext{
		Host:   "ops",
		Tenant: "bbbbbbbb-0000-0000-0000-000000000002",
		Extras: map[string]string{
			"X-Metacore-Invocation": "action",
			"X-Ops-Branch-ID":       "branch-99",
			"X-Ops-User-ID":         "user-7",
		},
	})
	_, err := disp.Dispatch(ctx, uuid.New(), srv.URL+"/x", http.MethodPost, []byte(`{}`))
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if got.Get("X-Metacore-Host") != "ops" {
		t.Errorf("X-Metacore-Host = %q, want ops", got.Get("X-Metacore-Host"))
	}
	if got.Get("X-Ops-Branch-Id") != "branch-99" {
		t.Errorf("X-Ops-Branch-ID = %q, want branch-99", got.Get("X-Ops-Branch-Id"))
	}
	if got.Get("X-Link-Phone") != "" {
		t.Errorf("cross-host leak: X-Link-Phone present on ops call")
	}
}
