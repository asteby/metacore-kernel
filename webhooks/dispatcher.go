package webhooks

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"
)

// newRequest builds the outbound HTTP request with HMAC-SHA256 signing headers.
//
// Headers:
//
//	X-Metacore-Event      the event key (e.g. "order.created")
//	X-Metacore-Timestamp  unix seconds
//	X-Metacore-Signature  hex(hmac-sha256(secret, timestamp + "." + body))
//	X-Metacore-Delivery   unique per attempt (guid-ish)
//
// This matches the scheme documented in security/webhookdispatch.go; the
// receiver verifies by recomputing the HMAC with the per-webhook secret.
func newRequest(ctx context.Context, w *Webhook, event string, body []byte, now time.Time) (*http.Request, error) {
	ts := strconv.FormatInt(now.Unix(), 10)
	sig := computeSignature([]byte(w.Secret), ts, body)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.URL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("webhooks: new request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Metacore-Event", event)
	req.Header.Set("X-Metacore-Timestamp", ts)
	req.Header.Set("X-Metacore-Signature", sig)
	req.Header.Set("User-Agent", "metacore-webhooks/1.0")
	return req, nil
}

// computeSignature is exported implicitly via the header. Test helper below.
func computeSignature(secret []byte, timestamp string, body []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(timestamp))
	mac.Write([]byte("."))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// marshalJSON is the real JSON marshaller (service.go has a no-op stub for
// cyclic-import-free compilation; we override via build tag here).
func marshalJSON(v any) ([]byte, error) { return json.Marshal(v) }
