package importer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"mime"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/asteby/metacore-kernel/netguard"
)

// TransformFunc post-processes a coerced cell value. Hosts register named
// transforms (and inject Deps via PrepareWithDeps) so domain work like
// "fetch this URL into my storage" stays out of the engine.
type TransformFunc func(ctx TransformContext, raw string) (any, error)

// TransformContext is the per-cell environment passed to a TransformFunc.
type TransformContext struct {
	Column   string // ImportColumn.Key
	Header   string
	RowIndex int // 0-based index among data rows being prepared (not spreadsheet row)
	Deps     *TransformDeps
}

// MediaStore persists bytes fetched during a media transform. Hosts implement
// this against their own disk / S3 layout; the engine only needs the stored
// filename (or relative key) back so it can write it into the create record.
type MediaStore interface {
	Put(ctx context.Context, suggestedName string, r io.Reader, contentType string) (storedName string, err error)
}

// HashedMediaStore is an optional MediaStore extension. When the host's Store
// implements it, the engine deduplicates by content: Lookup first (a hit
// writes nothing) and PutHashed otherwise. Names are keyed by the lowercase
// hex sha256 of the bytes.
type HashedMediaStore interface {
	MediaStore
	Lookup(ctx context.Context, sha256hex string) (storedName string, ok bool, err error)
	PutHashed(ctx context.Context, sha256hex, suggestedName string, r io.Reader, contentType string) (storedName string, err error)
}

// MediaPolicy bounds what media_url / media_url_list may download. The zero
// value is a safe default.
type MediaPolicy struct {
	// MaxBytes per file; default 15 MiB. Exceeding it is an error: a truncated
	// file is never stored.
	MaxBytes int64
	// AllowedTypes are MIME types accepted after sniffing the bytes (the
	// server's Content-Type is informational only). Default: jpeg, png, webp,
	// gif, avif. SVG is never in the default (it can carry scripts).
	AllowedTypes []string
	// AllowHosts restricts source hosts ("cdn.example.com", "*.example.com").
	// Empty = any public host. Redirects are re-checked against it.
	AllowHosts []string
	// MaxAttempts per URL; default 4.
	MaxAttempts int
	// BaseBackoff; default 500ms. Wait = Base * 2^(n-1) +-20% jitter, capped at
	// 8s; a Retry-After header (capped at 30s) takes precedence.
	BaseBackoff time.Duration
}

// TransformDeps are request-scoped dependencies for transforms that need I/O.
// Nil fields mean the corresponding builtin transforms will fail with a clear
// error instead of panicking.
type TransformDeps struct {
	// HTTPClient: when nil a netguard.NewClient (SSRF-safe) is used. A client
	// supplied by the host is respected as-is: the host owns its guard.
	HTTPClient *http.Client
	Store      MediaStore
	// Context cancels in-flight fetches when the request ends. Optional —
	// builtins fall back to context.Background.
	Context context.Context
	// Policy bounds media downloads (see MediaPolicy).
	Policy MediaPolicy
	// Sleep waits between retries; injectable for tests. Default honors ctx.
	Sleep func(context.Context, time.Duration) error
}

var (
	xfMu       sync.RWMutex
	transforms = map[string]TransformFunc{}
	// rawTransforms are the transforms that run before (instead of after) the
	// type coercion of their column.
	rawTransforms = map[string]bool{}
)

func init() {
	RegisterTransform("media_url", transformMediaURL)
	RegisterTransform("media_url_list", transformMediaURLList)
}

// RegisterTransform makes a named transform available to every model's
// ImportSpec. Registering twice under the same name replaces the previous one.
func RegisterTransform(name string, fn TransformFunc) {
	if name == "" || fn == nil {
		return
	}
	xfMu.Lock()
	defer xfMu.Unlock()
	transforms[name] = fn
	delete(rawTransforms, name) // re-registering as a normal transform clears the raw flag
}

// RegisterRawTransform registers a transform that parses the RAW cell text and
// returns the final typed value itself: the column's Type coercion is skipped
// for it (a "number" column with number_mixed must see "1.059,32", which
// strconv would reject before the transform ran). It is the same registry as
// RegisterTransform; only the order of operations differs.
func RegisterRawTransform(name string, fn TransformFunc) {
	if name == "" || fn == nil {
		return
	}
	RegisterTransform(name, fn)
	xfMu.Lock()
	defer xfMu.Unlock()
	rawTransforms[name] = true
}

func isRawTransform(name string) bool {
	xfMu.RLock()
	defer xfMu.RUnlock()
	return rawTransforms[name]
}

func lookupTransform(name string) (TransformFunc, bool) {
	xfMu.RLock()
	defer xfMu.RUnlock()
	fn, ok := transforms[name]
	return fn, ok
}

