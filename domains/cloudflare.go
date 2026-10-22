package domains

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// CloudflareAPI is the production base URL.
const CloudflareAPI = "https://api.cloudflare.com/client/v4"

// Cloudflare is the ZoneProvider over the Cloudflare v4 API.
//
// Minimal token: an API token scoped to "Zone:Read" and "DNS:Edit" on the
// tenant's zone(s). Nothing else is used; there is no global key support and
// no delete call exists in this type. Records are always created DNS-only
// (proxied=false): mail hosts must never be proxied.
type Cloudflare struct {
	token string
	base  string
	hc    *http.Client
}

// CloudflareOption customizes NewCloudflare.
type CloudflareOption func(*Cloudflare)

// The API base URL is fixed to CloudflareAPI in production code. There is
// deliberately no exported option to change it: a host that let a tenant or an
// addon choose it would send the API token to an attacker-controlled server.
// Tests redirect the client with the unexported withCloudflareBaseURL
// (cloudflare_testhook_test.go) or WithCloudflareHTTPClient.

// WithCloudflareHTTPClient replaces the HTTP client.
func WithCloudflareHTTPClient(hc *http.Client) CloudflareOption {
	return func(c *Cloudflare) { c.hc = hc }
}

// NewCloudflare builds a provider from an API token.
func NewCloudflare(token string, opts ...CloudflareOption) *Cloudflare {
	c := &Cloudflare{token: strings.TrimSpace(token), base: CloudflareAPI, hc: &http.Client{Timeout: 20 * time.Second}}
	for _, o := range opts {
		o(c)
	}
	return c
}

func (c *Cloudflare) Name() string { return ProviderCloudflare }

// String never prints the token.
func (c *Cloudflare) String() string { return "cloudflare(token=***)" }

// GoString keeps %#v from leaking the token.
func (c *Cloudflare) GoString() string { return c.String() }

type cfEnvelope struct {
	Success    bool            `json:"success"`
	Errors     []cfError       `json:"errors"`
	Result     json.RawMessage `json:"result"`
	ResultInfo struct {
		TotalPages int `json:"total_pages"`
	} `json:"result_info"`
}

type cfError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type cfRecord struct {
	ID       string   `json:"id,omitempty"`
	Type     string   `json:"type"`
	Name     string   `json:"name"`
	Content  string   `json:"content"`
	TTL      int      `json:"ttl"`
	Priority *int     `json:"priority,omitempty"`
	Proxied  *bool    `json:"proxied,omitempty"`
	Comment  string   `json:"comment,omitempty"`
	Tags     []string `json:"tags,omitempty"`
}

func (c *Cloudflare) do(ctx context.Context, method, path string, q url.Values, body any, out *cfEnvelope) error {
	if c.token == "" {
		return fmt.Errorf("%w: empty token", ErrProviderAuth)
	}
	u := c.base + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		// The URL carries no secret; the token travels in a header only.
		return fmt.Errorf("cloudflare: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return fmt.Errorf("cloudflare: reading response: %w", err)
	}
	_ = json.Unmarshal(raw, out)
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return fmt.Errorf("%w: %s", ErrProviderAuth, cfMessage(out, resp.StatusCode))
	case resp.StatusCode == http.StatusTooManyRequests:
		return fmt.Errorf("%w: retry after %ss", ErrRateLimited, resp.Header.Get("Retry-After"))
	case resp.StatusCode >= 300 || !out.Success:
		return fmt.Errorf("cloudflare: %s %s: %s", method, path, cfMessage(out, resp.StatusCode))
	}
	return nil
}

func cfMessage(e *cfEnvelope, status int) string {
	var parts []string
	for _, x := range e.Errors {
		parts = append(parts, fmt.Sprintf("%d %s", x.Code, x.Message))
	}
	if len(parts) == 0 {
		return "HTTP " + strconv.Itoa(status)
	}
	return strings.Join(parts, "; ")
}

// Verify checks the token is active (GET /user/tokens/verify).
func (c *Cloudflare) Verify(ctx context.Context) error {
	var env cfEnvelope
	if err := c.do(ctx, http.MethodGet, "/user/tokens/verify", nil, nil, &env); err != nil {
		return err
	}
	var r struct {
		Status string `json:"status"`
	}
	if json.Unmarshal(env.Result, &r) != nil || r.Status != "active" {
		return fmt.Errorf("%w: token status %q", ErrProviderAuth, r.Status)
	}
	return nil
}

