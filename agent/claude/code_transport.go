package claude

import (
	"net/http"
	"strings"
	"time"
)

// claudeCodeRoundTripper rewrites Anthropic requests so they authenticate with
// a Claude Code OAuth bearer token instead of x-api-key. langchaingo's Anthropic
// client always sets x-api-key; we strip it and inject Authorization + the
// oauth beta header required by Claude Pro/Max seats.
type claudeCodeRoundTripper struct {
	base http.RoundTripper
}

func (t *claudeCodeRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}

	token, err := EnsureClaudeCodeAccessToken()
	if err != nil {
		return nil, err
	}

	req2 := req.Clone(req.Context())
	req2.Header.Del("X-Api-Key")
	req2.Header.Del("x-api-key")
	req2.Header.Set("Authorization", "Bearer "+token)

	beta := claudeCodeOAuthBeta
	if existing := req2.Header.Get("anthropic-beta"); existing != "" {
		if !strings.Contains(existing, claudeCodeOAuthBeta) {
			beta = existing + "," + claudeCodeOAuthBeta
		} else {
			beta = existing
		}
	}
	req2.Header.Set("anthropic-beta", beta)

	return base.RoundTrip(req2)
}

func newClaudeCodeHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout:   timeout,
		Transport: &claudeCodeRoundTripper{},
	}
}