const (
	defaultMediaTimeout     = 15 * time.Second // per attempt
	defaultMediaMaxBytes    = 15 << 20         // 15 MiB
	defaultMediaMaxAttempts = 4
	defaultMediaBackoff     = 500 * time.Millisecond
	maxMediaBackoff         = 8 * time.Second
	maxRetryAfter           = 30 * time.Second
	sniffLen                = 512
)

var defaultMediaTypes = []string{"image/jpeg", "image/png", "image/webp", "image/gif", "image/avif"}

// Typed media errors; all are wrapped with %w so errors.Is / errors.As work.
var (
	// ErrMediaTooLarge: the file exceeds MediaPolicy.MaxBytes.
	ErrMediaTooLarge = errors.New("el archivo excede el tamaño máximo permitido")
	// ErrMediaType: the sniffed type is not allowed (or not an image at all).
	ErrMediaType = errors.New("tipo de archivo no permitido")
	// ErrMediaHost: the host is outside MediaPolicy.AllowHosts.
	ErrMediaHost = netguard.ErrHostNotAllowed
)

// ErrMediaHTTP is a non-2xx download response.
type ErrMediaHTTP struct{ Status int }

func (e ErrMediaHTTP) Error() string { return fmt.Sprintf("descarga HTTP %d", e.Status) }

func (p MediaPolicy) maxBytes() int64 {
	if p.MaxBytes > 0 {
		return p.MaxBytes
	}
	return defaultMediaMaxBytes
}

func (p MediaPolicy) maxAttempts() int {
	if p.MaxAttempts > 0 {
		return p.MaxAttempts
	}
	return defaultMediaMaxAttempts
}

func (p MediaPolicy) baseBackoff() time.Duration {
	if p.BaseBackoff > 0 {
		return p.BaseBackoff
	}
	return defaultMediaBackoff
}

func (p MediaPolicy) typeAllowed(t string) bool {
	list := p.AllowedTypes
	if len(list) == 0 {
		list = defaultMediaTypes
	}
	for _, a := range list {
		if strings.EqualFold(strings.TrimSpace(a), t) {
			return true
		}
	}
	return false
}

func transformDepsContext(deps *TransformDeps) context.Context {
	if deps != nil && deps.Context != nil {
		return deps.Context
	}
	return context.Background()
}

func httpClient(deps *TransformDeps) *http.Client {
	if deps != nil && deps.HTTPClient != nil {
		return deps.HTTPClient
	}
	return netguard.NewClient(netguard.Options{
		Timeout:    defaultMediaTimeout,
		AllowHosts: deps.Policy.AllowHosts,
	})
}

func sleepFn(deps *TransformDeps) func(context.Context, time.Duration) error {
	if deps.Sleep != nil {
		return deps.Sleep
	}
	return func(ctx context.Context, d time.Duration) error {
		t := time.NewTimer(d)
		defer t.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			return nil
		}
	}
}

func transformMediaURL(ctx TransformContext, raw string) (any, error) {
	name, err := fetchAndStore(ctx.Deps, raw)
	if err != nil {
		return nil, err
	}
	return name, nil
}

func transformMediaURLList(ctx TransformContext, raw string) (any, error) {
	parts := splitMediaList(raw)
	if len(parts) == 0 {
		return "", nil
	}
	stored := make([]string, 0, len(parts))
	for _, u := range parts {
		name, err := fetchAndStore(ctx.Deps, u)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", u, err)
		}
		stored = append(stored, name)
	}
	return strings.Join(stored, "|"), nil
}

func splitMediaList(raw string) []string {
	replacer := strings.NewReplacer(",", "|", ";", "|", "\n", "|")
	normalized := replacer.Replace(raw)
	chunks := strings.Split(normalized, "|")
	out := make([]string, 0, len(chunks))
	seen := make(map[string]struct{}, len(chunks))
	for _, c := range chunks {
		c = strings.TrimSpace(c)
		if c == "" {
			continue
		}
		if _, dup := seen[c]; dup {
			continue
		}
		seen[c] = struct{}{}
		out = append(out, c)
	}
	return out
}

func fetchAndStore(deps *TransformDeps, rawURL string) (string, error) {
	if deps == nil || deps.Store == nil {
		return "", fmt.Errorf("media transform requires TransformDeps.Store (host must call PrepareWithDeps)")
	}
	f, err := FetchMedia(deps, rawURL)
	if err != nil {
		return "", err
	}
	return storeMedia(transformDepsContext(deps), deps, f.URL, f.Data, f.Type)
}

// FetchedMedia es un medio descargado y validado (tipo por magic bytes, tamaño
// dentro de la política) que todavía no se guardó en ningún lado.
type FetchedMedia struct {
	URL  *url.URL
	Data []byte
	// Type es el MIME detectado por los bytes, no el que declara el servidor.
	Type string
	// SuggestedName conserva el nombre base de la URL con la extensión del tipo.
	SuggestedName string
	// ETag y LastModified son los validadores que el servidor mandó; sirven para
	// revalidar después con FetchOptions.
	ETag         string
	LastModified string
	// NotModified: el servidor respondió 304 a una petición condicional; Data va
	// vacío y lo que ya se tenía sigue vigente.
	NotModified bool
	// ResumedBytes son los bytes que no hizo falta volver a bajar porque una
	// descarga cortada se reanudó con Range.
	ResumedBytes int64
}

