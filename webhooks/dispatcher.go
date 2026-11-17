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

	"github.com/asteby/metacore-kernel/runtime/webhookout"
	"github.com/asteby/metacore-kernel/webhooks/signing"
)

// newRequest builds the outbound HTTP request with HMAC-SHA256 signing headers.
//
// Headers:
//
//	X-Metacore-Event      the event key (e.g. "order.created")
//	X-Metacore-Timestamp  unix seconds
//	X-Metacore-Signature  hex(hmac-sha256(secret, timestamp + "." + body))
//	X-Metacore-Delivery   delivery id, stable across retries (set by attemptDelivery)
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

// attemptDeclarative performs one HTTP attempt of a declarative subscription.
//
// d.Payload is the ALREADY transformed body (built when the event arrived), so
// the bytes are identical on every retry. The idempotency key is stored on the
// delivery and sent on every attempt; only the signature timestamp/nonce change
// (a receiver rejects a repeated nonce). Secrets are resolved by reference on
// each attempt (rotation-friendly) and never stored or logged.
func (s *Service) attemptDeclarative(ctx context.Context, w *Webhook, d *WebhookDelivery) {
	dec := w.Declarative
	d.AttemptCount++
	d.InFlight = false

	body, err := json.Marshal(d.Payload)
	if err != nil {
		s.persistFailure(ctx, w, d, 0, "marshal body: "+err.Error(), true)
		return
	}
	timeout := time.Duration(w.TimeoutSec) * time.Second
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	rctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(rctx, dec.method(), w.URL, bytes.NewReader(body))
	if err != nil {
		s.persistFailure(ctx, w, d, 0, "new request: "+err.Error(), true)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "metacore-webhooks/1.0")
	req.Header.Set("X-Metacore-Event", d.Event)
	req.Header.Set("X-Metacore-Delivery", d.ID.String())
	// Request fingerprint: lets the receiver tell "same key, same request"
	// (replayed) from "same key, different body" (idempotency_conflict).
	fp := sha256.Sum256(body)
	req.Header.Set("X-Metacore-Body-SHA256", hex.EncodeToString(fp[:]))
	for k, v := range dec.Headers {
		req.Header.Set(k, v)
	}
	if d.IdempotencyKey != "" {
		req.Header.Set(dec.idemHeader(), d.IdempotencyKey)
	}

	now := s.clock()
	if dec.Signing != nil {
		sec, err := s.resolveSecrets(ctx, dec.Signing)
		if err != nil {
			// Retryable: the secret may be provisioned/rotated meanwhile.
			s.persistFailure(ctx, w, d, 0, err.Error(), false)
			return
		}
		hdrs, err := signing.Sign(*dec.Signing, sec, signing.Request{
			Method: req.Method, Path: signing.PathOf(req.URL), Body: body,
		}, now, "")
		if err != nil {
			s.persistFailure(ctx, w, d, 0, "sign: "+err.Error(), true)
			return
		}
		for k, v := range hdrs {
			req.Header.Set(k, v)
		}
	} else {
		ts := strconv.FormatInt(now.Unix(), 10)
		req.Header.Set("X-Metacore-Timestamp", ts)
		req.Header.Set("X-Metacore-Signature", computeSignature([]byte(w.Secret), ts, body))
	}

	resp, err := s.egress.Do(req)
	if err != nil {
		s.persistFailure(ctx, w, d, 0, err.Error(), false)
		return
	}
	defer resp.Body.Close()
	status := resp.StatusCode
	respBody := safeResponseBody(readLimited(resp.Body, 8*1024))

	if status >= 200 && status < 300 {
		s.persistSuccess(ctx, w, d, status, respBody, true)
		return
	}
	s.persistFailure(ctx, w, d, status, respBody, isPermanentStatus(status))
}

// isPermanentStatus: a 4xx means the request itself is wrong, so retrying the
// same bytes cannot help — except the transient ones the contract of V2 marks
// as retryable (408, 409 in_progress, 423 account_paused, 425, 429).
func isPermanentStatus(status int) bool {
	if status < 400 || status >= 500 {
		return false
	}
	switch status {
	case 408, 409, 423, 425, 429:
		return false
	}
	return true
}

func (s *Service) resolveSecrets(ctx context.Context, spec *signing.Spec) (signing.Secrets, error) {
	var sec signing.Secrets
	if s.secrets == nil {
		return sec, ErrNoSecretResolver
	}
	var err error
	if spec.SecretRef != "" {
		if sec.HMACKey, err = s.secrets.ResolveSecret(ctx, spec.SecretRef); err != nil || sec.HMACKey == "" {
			return sec, fmt.Errorf("secret_ref %q could not be resolved", spec.SecretRef)
		}
	}
	if spec.KeyRef != "" {
		if sec.BearerToken, err = s.secrets.ResolveSecret(ctx, spec.KeyRef); err != nil || sec.BearerToken == "" {
			return sec, fmt.Errorf("key_ref %q could not be resolved", spec.KeyRef)
		}
	}
	return sec, nil
}

// safeResponseBody keeps what a target answered for the delivery log without
// persisting secrets it may echo: a JSON body goes through webhookout.RedactJSON
// (secret-keyed values replaced); anything else is cut to a short excerpt.
func safeResponseBody(b string) string {
	if b == "" {
		return ""
	}
	if r := webhookout.RedactJSON([]byte(b)); r != nil {
		return string(r)
	}
	return truncate(b, 256)
}
