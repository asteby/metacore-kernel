package webhooks

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
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
// (a receiver rejects a repeated nonce). Secrets are resolved by reference, FOR
// THE SUBSCRIPTION'S OWNER, on each attempt (rotation-friendly) and never
// stored or logged. The target is re-validated before every send and the
// delivery uses the protected egress client, never a host-supplied one.
func (s *Service) attemptDeclarative(ctx context.Context, w *Webhook, d *WebhookDelivery) {
	dec := w.Declarative
	d.AttemptCount++
	d.InFlight = false
	// None of these is the destination's fault: they are not health failures.
	cfgFail := failOpts{permanent: true, noHealth: true}

	if d.PayloadPurged {
		s.persistFailure(ctx, w, d, 0, "payload was purged; nothing to send", cfgFail)
		return
	}
	if err := s.validateTarget(w.URL); err != nil {
		s.persistFailure(ctx, w, d, 0, "target no longer allowed: "+err.Error(), cfgFail)
		return
	}
	body, err := json.Marshal(d.Payload)
	if err != nil {
		s.persistFailure(ctx, w, d, 0, "marshal body: "+err.Error(), cfgFail)
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
		s.persistFailure(ctx, w, d, 0, "new request: "+errText(err), cfgFail)
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
		if !validIdempotencyKey(d.IdempotencyKey) {
			s.persistFailure(ctx, w, d, 0, "invalid idempotency key", cfgFail)
			return
		}
		req.Header.Set(dec.idemHeader(), d.IdempotencyKey)
	}

	now := s.clock()
	if dec.Signing != nil {
		sec, err := s.resolveSecrets(ctx, w, dec.Signing)
		if err != nil {
			// Retryable (the secret may be provisioned/rotated meanwhile), but
			// not the destination's fault.
			s.persistFailure(ctx, w, d, 0, err.Error(), failOpts{noHealth: true})
			return
		}
		hdrs, err := signing.Sign(*dec.Signing, sec, signing.Request{
			Method: req.Method, Path: signing.PathOf(req.URL), Body: body,
		}, now, "")
		if err != nil {
			s.persistFailure(ctx, w, d, 0, "sign: "+err.Error(), cfgFail)
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
		s.persistFailure(ctx, w, d, 0, errText(err), failOpts{})
		return
	}
	defer resp.Body.Close()
	status := resp.StatusCode
	respBody := safeResponseBody(readLimited(resp.Body, 8*1024))

	if status >= 200 && status < 300 {
		s.persistSuccess(ctx, w, d, status, respBody, true)
		return
	}
	s.persistFailure(ctx, w, d, status, respBody, classifyStatus(status))
}

// errText is an HTTP client error without the URL (it may carry a query or a
// path with identifiers): only the underlying cause.
func errText(err error) string {
	var ue *url.Error
	if errors.As(err, &ue) && ue.Err != nil {
		return ue.Err.Error()
	}
	return err.Error()
}

// classifyStatus: a 4xx means the request itself is wrong, so retrying the same
// bytes cannot help (permanent, and not a health failure of the destination) -
// except the transient ones: 408 and 429 retry AND count as health failures;
// 409 (in progress), 423 (paused) and 425 retry without counting. 5xx retries
// and counts.
func classifyStatus(status int) failOpts {
	switch {
	case status >= 500, status == 408, status == 429:
		return failOpts{}
	case status == 409, status == 423, status == 425:
		return failOpts{noHealth: true}
	case status >= 400 && status < 500:
		return failOpts{permanent: true, noHealth: true}
	}
	// 1xx / 3xx are never followed: the destination rejected the request.
	return failOpts{permanent: true}
}

func (s *Service) resolveSecrets(ctx context.Context, w *Webhook, spec *signing.Spec) (signing.Secrets, error) {
	var sec signing.Secrets
	if s.secrets == nil {
		return sec, ErrNoSecretResolver
	}
	var err error
	if spec.SecretRef != "" {
		if sec.HMACKey, err = s.secrets.ResolveSecret(ctx, w.OwnerType, w.OwnerID, spec.SecretRef); err != nil || sec.HMACKey == "" {
			return sec, fmt.Errorf("secret_ref %q could not be resolved", spec.SecretRef)
		}
	}
	if spec.KeyRef != "" {
		if sec.BearerToken, err = s.secrets.ResolveSecret(ctx, w.OwnerType, w.OwnerID, spec.KeyRef); err != nil || sec.BearerToken == "" {
			return sec, fmt.Errorf("key_ref %q could not be resolved", spec.KeyRef)
		}
	}
	return sec, nil
}

// safeResponseBody keeps what a target answered for the delivery log without
// persisting secrets it may echo: a JSON body goes through webhookout.RedactJSON
// (secret-keyed values replaced); anything else is cut to a short excerpt. The
// result is always valid UTF-8 without NUL.
func safeResponseBody(b string) string {
	if b == "" {
		return ""
	}
	b = cleanText(b, 8*1024)
	if r := webhookout.RedactJSON([]byte(b)); r != nil {
		return cleanText(string(r), 8*1024)
	}
	return cleanText(b, 256)
}
