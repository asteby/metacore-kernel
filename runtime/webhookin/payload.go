package webhookin

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"mime"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
)

// DefaultHeaders is the documented allowlist of request headers (lowercase)
// forwarded to a webhook handler in the payload's `headers`. A trailing "*"
// matches a prefix. A route adds more through Route.Headers (manifest
// `webhooks[].headers`).
//
// "authorization" is forwarded ONLY when the route's Verify is "" or "addon"
// (the kernel did not consume it as a credential); on hmac-sha256/bearer
// routes it never leaves the kernel. The kernel never logs header values.
var DefaultHeaders = []string{
	"content-type",
	"user-agent",
	"x-request-id",
	"x-signature",
	"x-signature-256",
	"x-hub-signature-256",
	"stripe-signature",
	"paypal-transmission-id",
	"paypal-transmission-time",
	"paypal-transmission-sig",
	"paypal-cert-url",
	"paypal-auth-algo",
	"digest",
	"authorization",
	"x-kueski-*",
	"x-aplazo-*",
	"webhook-signature",
	"webhook-timestamp",
	"webhook-id",
}

// deniedHeaders never reach a handler, whatever a route's extension says.
var deniedHeaders = map[string]struct{}{
	"cookie":              {},
	"set-cookie":          {},
	"proxy-authorization": {},
}

// ResponseHeaders is the allowlist of response headers a handler may set in its
// response envelope (canonical spelling). Anything else is dropped, as is any
// value carrying CR/LF.
var ResponseHeaders = []string{
	"Content-Type",
	"Cache-Control",
	"Retry-After",
}

var responseHeaderIndex = func() map[string]string {
	m := make(map[string]string, len(ResponseHeaders))
	for _, h := range ResponseHeaders {
		m[strings.ToLower(h)] = h
	}
	return m
}()

// Payload keys handed to the handler. `org`, `webhook`, `path` and `body` are
// the original (v0) fields; the rest were added for provider-grade
// verification. `body` is a convenience copy that JSON serialization compacts
// and may re-escape — verify signatures over `raw_body_b64` only.
const (
	PayloadOrg        = "org"          // string, org UUID
	PayloadWebhook    = "webhook"      // string, route key
	PayloadPath       = "path"         // string, route path
	PayloadMethod     = "method"       // string, e.g. "POST"
	PayloadQuery      = "query"        // map[string][]string, never nil
	PayloadHeaders    = "headers"      // map[string]string, lowercase keys, allowlisted
	PayloadRawBodyB64 = "raw_body_b64" // string, base64 (std, padded) of the exact body
	PayloadBody       = "body"         // parsed JSON when valid, else string
	PayloadReceivedAt = "received_at"  // string, RFC 3339 (UTC, nanoseconds)
)

// now is the payload clock (swapped in tests).
var now = time.Now

// BuildPayload builds the map dispatched to a route's handler for a request.
// Exposed so hosts and tests can see exactly what a handler receives.
func BuildPayload(route Route, orgID uuid.UUID, in Inbound) map[string]any {
	method := strings.ToUpper(strings.TrimSpace(in.Method))
	if method == "" {
		method = http.MethodPost
	}
	query := map[string][]string{}
	for k, vs := range in.Query {
		query[k] = append([]string(nil), vs...)
	}
	return map[string]any{
		PayloadOrg:        orgID.String(),
		PayloadWebhook:    route.Key,
		PayloadPath:       in.Path,
		PayloadMethod:     method,
		PayloadQuery:      query,
		PayloadHeaders:    FilterHeaders(route, in.Headers),
		PayloadRawBodyB64: base64.StdEncoding.EncodeToString(in.Body),
		PayloadBody:       bodyValue(in.Body),
		PayloadReceivedAt: now().UTC().Format(time.RFC3339Nano),
	}
}

// FilterHeaders returns the request headers a route's handler may see:
// DefaultHeaders plus route.Headers, minus the deny list, minus Authorization
// when the kernel verified the request. Keys are lowercase; repeated values are
// joined with ", ".
func FilterHeaders(route Route, h http.Header) map[string]string {
	out := map[string]string{}
	allowAuth := route.Verify == VerifyNone || route.Verify == VerifyAddon
	for k, vs := range h {
		lk := strings.ToLower(strings.TrimSpace(k))
		if len(vs) == 0 || lk == "" {
			continue
		}
		if _, denied := deniedHeaders[lk]; denied {
			continue
		}
		if lk == "authorization" && !allowAuth {
			continue
		}
		if !headerAllowed(lk, DefaultHeaders) && !headerAllowed(lk, route.Headers) {
			continue
		}
		v := strings.Join(vs, ", ")
		if prev, dup := out[lk]; dup {
			v = prev + ", " + v
		}
		out[lk] = v
	}
	return out
}

