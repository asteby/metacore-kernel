// Package webhookout is the outgoing half of RFC-01 phase 2: it turns a
// handler.type "webhook" action into ONE signed HTTP request and maps the
// answer to a result or an error. It is host-neutral: the host resolves the
// base URL, supplies the instance Signer and the egress Policy, applies the
// action's permissions, and decides what to do with the Result.
//
// Guarantees:
//   - Fail-closed: any error, timeout, redirect or non-2xx is an *ActionError.
//     A success is returned only for a real 2xx from the destination.
//   - Never unsigned: no Signer, an unknown scheme or a signing failure sends
//     nothing.
//   - Egress: https only, public destinations only, unless the host names an
//     internal destination in Policy.AllowInternal. Redirects are not followed.
//   - Secrets: nothing is logged here; the returned body has bearer values
//     (license_key, license_token, tokens, secrets...) replaced by "[redacted]".
//
// The signature scheme (asteby-req-v1) is documented in
// docs/spec/v3/webhook-signing.md.
package webhookout

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"

	v3 "github.com/asteby/metacore-kernel/manifest/v3"
)

const (
	// DefaultMaxResponseBytes is how much of the answer is read (RFC-01 §2.3).
	DefaultMaxResponseBytes = 64 << 10
	// MaxRequestBytes bounds the body we send (the receiver rejects > 1 MiB).
	MaxRequestBytes = 1 << 20

	defaultTimeout = 10 * time.Second
	userAgent      = "metacore-webhookout/1"
	maxMessage     = 300
)

// Config configures a Client. The host owns every field.
type Config struct {
	// Signer is the instance identity. Required: there is no unsigned mode.
	Signer Signer
	// Policy is the egress policy. The zero value denies internal destinations.
	Policy Policy
	// MaxResponseBytes caps what is read from the answer. 0 = 64 KiB.
	MaxResponseBytes int64
	// ExtraSecretKeys adds JSON keys to redact in the returned body.
	ExtraSecretKeys []string
	// RequireEnvelope makes a 2xx count as success only if the body is a JSON
	// object with "success": true (the hub envelope). Anything else (HTML from
	// a proxy, empty body, {"success":false}, truncated body) is returned as
	// CodeBadResponse. Hosts that talk to the hub should set it.
	RequireEnvelope bool
	// Now is the clock (tests). nil = time.Now.
	Now func() time.Time
}

// Client sends signed action requests. It is safe for concurrent use.
type Client struct {
	cfg     Config
	http    *http.Client
	secrets map[string]struct{}
}

