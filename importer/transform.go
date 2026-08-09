package importer

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"path"
	"strings"
	"sync"
	"time"
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

// TransformDeps are request-scoped dependencies for transforms that need I/O.
// Nil fields mean the corresponding builtin transforms will fail with a clear
// error instead of panicking.
type TransformDeps struct {
	HTTPClient *http.Client
	Store      MediaStore
	// Context cancels in-flight fetches when the request ends. Optional —
	// builtins fall back to context.Background with a short timeout.
	Context context.Context
}

var (
	xfMu       sync.RWMutex
	transforms = map[string]TransformFunc{}
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
}

func lookupTransform(name string) (TransformFunc, bool) {
	xfMu.RLock()
	defer xfMu.RUnlock()
	fn, ok := transforms[name]
	return fn, ok
}

const (
	defaultMediaTimeout = 10 * time.Second
	defaultMediaMaxBytes = 5 << 20 // 5 MiB
)

func transformDepsContext(deps *TransformDeps) (context.Context, context.CancelFunc) {
	base := context.Background()
	if deps != nil && deps.Context != nil {
		base = deps.Context
	}
	return context.WithTimeout(base, defaultMediaTimeout)
}

func httpClient(deps *TransformDeps) *http.Client {
	if deps != nil && deps.HTTPClient != nil {
		return deps.HTTPClient
	}
	return &http.Client{Timeout: defaultMediaTimeout}
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
	for _, c := range chunks {
		c = strings.TrimSpace(c)
		if c != "" {
			out = append(out, c)
		}
	}
	return out
}

func fetchAndStore(deps *TransformDeps, rawURL string) (string, error) {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return "", fmt.Errorf("URL vacía")
	}
	if !strings.HasPrefix(rawURL, "http://") && !strings.HasPrefix(rawURL, "https://") {
		return "", fmt.Errorf("URL debe empezar con http:// o https:// (recibido: %q)", rawURL)
	}
	if deps == nil || deps.Store == nil {
		return "", fmt.Errorf("media transform requires TransformDeps.Store (host must call PrepareWithDeps)")
	}

	reqCtx, cancel := transformDepsContext(deps)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", fmt.Errorf("URL inválida: %w", err)
	}
	resp, err := httpClient(deps).Do(req)
	if err != nil {
		return "", fmt.Errorf("no se pudo descargar: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("descarga HTTP %d", resp.StatusCode)
	}

	ct := resp.Header.Get("Content-Type")
	if ct == "" {
		ct = "application/octet-stream"
	}
	suggested := path.Base(strings.Split(rawURL, "?")[0])
	if suggested == "" || suggested == "." || suggested == "/" {
		suggested = "import-media"
	}

	limited := io.LimitReader(resp.Body, defaultMediaMaxBytes+1)
	stored, err := deps.Store.Put(reqCtx, suggested, limited, ct)
	if err != nil {
		return "", fmt.Errorf("no se pudo guardar: %w", err)
	}
	return stored, nil
}