func headerAllowed(name string, allow []string) bool {
	for _, a := range allow {
		a = strings.ToLower(strings.TrimSpace(a))
		if a == "" {
			continue
		}
		if p, ok := strings.CutSuffix(a, "*"); ok {
			if p != "" && strings.HasPrefix(name, p) {
				return true
			}
			continue
		}
		if a == name {
			return true
		}
	}
	return false
}

// EncodePayload serializes a payload for a handler backend (e.g. as the input
// of a WASM export) without HTML escaping, so `<`, `>` and `&` in `body` stay
// literal. Backends should prefer it over json.Marshal; the exact bytes are in
// `raw_body_b64` either way.
func EncodePayload(payload map[string]any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(payload); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

// Response is what a handler made of the request. Output is the handler's raw
// output (nil when the dispatcher cannot return one). When Output is a response
// envelope —
//
//	{"status": 200, "headers": {"Content-Type": "..."}, "body": ...}
//
// a JSON object with only those keys and an integer status in 100..599 —
// Envelope is true and the host should answer the provider with Status,
// Headers (already filtered to ResponseHeaders) and BodyBytes(). Otherwise the
// host keeps its default answer. Used for provider-mandated acks (Aplazo
// {"status":true}, Kueski's signed JWT, Openpay's verification capture) and
// for a guest-verified 401.
type Response struct {
	Envelope bool
	Status   int
	Headers  map[string]string
	Body     json.RawMessage
	Output   []byte
	// Mapped is true when the kernel, not the handler, chose Status: a
	// payments route answered {"success":false,...} (an addon's errEnvelope)
	// and the kernel turned it into 503. Cause holds the handler's error
	// text then.
	Mapped bool
	Cause  string
}

// BodyBytes returns the bytes to write on the wire: Body as-is for a JSON (or
// absent) Content-Type; for any other Content-Type a Body holding a JSON string
// is unquoted, so a handler can answer text/plain or a bare JWT.
func (r Response) BodyBytes() []byte {
	if len(r.Body) == 0 || string(r.Body) == "null" {
		return nil
	}
	if !isJSONMediaType(r.Headers["Content-Type"]) && r.Body[0] == '"' {
		var s string
		if err := json.Unmarshal(r.Body, &s); err == nil {
			return []byte(s)
		}
	}
	return r.Body
}

func isJSONMediaType(ct string) bool {
	if ct == "" {
		return true
	}
	mt, _, err := mime.ParseMediaType(ct)
	if err != nil {
		return false
	}
	return mt == "application/json" || strings.HasSuffix(mt, "+json")
}

// decodeResponse wraps a handler's output, recognising the response envelope.
func decodeResponse(out []byte) Response {
	resp := Response{Output: out}
	raw := bytes.TrimSpace(out)
	if len(raw) == 0 || raw[0] != '{' {
		return resp
	}
	var env struct {
		Status  *int              `json:"status"`
		Headers map[string]string `json:"headers"`
		Body    json.RawMessage   `json:"body"`
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&env); err != nil || dec.More() {
		return resp
	}
	if env.Status == nil || *env.Status < 100 || *env.Status > 599 {
		return resp
	}
	resp.Envelope = true
	resp.Status = *env.Status
	resp.Headers = sanitizeResponseHeaders(env.Headers)
	if len(env.Body) > 0 && string(env.Body) != "null" {
		resp.Body = env.Body
	}
	return resp
}

func sanitizeResponseHeaders(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		canon, ok := responseHeaderIndex[strings.ToLower(strings.TrimSpace(k))]
		if !ok || strings.ContainsAny(v, "\r\n") {
			continue
		}
		out[canon] = strings.TrimSpace(v)
	}
	return out
}

// mapFailureEnvelope is the safety net of a payments route: a handler output
// of {"success":false,...} that is not a response envelope would otherwise be
// answered 200 by the host, and the provider would never retry the delivery.
// It becomes a 503 envelope carrying the handler's error text.
func mapFailureEnvelope(resp Response) Response {
	if resp.Envelope {
		return resp
	}
	raw := bytes.TrimSpace(resp.Output)
	if len(raw) == 0 || raw[0] != '{' {
		return resp
	}
	var out struct {
		Success *bool           `json:"success"`
		Error   json.RawMessage `json:"error"`
		Message string          `json:"message"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || out.Success == nil || *out.Success {
		return resp
	}
	cause := out.Message
	if len(out.Error) > 0 {
		var s string
		if json.Unmarshal(out.Error, &s) == nil {
			cause = s
		} else {
			cause = string(out.Error)
		}
	}
	if len(cause) > 300 {
		cause = cause[:300]
	}
	body, _ := json.Marshal(map[string]any{"success": false, "error": "temporarily unavailable"})
	resp.Envelope, resp.Status, resp.Body = true, http.StatusServiceUnavailable, body
	resp.Headers = map[string]string{"Retry-After": "30"}
	resp.Mapped, resp.Cause = true, cause
	return resp
}
