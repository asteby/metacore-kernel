package security_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/asteby/metacore-kernel/security"
	"github.com/google/uuid"
)

func TestWebhookDispatcher_SignsRequest(t *testing.T) {
	body := []byte(`{"event":"order.created","id":42}`)
	instID := uuid.New()
	secret := []byte("shhh")

	var gotHeaders http.Header
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeaders = r.Header.Clone()
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	signer := security.NewSigner(secret)
	signer.Clock = func() time.Time { return time.Unix(1_700_000_000, 0) }

	d := security.NewWebhookDispatcher(func(id uuid.UUID) (*security.Signer, error) {
		if id != instID {
			t.Fatalf("unexpected installation %s", id)
		}
		return signer, nil
	})

	ctx := security.WithEvent(context.Background(), "order.created")
	resp, err := d.Dispatch(ctx, instID, srv.URL+"/hook", http.MethodPost, body)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	resp.Body.Close()

	if got := gotHeaders.Get("X-Metacore-Installation-ID"); got != instID.String() {
		t.Errorf("installation header = %q want %q", got, instID.String())
	}
	if got := gotHeaders.Get("X-Metacore-Event"); got != "order.created" {
		t.Errorf("event header = %q", got)
	}
	ts := gotHeaders.Get("X-Metacore-Timestamp")
	sig := gotHeaders.Get("X-Metacore-Signature")
	nonce := gotHeaders.Get("X-Metacore-Nonce")
	if ts == "" || sig == "" || nonce == "" {
		t.Fatalf("missing signature headers: ts=%q sig=%q nonce=%q", ts, sig, nonce)
	}
	// Round-trip verification — receiver side would do the same and also
	// feed the nonce through a NonceCache to reject replays.
	if err := signer.VerifyWithNonce(http.MethodPost, "/hook", gotBody, ts, nonce, sig, time.Minute); err != nil {
		t.Fatalf("verify: %v", err)
	}
	// Nonce must not be reusable — replayed verbatim, a NonceCache rejects it.
	cache := security.NewNonceCache(time.Minute)
	if err := cache.CheckAndRecord(nonce); err != nil {
		t.Fatalf("first nonce record: %v", err)
	}
	if err := cache.CheckAndRecord(nonce); err == nil {
		t.Fatalf("expected replay rejection on second record")
	}
}

func TestWebhookDispatcher_UnsignedWhenNoSigner(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Metacore-Signature") != "" {
			t.Errorf("unexpected signature header on unsigned dispatch")
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	d := security.NewWebhookDispatcher(func(id uuid.UUID) (*security.Signer, error) { return nil, nil })
	resp, err := d.Dispatch(context.Background(), uuid.New(), srv.URL, http.MethodPost, []byte(`{}`))
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	resp.Body.Close()
}