// New builds a Client. A nil Signer is accepted so that the misconfiguration
// is reported per invocation as an ActionError (ErrNoSigner) instead of
// crashing the host at boot; nothing is ever sent without it.
func New(cfg Config) *Client {
	if cfg.MaxResponseBytes <= 0 {
		cfg.MaxResponseBytes = DefaultMaxResponseBytes
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Client{cfg: cfg, http: cfg.Policy.newHTTPClient(), secrets: secretSet(cfg.ExtraSecretKeys)}
}

// Request is one invocation of an action.
type Request struct {
	// Handler is the action's handler (Type "webhook"). Path, Method, Signing,
	// TimeoutMS and Retries are read from it.
	Handler v3.Handler
	// BaseURL is the destination base resolved BY THE HOST from
	// handler.url_setting / handler.url_ref: scheme://host[:port] with no
	// userinfo, path, query or fragment. The path comes from Handler.Path.
	BaseURL string
	// Record is the acted-on row (placeholders {record.col}, confirm_typed).
	Record map[string]any
	// Payload is the action's field payload. It is sent AS the JSON body, flat
	// (the receiver rejects unknown fields). confirm_text is removed.
	Payload map[string]any
	// Actor is the staff e-mail (X-Actor, signed).
	Actor string
	// ConfirmTyped, when the action declares it, is enforced before sending.
	ConfirmTyped *v3.ConfirmTyped

	// Idempotency identity of the invocation. InvocationID must be stable for
	// one logical user action (the request id the client generated, or the
	// stored delivery id) so a re-submit reuses the same Idempotency-Key; it is
	// the same on every retry. REQUIRED: an empty value is rejected as
	// webhook_misconfigured before anything is sent.
	AddonKey, OrgID, Model, Action, RecordID, InvocationID string
}

// Result is a successful (2xx) answer.
type Result struct {
	Status int
	// Body is the answer re-encoded with secrets redacted (nil if it was not
	// JSON or exceeded the read limit). Data is its decoded form.
	Body      []byte
	Data      any
	Truncated bool
	// Message is the destination's own short message, if any.
	Message string
	// IdempotencyKey was sent in the Idempotency-Key header.
	IdempotencyKey string
	Attempts       int
	Duration       time.Duration
}

// DeriveIdempotencyKey builds the stable Idempotency-Key for one invocation:
// "mc1-" + hex(sha256) of the length-prefixed parts. 68 characters of
// [a-z0-9-], inside the receiver's grammar [A-Za-z0-9._:-]{1,128}.
func DeriveIdempotencyKey(addon, org, model, action, recordID, invocationID string) string {
	h := sha256.New()
	for _, p := range []string{addon, org, model, action, recordID, invocationID} {
		fmt.Fprintf(h, "%d:%s|", len(p), p)
	}
	return "mc1-" + hex.EncodeToString(h.Sum(nil))
}

// Do performs the invocation. It returns a Result only for a real 2xx.
func (c *Client) Do(ctx context.Context, r Request) (*Result, error) {
	start := c.cfg.Now()
	if err := VerifyConfirmTyped(r.ConfirmTyped, r.Record, r.Payload); err != nil {
		return nil, &ActionError{Code: CodeConfirmMismatch, Message: confirmMessage(err), cause: err}
	}
	h := r.Handler
	if h.Type != "webhook" {
		return nil, misconfigured(nil, "handler.type must be webhook")
	}
	if c.cfg.Signer == nil {
		return nil, misconfigured(ErrNoSigner, "the instance signing key is not available; nothing was sent")
	}
	if h.Signing == nil || h.Signing.Scheme != SchemeManifest {
		return nil, misconfigured(nil, "handler.signing.scheme must be %s; unsigned requests are never sent", SchemeManifest)
	}
	method := strings.ToUpper(h.Method)
	if method == "" {
		method = http.MethodPost
	}
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
	default:
		return nil, misconfigured(nil, "unsupported method %q", h.Method)
	}
	budget := defaultTimeout
	if h.TimeoutMS != 0 {
		if h.TimeoutMS < 1000 || h.TimeoutMS > 30000 {
			return nil, misconfigured(nil, "timeout_ms must be within 1000..30000")
		}
		budget = time.Duration(h.TimeoutMS) * time.Millisecond
	}
	retries := 2
	if h.Retries != nil {
		retries = *h.Retries
	}
	if retries < 0 || retries > 3 {
		return nil, misconfigured(nil, "retries must be within 0..3")
	}
	if !validActor(r.Actor) {
		return nil, misconfigured(nil, "the acting user has no valid e-mail for X-Actor")
	}

	target, aerr := c.target(r)
	if aerr != nil {
		return nil, aerr
	}
	body, err := encodeBody(r.Payload)
	if err != nil {
		return nil, misconfigured(err, "the payload cannot be encoded as a flat JSON object")
	}

	// A random key would make a re-submit after a timeout or a double click a
	// NEW write at the destination (idempotency fail-open), so it is required.
	invocation := strings.TrimSpace(r.InvocationID)
	if invocation == "" {
		return nil, misconfigured(nil, "InvocationID is required: it must be stable per user action so a re-submit reuses the Idempotency-Key; nothing was sent")
	}
	idem := DeriveIdempotencyKey(r.AddonKey, r.OrgID, r.Model, r.Action, r.RecordID, invocation)

	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()

	var last *ActionError
	for attempt := 1; attempt <= retries+1; attempt++ {
		if attempt > 1 {
			wait := 250 * time.Millisecond
			if attempt > 2 {
				wait = time.Second
			}
			select {
			case <-ctx.Done():
				return nil, c.finish(last, ctx.Err(), attempt-1)
			case <-time.After(wait):
			}
		}
		res, aerr := c.once(ctx, method, target, idem, r.Actor, body)
		if aerr == nil {
			res.IdempotencyKey, res.Attempts, res.Duration = idem, attempt, c.cfg.Now().Sub(start)
			return res, nil
		}
		aerr.Attempts = attempt
		last = aerr
		if aerr.Code != CodeUnavailable || ctx.Err() != nil {
			break
		}
	}
	if last.Code == CodeUnavailable && ctx.Err() != nil {
		return nil, c.finish(last, ctx.Err(), last.Attempts)
	}
	return nil, last
}

func (c *Client) finish(last *ActionError, ctxErr error, attempts int) *ActionError {
	if errors.Is(ctxErr, context.DeadlineExceeded) {
		return &ActionError{Code: CodeTimeout, Message: "the destination did not answer within the time budget", Attempts: attempts, cause: ctxErr}
	}
	if last != nil {
		return last
	}
	return &ActionError{Code: CodeUnavailable, Message: "the request was cancelled", Attempts: attempts, cause: ctxErr}
}

type destination struct {
	u    *url.URL
	path string // decoded path, as the receiver sees it (what is signed)
	raw  string
}

func (c *Client) target(r Request) (*destination, *ActionError) {
	base, err := url.Parse(strings.TrimSpace(r.BaseURL))
	if err != nil || base.Host == "" || base.User != nil || base.RawQuery != "" || base.Fragment != "" ||
		base.ForceQuery || (base.Path != "" && base.Path != "/") || base.Opaque != "" {
		return nil, misconfigured(err, "the base URL must be scheme://host[:port] with no credentials, path, query or fragment")
	}
	if r.Handler.Path == "" {
		return nil, misconfigured(nil, "handler.path is required")
	}
	p, err := ExpandPath(r.Handler.Path, r.Record, r.Payload)
	if err != nil {
		return nil, misconfigured(err, "handler.path: %v", err)
	}
	full, err := url.Parse(base.Scheme + "://" + base.Host + p)
	if err != nil {
		return nil, misconfigured(err, "the destination URL is not valid")
	}
	if err := c.cfg.Policy.check(full); err != nil {
		return nil, misconfigured(err, "destination not allowed by the egress policy: %v", err)
	}
	return &destination{u: full, path: full.Path, raw: full.RawQuery}, nil
}

