package claude

import (
	"net/http"
	"strings"
	"time"
)

// claudeCodeRoundTripper rewrites Anthropic requests so they authenticate with
// a Claude Code OAuth bearer token instead of x-api-key. langchaingo's Anthropic
// client always sets x-api-key; we strip it and inject Authorization + the
// oauth beta header required by Claude Pro/Max seats. It also drops sampling
// params (temperature/top_p/top_k) for models that reject them (Claude ≥5).
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

	// Claude 5.x rechaza temperature/top_p, que langchaingo siempre envía.
	req3, err := sanitizeSamplingRequest(req2)
	if err != nil {
		return nil, err
	}
	return base.RoundTrip(req3)
}

// NewCodeHTTPClient returns an http.Client that authenticates Anthropic requests
// with the host Claude Code OAuth seat instead of x-api-key.
func NewCodeHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout:   timeout,
		Transport: &claudeCodeRoundTripper{},
	}
}
