package domains

import "strings"

// withCloudflareBaseURL points the provider at a test server. It lives in a
// _test.go file on purpose: production code cannot redirect the API token.
func withCloudflareBaseURL(u string) CloudflareOption {
	return func(c *Cloudflare) { c.base = strings.TrimSuffix(u, "/") }
}
