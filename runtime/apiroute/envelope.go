package apiroute

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"mime"
	"net/http"
	"strings"

	"github.com/google/uuid"
)

// DefaultRequestHeaders is the allowlist of request headers forwarded to the
// handler (lowercase). Everything else — cookies, Authorization, internal
// proxy headers — never reaches the addon. Extend it with WithRequestHeaders.
//
// x-customer-token carries an end-customer session the addon that issued it
// validates itself; idempotency-key lets write handlers deduplicate retries.
var DefaultRequestHeaders = []string{
	"accept",
	"accept-language",
	"if-none-match",
	"content-type",
	"user-agent",
	"x-forwarded-for",
	"x-request-id",
	"x-customer-token",
	"idempotency-key",
}

// ResponseHeaders is the allowlist of response headers a handler may set, in
// their canonical spelling. Any other header is dropped.
var ResponseHeaders = []string{
	"Cache-Control",
	"ETag",
	"Vary",
	"Content-Type",
	"Location",
	"Content-Language",
	"Last-Modified",
	"Retry-After",
}

var responseHeaderIndex = func() map[string]string {
	m := make(map[string]string, len(ResponseHeaders))
	for _, h := range ResponseHeaders {
		m[strings.ToLower(h)] = h
	}
	return m
}()

// Request is the envelope the gateway hands the route handler (marshalled to
// JSON for a WASM export — use Encode, which never HTML-escapes).
type Request struct {
	// Addon is the key of the addon that owns the route.
	Addon string `json:"addon"`
	// Route is the route key within the addon.
	Route string `json:"route"`
	// Method is the request method (HEAD arrives as "HEAD" on GET routes).
	Method string `json:"method"`
	// Path is the request path relative to the gateway mount.
	Path string `json:"path"`
	// Params are the :param / *wildcard captures, unescaped.
	Params map[string]string `json:"params"`
	// Query is the parsed query string (repeated keys keep every value).
	Query map[string][]string `json:"query"`
	// Headers are the allowlisted request headers, lowercase keys, repeated
	// values joined with ", ".
	Headers map[string]string `json:"headers"`
	// Body is the request body: verbatim when it is valid JSON, otherwise a
	// JSON string holding it. Absent when the request has no body. A
	// convenience: JSON encoding compacts it, so anything that must see the
	// exact bytes (a signature, a hash) reads RawBody.
	Body json.RawMessage `json:"body,omitempty"`
	// RawBody is the exact request body, base64 (standard, padded). Absent
	// when the request has no body.
	RawBody string `json:"raw_body_b64,omitempty"`
	// Auth describes the principal.
	Auth RequestAuth `json:"auth"`
	// Org is the organization the request acts on.
	Org uuid.UUID `json:"org"`
	// Market is the organization's market the request acts in ("mx", "us");
	// absent = organization level. Handlers pass it on to capability
	// queries and use it to pick prices/currency.
	Market string `json:"market,omitempty"`
}

// Encode serializes the envelope for a handler backend without HTML escaping
// (json.Marshal would turn `<`, `>` and `&` in Body into \u003c-style
// escapes). Dispatchers that hand the envelope to a WASM export should use it
// instead of json.Marshal.
func (r Request) Encode() ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(r); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

// RequestAuth is the principal as the handler sees it.
type RequestAuth struct {
	Kind       AuthKind `json:"kind"`
	TokenID    string   `json:"token_id,omitempty"`
	Scopes     []string `json:"scopes,omitempty"`
	CustomerID string   `json:"customer_id,omitempty"`
	UserID     string   `json:"user_id,omitempty"`
}

// Response is the envelope the handler returns.
//
//	{"status": 200, "headers": {"Cache-Control": "..."}, "body": {...}}
//
// Status 0 means 200. Headers outside ResponseHeaders are dropped. Body is
// JSON; to answer with another media type (text/html, application/xml) set
// Content-Type and make Body a JSON string — the gateway writes its contents
// verbatim (see BodyBytes).
type Response struct {
	Status  int               `json:"status"`
	Headers map[string]string `json:"headers,omitempty"`
	Body    json.RawMessage   `json:"body,omitempty"`
}

// DecodeResponse parses the bytes a handler (typically a WASM export)
// returned. Empty output is 204 No Content; anything that is not a response
// envelope (unknown fields, trailing data, invalid JSON) is ErrBadResponse.
func DecodeResponse(raw []byte) (Response, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return Response{Status: http.StatusNoContent}, nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var r Response
	if err := dec.Decode(&r); err != nil {
		return Response{}, fmt.Errorf("%w: %v", ErrBadResponse, err)
	}
	if dec.More() {
		return Response{}, fmt.Errorf("%w: trailing data after the envelope", ErrBadResponse)
	}
	return r, nil
}

// Header returns the value of a response header, case-insensitively.
func (r Response) Header(name string) string {
	for k, v := range r.Headers {
		if strings.EqualFold(k, name) {
			return v
		}
	}
	return ""
}

// BodyBytes returns the bytes to write on the wire. For a JSON media type (or
// none) that is Body itself; for any other Content-Type a Body holding a JSON
// string is unquoted, so a handler can serve HTML, XML or plain text.
func (r Response) BodyBytes() []byte {
	if len(r.Body) == 0 {
		return nil
	}
	if !isJSONMediaType(r.Header("Content-Type")) && r.Body[0] == '"' {
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

// encodeBody turns a raw request body into the envelope's Body field.
func encodeBody(b []byte) json.RawMessage {
	if len(bytes.TrimSpace(b)) == 0 {
		return nil
	}
	if json.Valid(b) {
		return append(json.RawMessage(nil), b...)
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(string(b))
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n"))
}

// encodeRawBody is the envelope's RawBody: the exact bytes, base64.
func encodeRawBody(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	return base64.StdEncoding.EncodeToString(b)
}

// filterRequestHeaders keeps the allowlisted request headers.
func filterRequestHeaders(h http.Header, allow map[string]struct{}) map[string]string {
	out := map[string]string{}
	for k, vs := range h {
		lk := strings.ToLower(k)
		if _, ok := allow[lk]; !ok || len(vs) == 0 {
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

// sanitizeResponseHeaders keeps allowlisted headers under their canonical
// names and drops values carrying CR/LF (header injection).
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

// cacheControlFor derives Cache-Control from a route cache policy.
func cacheControlFor(c *Cache) string {
	if c.Private {
		return "private, no-store"
	}
	v := fmt.Sprintf("public, max-age=%d", c.MaxAge)
	if c.SWR > 0 {
		v += fmt.Sprintf(", stale-while-revalidate=%d", c.SWR)
	}
	return v
}

// weakETag computes W/"<128-bit sha256 prefix, hex>" over the body.
func weakETag(body []byte) string {
	sum := sha256.Sum256(body)
	return `W/"` + hex.EncodeToString(sum[:16]) + `"`
}

// etagMatches implements the weak comparison of If-None-Match (RFC 9110
// §13.1.2): "*" matches any current representation; otherwise any listed tag
// whose opaque part equals etag's.
func etagMatches(ifNoneMatch, etag string) bool {
	if ifNoneMatch == "" || etag == "" {
		return false
	}
	want := strings.TrimPrefix(strings.TrimSpace(etag), "W/")
	for _, t := range strings.Split(ifNoneMatch, ",") {
		t = strings.TrimSpace(t)
		if t == "*" || strings.TrimPrefix(t, "W/") == want {
			return true
		}
	}
	return false
}
