package dynamic

import (
	"strings"

	"github.com/google/uuid"
)

// Isolation mirrors manifest.TenantIsolation values. Kept here as typed
// constants to avoid stringly-typed checks scattered across the kernel.
type Isolation string

const (
	// IsolationShared — single schema, all orgs share tables, RLS enforces
	// row-level isolation.
	IsolationShared Isolation = "shared"
	// IsolationPerTenant — one schema per installation; zero cross-org data.
	IsolationPerTenant Isolation = "schema-per-tenant"
)

// ParseIsolation normalizes the manifest field. Empty string defaults to
// shared so legacy addons authored before v2 keep working unchanged.
func ParseIsolation(v string) Isolation {
	switch v {
	case string(IsolationPerTenant):
		return IsolationPerTenant
	default:
		return IsolationShared
	}
}

// SchemaName returns the Postgres schema where an addon's tables live.
// For shared addons this is the global addon_<key>. For tenant-isolated
// addons it is addon_<key>_<orgshort>, where orgshort is the first 8 hex
// characters of the org UUID (collision-free for any realistic population).
func SchemaName(addonKey string, orgID uuid.UUID, iso Isolation) string {
	base := "addon_" + strings.ToLower(addonKey)
	if iso == IsolationPerTenant {
		return base + "_" + orgShort(orgID)
	}
	return base
}

// orgShort produces a stable 8-hex-character prefix from a UUID. This is a
// *namespace* suffix, not a secret — 32 bits of entropy is plenty when the
// cardinality is (# orgs with this addon installed).
func orgShort(orgID uuid.UUID) string {
	hex := strings.ReplaceAll(orgID.String(), "-", "")
	if len(hex) < 8 {
		return hex
	}
	return hex[:8]
}

// SharedSchemaName is a convenience for callers that only need the shared
// layout — navigation, catalog views, etc.
func SharedSchemaName(addonKey string) string {
	return SchemaName(addonKey, uuid.Nil, IsolationShared)
}
