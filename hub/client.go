// Package hub is the HTTP client for the metacore Hub — the upstream addon
// catalog + bundle distribution service that any kernel consumer (ops, link,
// custom apps embedding the kernel) talks to when an admin browses the
// addon marketplace UI.
//
// The Hub contract (Phase 1 audited surface):
//
//	GET  {base}/v1/catalog                              → CatalogResponse
//	GET  {base}/v1/addons/{key}                         → AddonDetail (with versions)
//	GET  {base}/v1/addons/{key}/bundle?version={ver}    → application/gzip (.tar.gz)
//	GET  {base}/v1/spec                                 → Spec (kernel/protocol compat hints)
//
// Bearer authentication is optional; private catalogs (e.g. customer-pinned
// pre-release channels) require a license token. The public catalog stays
// open so onboarding can list free addons before the license is even issued.
//
// Why this lives in the kernel:
//
//   - The kernel already owns the *install side* of the hub flow:
//     `bundle.Read` parses the .tar.gz, `installer.Installer.Install` runs
//     migrations + lifecycle hooks + signature/checksum verification, and
//     `marketplace.Handler` exposes the embedded install HTTP surface.
//   - Adding the *catalog side* here closes the loop: every kernel consumer
//     gets browse → fetch → install with one import path, instead of each
//     app rewriting (and drifting on) its own hub client.
//   - The package intentionally has zero kernel-internal dependencies so
//     non-kernel Go services (Hub admin tooling, CI bots) can import it
//     standalone too.
//
// Configuration is environment-driven so apps don't need to thread URLs
// through their config layer:
//
//	HUB_BASE_URL        base URL (default https://hub.asteby.com)
//	HUB_LICENSE_TOKEN   optional Bearer token sent on every request
package hub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	// defaultBaseURL is the public marketplace hub used when HUB_BASE_URL is
	// unset. It must be a LIVE host: a vanilla install relies on this default to
	// reach the catalog/bundle API, so a stale placeholder silently broke every
	// install/upgrade/resync with "no such host". hub.asteby.com is the
	// production hub; air-gapped deployments override via HUB_BASE_URL.
	defaultBaseURL = "https://hub.asteby.com"
	defaultTimeout = 60 * time.Second
	// userAgent identifies clients to the Hub. We deliberately use a
	// kernel-scoped UA so server-side logs/metrics show the kernel version
	// every consumer ships with — the app name (ops, link, …) does NOT
	// matter for the Hub contract, only the kernel HTTP surface does.
	userAgent = "metacore-kernel-hub/1"
)

// ErrSpecUnavailable is returned by FetchSpec when the Hub does not (yet)
// expose `/v1/spec`. Callers treat this as "no compat info available" and
// continue — the endpoint is being rolled out in a separate PR upstream.
var ErrSpecUnavailable = errors.New("hub: /v1/spec not available")

// ErrNotFound is returned by FetchAddon / DownloadBundle when the Hub
// responds with 404. Sentinel error so handlers can map it to a 404 in their
// own response without re-parsing the wrapped status string.
var ErrNotFound = errors.New("hub: not found")

// Client talks to a metacore Hub over HTTPS.
type Client struct {
	baseURL      string
	licenseToken string
	httpClient   *http.Client
}

// Option configures a Client at construction time. Kept as functional
// options so future knobs (custom transport, retry policy) don't break the
// callsite.
type Option func(*Client)

// WithHTTPClient swaps the underlying *http.Client. Tests pass httptest's
// transport this way; production callers rarely need it.
func WithHTTPClient(h *http.Client) Option {
	return func(c *Client) {
		if h != nil {
			c.httpClient = h
		}
	}
}

// WithTimeout overrides the default 60s request timeout.
func WithTimeout(d time.Duration) Option {
	return func(c *Client) {
		if d > 0 {
			c.httpClient.Timeout = d
		}
	}
}

// NewClient builds a hub Client. baseURL must be non-empty and a syntactically
// valid URL.
func NewClient(baseURL, licenseToken string, opts ...Option) (*Client, error) {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		return nil, fmt.Errorf("hub: base URL is empty")
	}
	if _, err := url.Parse(baseURL); err != nil {
		return nil, fmt.Errorf("hub: invalid base URL: %w", err)
	}
	c := &Client{
		baseURL:      baseURL,
		licenseToken: strings.TrimSpace(licenseToken),
		httpClient:   &http.Client{Timeout: defaultTimeout},
	}
	for _, o := range opts {
		o(c)
	}
	return c, nil
}

