package permission

import "strings"

// Capability is the canonical authorization unit: "resource.action". Apps
// declare capabilities when they register their models (via the metadata
// package) — the kernel itself never hardcodes the full list.
//
// Examples: "users.create", "products.delete", "invoices.approve".
//
// The wildcard "*" is reserved: a role or user with "*" in its capability set
// is treated as having every capability. Apps should not create capabilities
// containing "*" for any other purpose.
type Capability string

// Common action verbs shared by every app's CRUD surface. Apps are free to
// introduce their own verbs (e.g. "approve", "cancel") — these constants just
// cover the 80% case and keep call sites readable.
const (
	CapCreate Capability = "create"
	CapRead   Capability = "read"
	CapUpdate Capability = "update"
	CapDelete Capability = "delete"
	CapList   Capability = "list"
	CapExport Capability = "export"
	CapImport Capability = "import"
)

// Wildcard grants every capability when present in a user's or role's grant
// set. Exported so apps can seed an "admin-style" role without importing a
// string literal.
const Wildcard Capability = "*"

// Cap builds a "resource.action" capability. It trims whitespace and lowers
// the resource segment so callers need not worry about casing differences
// between code and database rows.
//
//	permission.Cap("Users", "Create") // -> Capability("users.create")
func Cap(resource, action string) Capability {
	r := strings.ToLower(strings.TrimSpace(resource))
	a := strings.TrimSpace(action)
	if r == "" {
		return Capability(a)
	}
	if a == "" {
		return Capability(r)
	}
	return Capability(r + "." + a)
}

// Resource returns the segment before the first dot, or the whole capability
// if there is no dot. Empty for the wildcard.
func (c Capability) Resource() string {
	s := string(c)
	if s == string(Wildcard) {
		return ""
	}
	if i := strings.IndexByte(s, '.'); i >= 0 {
		return s[:i]
	}
	return s
}

// Action returns the segment after the first dot, or "" if there is no dot.
func (c Capability) Action() string {
	s := string(c)
	if i := strings.IndexByte(s, '.'); i >= 0 {
		return s[i+1:]
	}
	return ""
}

// String returns the raw "resource.action" form.
func (c Capability) String() string { return string(c) }

// Matches returns true if c satisfies want. The current semantics:
//
//   - Wildcard grants everything.
//   - Otherwise the match is exact.
//
// A future extension could introduce "users.*" patterns; keeping the rule
// simple today avoids surprise and keeps the cache keys trivially comparable.
func (c Capability) Matches(want Capability) bool {
	if c == Wildcard {
		return true
	}
	return c == want
}