// encodeBody renders the payload as a flat JSON object with no HTML escaping.
// confirm_text is the user's confirmation, not data for the destination.
func encodeBody(payload map[string]any) ([]byte, error) {
	out := make(map[string]any, len(payload))
	for k, v := range payload {
		if k != ConfirmTextKey {
			out[k] = v
		}
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(out); err != nil {
		return nil, err
	}
	b := bytes.TrimRight(buf.Bytes(), "\n")
	if len(b) > MaxRequestBytes {
		return nil, errors.New("body exceeds 1 MiB")
	}
	return b, nil
}

func (c *Client) once(ctx context.Context, method string, d *destination, idem, actor string, body []byte) (*Result, *ActionError) {
	sig, ts, err := SignatureHeaders(c.cfg.Signer, method, d.path, d.raw, idem, actor, body, c.cfg.Now())
	if err != nil {
		return nil, misconfigured(err, "the request could not be signed; nothing was sent")
	}
	req, err := http.NewRequestWithContext(ctx, method, d.u.String(), bytes.NewReader(body))
	if err != nil {
		return nil, misconfigured(err, "the request could not be built")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set(HeaderScheme, Scheme)
	req.Header.Set(HeaderSignature, sig)
	req.Header.Set(HeaderTimestamp, ts)
	req.Header.Set(HeaderPubKey, strings.ToLower(c.cfg.Signer.PublicKeyHex()))
	req.Header.Set(HeaderIdempotencyKey, idem)
	req.Header.Set(HeaderActor, actor)

	resp, err := c.http.Do(req)
	if err != nil {
		if errors.Is(err, ErrDestinationBlocked) || strings.Contains(err.Error(), "netguard:") {
			return nil, misconfigured(ErrDestinationBlocked, "destination not allowed by the egress policy")
		}
		if ctx.Err() != nil {
			return nil, &ActionError{Code: CodeTimeout, Message: "the destination did not answer within the time budget", cause: err}
		}
		return nil, &ActionError{Code: CodeUnavailable, Message: "the destination could not be reached", cause: err}
	}
	defer resp.Body.Close()
	raw, rerr := io.ReadAll(io.LimitReader(resp.Body, c.cfg.MaxResponseBytes+1))
	truncated := int64(len(raw)) > c.cfg.MaxResponseBytes
	if truncated {
		raw = raw[:c.cfg.MaxResponseBytes]
	}

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		msg := remoteMessage(raw, resp.StatusCode)
		code := CodeRejected
		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
			code = CodeUnavailable
			msg = "the destination service is unavailable, try again"
		}
		return nil, &ActionError{Code: code, Message: msg, Status: resp.StatusCode}
	}
	if rerr != nil {
		// A 2xx whose body could not be read in full: the effect may have been
		// applied, so report it as ambiguous instead of as a clean success.
		return nil, &ActionError{Code: CodeTimeout, Message: "the answer was cut off; check the result before retrying", Status: resp.StatusCode, cause: rerr}
	}

	if c.cfg.RequireEnvelope && (truncated || !envelopeOK(raw)) {
		return nil, &ActionError{Code: CodeBadResponse, Message: "the destination answered 2xx but not with a verifiable success envelope; check the result before retrying", Status: resp.StatusCode}
	}

	res := &Result{Status: resp.StatusCode, Truncated: truncated}
	if !truncated {
		res.Data, res.Body = redactJSON(raw, c.secrets)
		if m, ok := res.Data.(map[string]any); ok {
			if s, ok := m["message"].(string); ok {
				res.Message = clip(s)
			}
		}
	}
	return res, nil
}

// envelopeOK reports whether raw is a JSON object with "success": true.
func envelopeOK(raw []byte) bool {
	var v struct {
		Success *bool `json:"success"`
	}
	return json.Unmarshal(raw, &v) == nil && v.Success != nil && *v.Success
}

// remoteMessage extracts the destination's short error text from
// {"error":{"message"}} or {"message"}; never headers, never the raw body.
func remoteMessage(raw []byte, status int) string {
	var v struct {
		Error   json.RawMessage `json:"error"`
		Message string          `json:"message"`
	}
	if json.Unmarshal(raw, &v) == nil {
		var e struct {
			Message string `json:"message"`
		}
		if len(v.Error) > 0 && json.Unmarshal(v.Error, &e) == nil && e.Message != "" {
			return clip(e.Message)
		}
		if v.Message != "" {
			return clip(v.Message)
		}
	}
	return "the destination rejected the request (" + strconv.Itoa(status) + ")"
}

func clip(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
	if r := []rune(s); len(r) > maxMessage {
		return string(r[:maxMessage])
	}
	return s
}

func confirmMessage(err error) string {
	return strings.TrimPrefix(err.Error(), ErrConfirmMismatch.Error()+": ")
}