// FindZone tries host and its parents, most specific first.
func (c *Cloudflare) FindZone(ctx context.Context, host string) (Zone, error) {
	for _, cand := range ParentZones(strings.ToLower(host)) {
		var env cfEnvelope
		if err := c.do(ctx, http.MethodGet, "/zones", url.Values{"name": {cand}, "status": {"active"}}, nil, &env); err != nil {
			return Zone{}, err
		}
		var zs []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		}
		if err := json.Unmarshal(env.Result, &zs); err != nil {
			return Zone{}, fmt.Errorf("cloudflare: decoding zones: %w", err)
		}
		for _, z := range zs {
			if strings.EqualFold(z.Name, cand) {
				return Zone{ID: z.ID, Name: strings.ToLower(z.Name)}, nil
			}
		}
	}
	return Zone{}, fmt.Errorf("%w: %s", ErrZoneNotFound, host)
}

// ListRecords returns every record at the exact name.
func (c *Cloudflare) ListRecords(ctx context.Context, z Zone, name string) ([]ProviderRecord, error) {
	var out []ProviderRecord
	for page := 1; page <= 10; page++ {
		var env cfEnvelope
		q := url.Values{"name": {name}, "per_page": {"100"}, "page": {strconv.Itoa(page)}}
		if err := c.do(ctx, http.MethodGet, "/zones/"+url.PathEscape(z.ID)+"/dns_records", q, nil, &env); err != nil {
			return nil, err
		}
		var rs []cfRecord
		if err := json.Unmarshal(env.Result, &rs); err != nil {
			return nil, fmt.Errorf("cloudflare: decoding records: %w", err)
		}
		for _, r := range rs {
			out = append(out, fromCF(r))
		}
		if page >= env.ResultInfo.TotalPages {
			break
		}
	}
	return out, nil
}

func fromCF(r cfRecord) ProviderRecord {
	p := ProviderRecord{ID: r.ID, Type: RecordType(r.Type), Name: strings.ToLower(r.Name), Value: r.Content, TTL: r.TTL}
	if r.Priority != nil {
		p.Priority = *r.Priority
	}
	if p.Type == TypeTXT {
		p.Value = canonTXT(p.Value)
	}
	p.Comment, p.Tags, p.Proxied = r.Comment, r.Tags, r.Proxied
	return p
}

func toCF(r Record) cfRecord {
	ttl := r.TTL
	if ttl == 0 {
		ttl = 1 // Cloudflare "automatic"
	}
	out := cfRecord{Type: string(r.Type), Name: r.Name, TTL: ttl}
	switch r.Type {
	case TypeMX:
		out.Content = canonHost(r.Value)
		pr := r.Priority
		out.Priority = &pr
	case TypeCNAME:
		out.Content = canonHost(r.Value)
	case TypeTXT:
		out.Content = canonTXT(r.Value)
	default:
		out.Content = strings.TrimSpace(r.Value)
	}
	return out
}

// toCFCreate is toCF plus proxied=false on address/CNAME records: NEW mail and
// auth hosts are never proxied. Updates (toCF alone) leave proxied untouched.
func toCFCreate(r Record) cfRecord {
	out := toCF(r)
	if r.Type == TypeA || r.Type == TypeAAAA || r.Type == TypeCNAME {
		f := false
		out.Proxied = &f
	}
	return out
}

func (c *Cloudflare) CreateRecord(ctx context.Context, z Zone, r Record) (ProviderRecord, error) {
	var env cfEnvelope
	if err := c.do(ctx, http.MethodPost, "/zones/"+url.PathEscape(z.ID)+"/dns_records", nil, toCFCreate(r), &env); err != nil {
		return ProviderRecord{}, err
	}
	return decodeCFRecord(env)
}

// UpdateRecord PATCHes only the fields the applier manages (type, name,
// content, ttl, priority). comment, tags and proxied are never sent, so the
// tenant's annotations survive and a proxied record stays as it was (the plan
// surfaces it as a warning); a PUT would reset them.
func (c *Cloudflare) UpdateRecord(ctx context.Context, z Zone, id string, r Record) (ProviderRecord, error) {
	if id == "" {
		return ProviderRecord{}, errors.New("cloudflare: record id is required")
	}
	var env cfEnvelope
	if err := c.do(ctx, http.MethodPatch, "/zones/"+url.PathEscape(z.ID)+"/dns_records/"+url.PathEscape(id), nil, toCF(r), &env); err != nil {
		return ProviderRecord{}, err
	}
	return decodeCFRecord(env)
}

func decodeCFRecord(env cfEnvelope) (ProviderRecord, error) {
	var r cfRecord
	if err := json.Unmarshal(env.Result, &r); err != nil {
		return ProviderRecord{}, fmt.Errorf("cloudflare: decoding record: %w", err)
	}
	return fromCF(r), nil
}

var _ ZoneProvider = (*Cloudflare)(nil)
