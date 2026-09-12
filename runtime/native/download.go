package native

import (
	"context"
	"fmt"
	"io"
	"net/http"
)

// MaxDownloadBytes bounds an out-of-band artifact fetch (Artifact.DownloadURL)
// to defend against a compromised or misconfigured download host serving an
// oversized or unbounded response. It is deliberately larger than the
// bundle's own 64 MiB decompressed-size cap (bundle.Read) — DownloadURL
// exists specifically for payloads that don't fit inside that cap — while
// still bounding the worst case for host memory and disk.
//
// It is a var rather than a const solely so tests can shrink it to keep
// fixtures small; production code must never mutate it at runtime.
var MaxDownloadBytes int64 = 256 << 20 // 256 MiB

// FetchArtifact downloads artifact.DownloadURL and verifies the fetched bytes
// against artifact.SHA256 before returning them. The digest check happens
// strictly after the full, capped download completes: the URL is only a
// transport hint, never a trust boundary. Callers still MUST NOT execute or
// persist the returned bytes anywhere durable until this returns nil — that
// discipline lives here so every host implementation gets it for free.
//
// client may be nil, in which case http.DefaultClient is used. Callers
// concerned with timeouts/redirect policy should pass a configured client;
// this function applies no timeout of its own beyond ctx's.
func FetchArtifact(ctx context.Context, client *http.Client, artifact Artifact) ([]byte, error) {
	if artifact.DownloadURL == "" {
		return nil, fmt.Errorf("native runtime: artifact %q has no download_url", artifact.Path)
	}
	if client == nil {
		client = http.DefaultClient
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, artifact.DownloadURL, nil)
	if err != nil {
		return nil, fmt.Errorf("native runtime: build download request: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("native runtime: download artifact: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("native runtime: download artifact: unexpected status %d", resp.StatusCode)
	}
	// Read one byte past the cap so an oversized response is detected instead
	// of silently truncated (which would otherwise pass a coincidental digest
	// check against a truncated file — it wouldn't, but truncating byte
	// content without noticing is still the wrong failure mode).
	limited := io.LimitReader(resp.Body, MaxDownloadBytes+1)
	payload, err := io.ReadAll(limited)
	if err != nil {
		return nil, fmt.Errorf("native runtime: read downloaded artifact: %w", err)
	}
	if int64(len(payload)) > MaxDownloadBytes {
		return nil, fmt.Errorf("native runtime: downloaded artifact exceeds %d bytes", MaxDownloadBytes)
	}
	if err := VerifyDownloadedArtifact(artifact, payload); err != nil {
		return nil, err
	}
	return payload, nil
}