// FromEnv builds a Client from HUB_BASE_URL / HUB_LICENSE_TOKEN. When
// HUB_BASE_URL is unset the default public hub is used so a vanilla install
// can browse the catalog without configuration.
func FromEnv(opts ...Option) (*Client, error) {
	base := strings.TrimSpace(os.Getenv("HUB_BASE_URL"))
	if base == "" {
		base = defaultBaseURL
	}
	return NewClient(base, os.Getenv("HUB_LICENSE_TOKEN"), opts...)
}

// BaseURL returns the configured base URL — useful for audit logs and the
// `source` field surfaced to admins in the marketplace UI.
func (c *Client) BaseURL() string { return c.baseURL }

// HasLicenseToken reports whether the client is authenticated. The public
// catalog works without one; this flag lets the UI hide gated entries when
// it knows the request is anonymous.
func (c *Client) HasLicenseToken() bool { return c.licenseToken != "" }

// AddonSummary is the wire shape of an entry in the Hub catalog. It's a
// trimmed view — full per-version detail lives behind FetchAddon.
type AddonSummary struct {
	Key           string   `json:"key"`
	Name          string   `json:"name"`
	Description   string   `json:"description"`
	Category      string   `json:"category,omitempty"`
	Icon          string   `json:"icon,omitempty"`
	Author        string   `json:"author,omitempty"`
	Verified      bool     `json:"verified"`
	LatestVersion string   `json:"latest_version,omitempty"`
	Price         string   `json:"price,omitempty"`
	Tags          []string `json:"tags,omitempty"`
	Downloads     int      `json:"downloads,omitempty"`
	Rating        float64  `json:"rating,omitempty"`
	UpdatedAt     string   `json:"updated_at,omitempty"`
	RequiresCore  string   `json:"requires_core,omitempty"`
}

// AddonVersion is one publishable artifact for an addon. Keys are normative
// (Hub guarantees) and surface enough info for the UI to render a version
// picker without a second round trip.
type AddonVersion struct {
	Version      string `json:"version"`
	ReleasedAt   string `json:"released_at,omitempty"`
	ReleaseNotes string `json:"release_notes,omitempty"`
	RequiresCore string `json:"requires_core,omitempty"`
	BundleSHA256 string `json:"bundle_sha256,omitempty"`
	SizeBytes    int64  `json:"size_bytes,omitempty"`
	Prerelease   bool   `json:"prerelease,omitempty"`
	Yanked       bool   `json:"yanked,omitempty"`
}

// AddonDetail is the full per-addon record returned by GET /v1/addons/{key}.
// It embeds AddonSummary so the UI's catalog row → detail page transition is
// strictly additive.
type AddonDetail struct {
	AddonSummary
	Versions    []AddonVersion `json:"versions"`
	Screenshots []string       `json:"screenshots,omitempty"`
	Features    []string       `json:"features,omitempty"`
	Homepage    string         `json:"homepage,omitempty"`
	Repository  string         `json:"repository,omitempty"`
	License     string         `json:"license,omitempty"`
	Readme      string         `json:"readme,omitempty"`
}

// CatalogParams is the input to FetchCatalog. All fields are optional — the
// Hub treats zero values as "no filter".
type CatalogParams struct {
	Search   string
	Category string
	Limit    int
	Offset   int
}

// catalogResponse is the on-the-wire shape of GET /v1/catalog. The handler
// layer flattens it to `{success, data, meta}` before responding to the UI.
type catalogResponse struct {
	Items []AddonSummary `json:"items"`
	Total int            `json:"total,omitempty"`
}

// CatalogResult bundles the catalog page with paging metadata so the handler
// can echo `meta.total` to the UI for "showing 1–25 of N" affordances.
type CatalogResult struct {
	Items []AddonSummary
	Total int
}

// Spec describes the Hub's view of the kernel/protocol compatibility window.
// Fields are advisory — consumers use them to decide whether to gate `install`
// on kernel-version compatibility BEFORE downloading the bundle.
type Spec struct {
	HubVersion     string   `json:"hub_version"`
	APIVersion     string   `json:"api_version"`
	KernelVersions []string `json:"kernel_versions,omitempty"`
	BundleFormat   string   `json:"bundle_format,omitempty"`
	SignedBy       []string `json:"signed_by,omitempty"`
}

