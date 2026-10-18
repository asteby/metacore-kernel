package agent

import (
	"errors"
	"net"
	"net/http"
	"time"

	"github.com/asteby/metacore-kernel/netguard"
)

// ErrQueryToolsDisabled is returned for tools of type "query". They used to run
// the tool's Endpoint as raw SQL against the SHARED database connection — no
// organization scoping, and parameters interpolated as text — so any org user
// could read every tenant's tables (users, credentials, …) through the bot
// tool editor's "test" button or through the LLM itself (QA-EXP-ROLES).
// Tenant-authored SQL cannot be made safe on a multi-tenant database; use an
// "api"/"webhook" tool against the data API instead.
var ErrQueryToolsDisabled = errors.New("las herramientas de tipo consulta SQL están deshabilitadas por seguridad; usa una herramienta de tipo API")

// ErrBlockedEgress is returned when a tool targets a loopback, private,
// link-local (cloud metadata) or otherwise internal address. It is the shared
// netguard error, so errors.Is works across packages.
var ErrBlockedEgress = netguard.ErrBlockedEgress

// allowPrivateEgress lets tests point tools at an httptest server on
// 127.0.0.1. Never set in production code paths.
var allowPrivateEgress = false

// isBlockedEgressIP is kept as an alias of the shared rule.
func isBlockedEgressIP(ip net.IP) bool { return netguard.IsBlockedIP(ip) }

// NewGuardedHTTPClient returns an http.Client for tenant-configured outbound
// calls: every connection (including redirects) is checked after DNS
// resolution by netguard, and proxies from the environment are ignored so the
// check sees the real destination.
func NewGuardedHTTPClient(timeout time.Duration) *http.Client {
	return netguard.NewClient(netguard.Options{
		Timeout:      timeout,
		MaxRedirects: 5,
		AllowPrivate: allowPrivateEgress,
	})
}
