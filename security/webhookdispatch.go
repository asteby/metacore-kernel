package security

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"time"

	"github.com/google/uuid"
)

// WebhookDispatcher sends outbound webhook calls to addons with an HMAC
// signature derived from the per-installation secret. The secret is resolved
// lazily through SignerLookup — callers typically read it from a secrets
// manager indexed by installation ID.
//
// The dispatcher never logs the raw secret or the signature's pre-image; only
// the generated signature header (which is safe to log: it depends on the
// secret but does not reveal it) and the outcome (status, duration, error).
type WebhookDispatcher struct {
	// HTTPClient executes the request. If nil, a default client using Timeout
	// is created on first use.
	HTTPClient *http.Client
	// Timeout bounds each outbound call. Default: 15s.
	Timeout time.Duration
	// SignerLookup resolves a *Signer for the given installation. Returning
	// (nil, nil) means "no known secret — send unsigned" (retro-compat path
	// used while we roll the feature out). Returning an error aborts the call.
	SignerLookup func(installationID uuid.UUID) (*Signer, error)
	// Logger receives dispatch outcomes. Defaults to the standard logger.
	Logger *log.Logger
}

// NewWebhookDispatcher returns a dispatcher with sensible defaults.
func NewWebhookDispatcher(lookup func(uuid.UUID) (*Signer, error)) *WebhookDispatcher {
	return &WebhookDispatcher{
		Timeout:      15 * time.Second,
		SignerLookup: lookup,
	}
}

// Dispatch sends method+body to url, adding HMAC signing headers when a
// signer is available for installationID. The caller owns closing the
// returned response body.
//
// Headers added when signed:
//
//	X-Metacore-Timestamp, X-Metacore-Signature (from Signer.Sign)
//	X-Metacore-Installation-ID: <uuid>
//	X-Metacore-Event:           <event> (taken from ctx key "metacore.event" if set)
func (d *WebhookDispatcher) Dispatch(ctx context.Context, installationID uuid.UUID, rawURL, method string, body []byte) (*http.Response, error) {
	if method == "" {
		method = http.MethodPost
	}
	client := d.HTTPClient
	timeout := d.Timeout
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	if client == nil {
		client = &http.Client{Timeout: timeout}
	}
	logger := d.Logger
	if logger == nil {
		logger = log.Default()
	}

	req, err := http.NewRequestWithContext(ctx, method, rawURL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("webhook dispatch: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Metacore-Installation-ID", installationID.String())
	if ev, ok := ctx.Value(eventKey{}).(string); ok && ev != "" {
		req.Header.Set("X-Metacore-Event", ev)
	}
	// Host-supplied context (X-Metacore-Host, X-Metacore-Tenant, and per-host
	// keys like X-Link-Phone or X-Ops-Branch-ID) lets the same addon backend
	// serve multiple hosts by reading whichever headers it understands.
	if hc, ok := ctx.Value(hostCtxKey{}).(map[string]string); ok {
		for k, v := range hc {
			if v == "" {
				continue
			}
			req.Header.Set(k, v)
		}
	}

	signed := false
	if d.SignerLookup != nil {
		signer, err := d.SignerLookup(installationID)
		if err != nil {
			return nil, fmt.Errorf("webhook dispatch: signer lookup: %w", err)
		}
		if signer != nil {
			path := signedPath(rawURL)
			// Per-call nonce binds each dispatch uniquely so a captured
			// request cannot be replayed within the timestamp skew window.
			// Receivers must plug a NonceCache to actually reject repeats.
			nonce := uuid.NewString()
			for k, v := range signer.SignWithNonce(method, path, body, nonce) {
				req.Header.Set(k, v)
			}
			signed = true
		}
	}

	start := time.Now()
	resp, err := client.Do(req)
	dur := time.Since(start)
	if err != nil {
		logger.Printf("metacore.webhook dispatch installation=%s url=%s signed=%t err=%v dur=%s",
			installationID, rawURL, signed, err, dur)
		return nil, err
	}
	logger.Printf("metacore.webhook dispatch installation=%s url=%s signed=%t status=%d dur=%s",
		installationID, rawURL, signed, resp.StatusCode, dur)
	return resp, nil
}

// DispatchAndRead is a convenience that reads+closes the body and returns
// (statusCode, responseBody, err). Use it when the caller does not need
// streaming semantics.
func (d *WebhookDispatcher) DispatchAndRead(ctx context.Context, installationID uuid.UUID, rawURL, method string, body []byte) (int, []byte, error) {
	resp, err := d.Dispatch(ctx, installationID, rawURL, method, body)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	b, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		return resp.StatusCode, nil, readErr
	}
	return resp.StatusCode, b, nil
}

// WithEvent returns a new context tagged with the event name — the dispatcher
// emits it as the X-Metacore-Event header. We use a private key type to avoid
// colliding with callers' context keys.
func WithEvent(ctx context.Context, event string) context.Context {
	return context.WithValue(ctx, eventKey{}, event)
}

type eventKey struct{}

// HostContext wraps the headers a host attaches to every outbound webhook so
// the addon backend can branch on source (link vs ops) without the addon
// having to hard-code host identifiers into its manifest.
type HostContext struct {
	Host    string            // "ops" | "link" | any host-defined ID
	Tenant  string            // organization UUID string
	Extras  map[string]string // any "X-<Host>-<Key>" headers
}

// WithHostContext tags ctx so the dispatcher emits:
//
//	X-Metacore-Host:   <hc.Host>
//	X-Metacore-Tenant: <hc.Tenant>
//	...plus every entry in hc.Extras verbatim (caller supplies the full header name).
//
// Hosts typically construct this once per request from their middleware.
func WithHostContext(ctx context.Context, hc HostContext) context.Context {
	m := map[string]string{}
	if hc.Host != "" {
		m["X-Metacore-Host"] = hc.Host
	}
	if hc.Tenant != "" {
		m["X-Metacore-Tenant"] = hc.Tenant
	}
	for k, v := range hc.Extras {
		m[k] = v
	}
	if len(m) == 0 {
		return ctx
	}
	return context.WithValue(ctx, hostCtxKey{}, m)
}

type hostCtxKey struct{}

// signedPath extracts the path (with query) used as part of the signed
// payload. The full URL would tie the signature to the host the host already
// knows; signing only the path matches how the Signer.Verify side reconstructs
// it on the receiving addon.
func signedPath(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	p := u.Path
	if p == "" {
		p = "/"
	}
	if u.RawQuery != "" {
		p += "?" + u.RawQuery
	}
	return p
}