// FetchCatalog returns one page of catalog entries. The Hub is authoritative
// for sorting; consumers echo the order verbatim.
func (c *Client) FetchCatalog(ctx context.Context, params CatalogParams) (CatalogResult, error) {
	u := c.buildURL("/v1/catalog")
	q := url.Values{}
	if s := strings.TrimSpace(params.Search); s != "" {
		q.Set("search", s)
	}
	if cat := strings.TrimSpace(params.Category); cat != "" {
		q.Set("category", cat)
	}
	if params.Limit > 0 {
		q.Set("limit", strconv.Itoa(params.Limit))
	}
	if params.Offset > 0 {
		q.Set("offset", strconv.Itoa(params.Offset))
	}
	if encoded := q.Encode(); encoded != "" {
		u = u + "?" + encoded
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return CatalogResult{}, err
	}
	c.authorise(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return CatalogResult{}, fmt.Errorf("hub: catalog request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return CatalogResult{}, statusErr(resp, "catalog")
	}
	var out catalogResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return CatalogResult{}, fmt.Errorf("hub: decode catalog: %w", err)
	}
	if out.Items == nil {
		out.Items = []AddonSummary{}
	}
	if out.Total == 0 {
		out.Total = len(out.Items)
	}
	return CatalogResult{Items: out.Items, Total: out.Total}, nil
}

// FetchAddon returns the full per-addon record for the supplied key. The Hub
// returns 404 when the key is unknown; consumers surface that as the same
// error the caller can compare with errors.Is(err, ErrNotFound).
func (c *Client) FetchAddon(ctx context.Context, key string) (*AddonDetail, error) {
	key = strings.TrimSpace(key)
	if key == "" {
		return nil, fmt.Errorf("hub: empty addon key")
	}
	u := c.buildURL("/v1/addons/" + url.PathEscape(key))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	c.authorise(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("hub: addon request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, ErrNotFound
	}
	if resp.StatusCode != http.StatusOK {
		return nil, statusErr(resp, "addon")
	}
	var out AddonDetail
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("hub: decode addon: %w", err)
	}
	return &out, nil
}

// DownloadBundle streams the .tar.gz bundle for (key, version). When version
// is empty the Hub serves whatever it considers "latest" for the addon. The
// caller MUST close the returned ReadCloser; the body is wired directly into
// `kernel/bundle.Read` so the bytes never sit in memory in full.
func (c *Client) DownloadBundle(ctx context.Context, key, version string) (io.ReadCloser, error) {
	key = strings.TrimSpace(key)
	if key == "" {
		return nil, fmt.Errorf("hub: empty addon key")
	}
	u := c.buildURL("/v1/addons/" + url.PathEscape(key) + "/bundle")
	if v := strings.TrimSpace(version); v != "" {
		u = u + "?version=" + url.QueryEscape(v)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	c.authorise(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("hub: bundle request: %w", err)
	}
	if resp.StatusCode == http.StatusNotFound {
		resp.Body.Close()
		return nil, ErrNotFound
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		return nil, statusErr(resp, "bundle")
	}
	return resp.Body, nil
}

// FetchSpec returns the Hub's compat manifest. Until upstream ships the
// endpoint it 404s; consumers return ErrSpecUnavailable so callers can
// soft-fail (continue without compat hints).
func (c *Client) FetchSpec(ctx context.Context) (*Spec, error) {
	u := c.buildURL("/v1/spec")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	c.authorise(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("hub: spec request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, ErrSpecUnavailable
	}
	if resp.StatusCode != http.StatusOK {
		return nil, statusErr(resp, "spec")
	}
	var out Spec
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("hub: decode spec: %w", err)
	}
	return &out, nil
}

// authorise adds Authorization + Accept + User-Agent headers consistent with
// the Hub contract. Bearer token is sent only when configured; the public
// catalog stays browsable without authentication.
func (c *Client) authorise(req *http.Request) {
	if c.licenseToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.licenseToken)
	}
	req.Header.Set("Accept", "application/json, application/gzip")
	req.Header.Set("User-Agent", userAgent)
}

// buildURL joins the base URL with a relative path. The path is expected to
// be absolute (`/v1/...`); the function trims any duplicate leading slashes.
func (c *Client) buildURL(path string) string {
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	return c.baseURL + path
}

// statusErr formats a non-2xx response into a debuggable error. We cap the
// body snippet so a misbehaving Hub can't blow up our logs.
func statusErr(resp *http.Response, kind string) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	snippet := strings.TrimSpace(string(body))
	if len(snippet) > 200 {
		snippet = snippet[:200] + "..."
	}
	return fmt.Errorf("hub: %s returned %d: %s", kind, resp.StatusCode, snippet)
}
