package licensing

import (
	"strings"
	"time"
)

// State is an immutable snapshot of the instance's licensing posture. The
// service swaps it atomically; callers get a consistent view without locking.
// Its JSON shape is the contract the SDK's <LicenseGate> and every host admin
// UI consume — keep the field tags stable.
type State struct {
	// Enforced mirrors the enforce flag. When false the gates are advisory:
	// State is still computed and surfaced, but nothing is blocked.
	Enforced bool `json:"enforced"`
	// Configured reports whether any token is present at all.
	Configured bool `json:"configured"`
	// Valid is true only when a token verified AND is within its window.
	Valid bool `json:"valid"`
	// Status is the coarse posture: valid | stale | grace | expired | missing | invalid.
	Status string `json:"status"`
	// Reason carries the verification error for missing/invalid/expired.
	Reason string `json:"reason,omitempty"`

	// OrgID carries the license's customer id (claims.cid).
	OrgID string `json:"org_id,omitempty"`
	// Plan is the commercial plan label; "unlimited" = first-party perpetual.
	Plan   string `json:"plan,omitempty"`
	Preset string `json:"preset,omitempty"`
	// Entitlements is addons ∪ presets from the claims ("*" = wildcard).
	Entitlements []string `json:"entitlements"`
	Wildcard     bool     `json:"wildcard"`

	// Stale = lease expired: the instance missed its check-in window
	// (max_offline_hours since the last renew) while the license window is
	// still open. Operable but degraded; renew clears it.
	Stale           bool `json:"stale"`
	MaxOfflineHours int  `json:"max_offline_hours,omitempty"`

	IssuedAt      *time.Time `json:"issued_at,omitempty"`
	ExpiresAt     *time.Time `json:"expires_at,omitempty"`
	InGrace       bool       `json:"in_grace"`
	GraceUntil    *time.Time `json:"grace_until,omitempty"`
	DaysRemaining int        `json:"days_remaining"`
	LastCheckedAt *time.Time `json:"last_checked_at,omitempty"`
}

// Entitles reports whether the current snapshot covers addonKey. Wildcard
// short-circuits. Used by the install gate.
func (s *State) Entitles(addonKey string) bool {
	if s.Wildcard {
		return true
	}
	addonKey = strings.ToLower(strings.TrimSpace(addonKey))
	for _, k := range s.Entitlements {
		if strings.ToLower(strings.TrimSpace(k)) == addonKey {
			return true
		}
	}
	return false
}

// Operable reports whether the instance may run normally: enforcement off, or
// the license is valid or within its grace window. The pure predicate the
// write/install gates are built from.
func (s *State) Operable() bool {
	return !s.Enforced || s.Valid || s.InGrace
}

// WritesBlocked reports whether write operations should be denied: enforcement
// on, and the license neither valid nor within grace. Equivalent to an enforced
// instance that is not Operable — hosts wire this into a mutating-request
// middleware (deny non-GET, keep reads + the recovery surface open).
func (s *State) WritesBlocked() bool {
	return s.Enforced && !s.Operable()
}

// InstallBlocked reports whether new installs/claims should be denied outright:
// enforcement on and the instance not Operable. Beyond this coarse gate the
// host still checks Entitles(key) per addon (a wildcard passes everything).
func (s *State) InstallBlocked() bool {
	return s.Enforced && !s.Operable()
}