// IsRetryable indica si vale la pena reintentar más tarde tras err: errores de
// red, timeouts, 408/425/429 y 5xx. Los 4xx definitivos, los bloqueos de
// netguard, host/tamaño/tipo y la cancelación no se reintentan.
func IsRetryable(ctx context.Context, err error) bool { return retryable(ctx, err) }

// retryable: network errors, timeouts, 408/425/429 and 5xx. Definitive 4xx,
// netguard blocks, host/size/type errors and a cancelled context are not.
func retryable(ctx context.Context, err error) bool {
	if ctx.Err() != nil {
		return false
	}
	var he ErrMediaHTTP
	if errors.As(err, &he) {
		switch he.Status {
		case 408, 425, 429:
			return true
		}
		return he.Status >= 500
	}
	for _, fatal := range []error{
		netguard.ErrBlockedEgress, netguard.ErrHostNotAllowed, netguard.ErrTooManyRedirects,
		ErrMediaTooLarge, ErrMediaType, context.Canceled,
	} {
		if errors.Is(err, fatal) {
			return false
		}
	}
	return true
}

// backoff is base * 2^(n-1) with +-20% jitter, capped at 8s.
func backoff(base time.Duration, n int) time.Duration {
	d := base
	for i := 1; i < n && d < maxMediaBackoff; i++ {
		d *= 2
	}
	d = time.Duration(float64(d) * (0.8 + 0.4*rand.Float64()))
	if d > maxMediaBackoff {
		d = maxMediaBackoff
	}
	return d
}

// parseRetryAfter accepts delta-seconds or an HTTP date; capped at 30s.
func parseRetryAfter(v string) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	var d time.Duration
	if secs, err := strconv.Atoi(v); err == nil {
		d = time.Duration(secs) * time.Second
	} else if t, err := http.ParseTime(v); err == nil {
		d = time.Until(t)
	}
	if d < 0 {
		return 0
	}
	if d > maxRetryAfter {
		d = maxRetryAfter
	}
	return d
}

// detectImageType sniffs the real type from the bytes.
func detectImageType(b []byte) string {
	if len(b) > sniffLen {
		b = b[:sniffLen]
	}
	// AVIF: ISO-BMFF box "ftyp" with major brand avif/avis.
	if len(b) >= 12 && string(b[4:8]) == "ftyp" && (string(b[8:12]) == "avif" || string(b[8:12]) == "avis") {
		return "image/avif"
	}
	if len(b) >= 12 && string(b[0:4]) == "RIFF" && string(b[8:12]) == "WEBP" {
		return "image/webp"
	}
	t, _, err := mime.ParseMediaType(http.DetectContentType(b))
	if err != nil {
		return "application/octet-stream"
	}
	return t
}

var extByType = map[string]string{
	"image/jpeg": ".jpg", "image/png": ".png", "image/webp": ".webp",
	"image/gif": ".gif", "image/avif": ".avif",
}

func storeMedia(ctx context.Context, deps *TransformDeps, u *url.URL, data []byte, _ string) (string, error) {
	detected := detectImageType(data)
	if !deps.Policy.typeAllowed(detected) {
		return "", fmt.Errorf("%w: %s", ErrMediaType, detected)
	}
	sum := sha256.Sum256(data)
	hexsum := hex.EncodeToString(sum[:])
	suggested := suggestName(u, detected)

	if hs, ok := deps.Store.(HashedMediaStore); ok {
		if name, found, err := hs.Lookup(ctx, hexsum); err != nil {
			return "", fmt.Errorf("no se pudo guardar: %w", err)
		} else if found {
			return name, nil
		}
		name, err := hs.PutHashed(ctx, hexsum, suggested, bytes.NewReader(data), detected)
		if err != nil {
			return "", fmt.Errorf("no se pudo guardar: %w", err)
		}
		return name, nil
	}
	name, err := deps.Store.Put(ctx, suggested, bytes.NewReader(data), detected)
	if err != nil {
		return "", fmt.Errorf("no se pudo guardar: %w", err)
	}
	return name, nil
}

// suggestName keeps the URL's base name but takes the extension from the
// detected type, never from the URL.
func suggestName(u *url.URL, detected string) string {
	base := path.Base(u.Path)
	if base == "" || base == "." || base == "/" {
		base = "import-media"
	}
	base = strings.TrimSuffix(base, path.Ext(base))
	if base == "" {
		base = "import-media"
	}
	return base + extByType[detected]
}
