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
// calling me" without the addon having to hard-code per-host knowledge.
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
		Host:   "host-a",
		Tenant: "aaaaaaaa-0000-0000-0000-000000000001",
		Extras: map[string]string{
			"X-Metacore-Invocation":     "tool",
			"X-Host-A-Phone":            "+521234",
			"X-Host-A-Conversation-ID":  "conv-42",
		},
	})
	ctx = security.WithEvent(ctx, "order.create")

	_, err := disp.Dispatch(ctx, install, srv.URL+"/webhooks/x", http.MethodPost,
		bytes.NewBufferString(`{"ok":true}`).Bytes())
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}

	cases := map[string]string{
		"X-Metacore-Host":            "host-a",
		"X-Metacore-Tenant":          "aaaaaaaa-0000-0000-0000-000000000001",
		"X-Metacore-Installation-Id": install.String(),
		"X-Metacore-Event":           "order.create",
		"X-Metacore-Invocation":      "tool",
		"X-Host-A-Phone":             "+521234",
		"X-Host-A-Conversation-Id":   "conv-42",
	}
	for k, want := range cases {
		if got.Get(k) != want {
			t.Errorf("header %s = %q, want %q", k, got.Get(k), want)
		}
	}
	if got.Get("X-Metacore-Signature") == "" {
		t.Error("expected signature header, got empty")
	}
	// Host-B-specific header must NOT appear when the host tagged itself as host-a.
	if got.Get("X-Host-B-Branch-Id") != "" {
		t.Error("cross-host header leakage: X-Host-B-Branch-ID set")
	}
}

// TestWithHostContext_OtherExtras mirrors the above from a second host's
// perspective.
func TestWithHostContext_OtherExtras(t *testing.T) {
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
		Host:   "host-b",
		Tenant: "bbbbbbbb-0000-0000-0000-000000000002",
		Extras: map[string]string{
			"X-Metacore-Invocation": "action",
			"X-Host-B-Branch-ID":    "branch-99",
			"X-Host-B-User-ID":      "user-7",
		},
	})
	_, err := disp.Dispatch(ctx, uuid.New(), srv.URL+"/x", http.MethodPost, []byte(`{}`))
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if got.Get("X-Metacore-Host") != "host-b" {
		t.Errorf("X-Metacore-Host = %q, want host-b", got.Get("X-Metacore-Host"))
	}
	if got.Get("X-Host-B-Branch-Id") != "branch-99" {
		t.Errorf("X-Host-B-Branch-ID = %q, want branch-99", got.Get("X-Host-B-Branch-Id"))
	}
	if got.Get("X-Host-A-Phone") != "" {
		t.Errorf("cross-host leak: X-Host-A-Phone present on host-b call")
	}
}
