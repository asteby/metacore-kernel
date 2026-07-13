// Package licensing is the kernel's embedder-agnostic INSTANCE licensing
// primitive. It is the shared enforcement half of the metacore trust chain:
// the hub (the central marketplace trust anchor) mints Ed25519-signed instance
// license tokens that carry an instance's entitlement set (presets + addons,
// or "*"), a validity window, a grace period and an optional LEASE (max hours
// between check-ins); this package verifies those tokens OFFLINE against the
// hub's public key, caches the resulting entitlement snapshot, re-checks it
// periodically, and (when the instance is linked + online) auto-renews the
// token before it lapses — the renew IS the mandatory check-in that keeps a
// leased license from going stale and makes remote revocation effective.
//
// The license authorizes the INSTANCE to EXIST; it never limits what you may
// install beyond the entitlement set the hub chose to grant. Every metacore
// product — ops, the verticals, self-contained appliances — embeds this same
// primitive so "the business is licensed at the base" for free: they differ
// only in how they persist the token (the LicenseStore interface) and how they
// wire the resulting State into their request pipeline (the gate helpers).
//
// The verifier + Claims here are a faithful, generic port of the ops consumer
// (services/instancelicense); the JSON field tags are the hub's real wire
// format, so a token minted by the hub verifies here byte-for-byte unchanged.
//
// CANONICAL WIRE FORMAT (identical to the hub's internal/license):
//
//	token = base64url(claimsJSON) + "." + base64url(ed25519Sig)
//	claimsJSON = compact json.Marshal of Claims{v,typ:"instance",lid,
//	                 cid,iid?,ipk?,plan?,presets?,addons?,iat,exp,grace?,moh?}
//	sig = ed25519.Sign(hubPriv, claimsJSON)
//
// Verify: split on ".", base64url-decode both halves, ed25519.Verify over the
// RAW claims bytes (never a re-marshal), unmarshal, then check the window.
package licensing

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// tokenVersion is the only claims version this verifier accepts. Bumping it on
// the hub without teaching the kernel the new shape yields a clean rejection
// instead of a silent misparse.
const tokenVersion = 1

// claimType is the discriminator the hub stamps on instance licenses; annual
// on-prem and trial tokens carry a different (or no) typ and must be rejected.
const claimType = "instance"

// PlanUnlimited is the first-party perpetual plan: wildcard entitlements,
// ~perpetual expiry, exempt from the lease. Mirrors the hub's constant.
const PlanUnlimited = "unlimited"

// Claims is the signed payload of an instance license. Field tags mirror the
// hub's internal/license.InstanceClaims byte-for-byte so signatures match.
type Claims struct {
	Version    int       `json:"v"`
	Type       string    `json:"typ"` // always "instance"
	LicenseID  uuid.UUID `json:"lid"`
	CustomerID uuid.UUID `json:"cid"`
	// InstanceID/InstancePubKey are set once the license was activated against
	// a concrete instance; the verifier can confirm the token names THIS box.
	InstanceID     uuid.UUID `json:"iid,omitempty"`
	InstancePubKey string    `json:"ipk,omitempty"`

	Plan    string   `json:"plan,omitempty"`
	Presets []string `json:"presets,omitempty"`
	Addons  []string `json:"addons,omitempty"`

	IssuedAt  time.Time `json:"iat"`
	ExpiresAt time.Time `json:"exp"`
	// GraceDays extends the operable (degraded) window past ExpiresAt.
	GraceDays int `json:"grace,omitempty"`
	// MaxOfflineHours is the LEASE: max hours without a successful check-in
	// (renew re-stamps iat) before the license goes stale. 0 = no lease.
	MaxOfflineHours int `json:"moh,omitempty"`
}

// Errors callers branch on. ErrExpired is distinct so the service can apply a
// grace window instead of treating the license as absent.
var (
	ErrMalformed   = errors.New("licensing: token is malformed")
	ErrBadSig      = errors.New("licensing: signature does not verify against the hub public key")
	ErrExpired     = errors.New("licensing: license has expired")
	ErrNotYetValid = errors.New("licensing: license is not valid yet (issued_at in the future)")
	ErrNoPubKey    = errors.New("licensing: no trusted hub public key configured")
	ErrVersion     = errors.New("licensing: unsupported token version")
	ErrWrongType   = errors.New("licensing: token is not an instance license")
)

var enc = base64.RawURLEncoding

// Unlimited reports whether this is the first-party perpetual plan — exempt
// from the lease and from lapse-driven degradation.
func (c *Claims) Unlimited() bool {
	return strings.EqualFold(strings.TrimSpace(c.Plan), PlanUnlimited)
}

// Wildcard reports whether the claims grant "*" (any addon/preset).
func (c *Claims) Wildcard() bool {
	for _, k := range c.Addons {
		if strings.TrimSpace(k) == "*" {
			return true
		}
	}
	return false
}

// Entitlements is the flattened entitlement set the host UI + install gate use:
// addon keys ∪ preset keys ("*" rides through as-is). The hub is the authority
// for expanding a preset into its member addons at install time — locally a
// granted preset entitles the preset key itself.
func (c *Claims) Entitlements() []string {
	out := make([]string, 0, len(c.Addons)+len(c.Presets))
	seen := map[string]struct{}{}
	add := func(k string) {
		k = strings.ToLower(strings.TrimSpace(k))
		if k == "" {
			return
		}
		if _, ok := seen[k]; ok {
			return
		}
		seen[k] = struct{}{}
		out = append(out, k)
	}
	for _, k := range c.Addons {
		add(k)
	}
	for _, k := range c.Presets {
		add(k)
	}
	return out
}

// Entitles reports whether the claims cover key — an addon key or a preset
// key, case-insensitive. Wildcard short-circuits to true.
func (c *Claims) Entitles(key string) bool {
	if c.Wildcard() {
		return true
	}
	key = strings.ToLower(strings.TrimSpace(key))
	if key == "" {
		return false
	}
	for _, k := range c.Entitlements() {
		if k == key {
			return true
		}
	}
	return false
}

// GraceUntil is the instant the license is fully lapsed: exp + grace days.
func (c *Claims) GraceUntil() time.Time {
	if c.GraceDays <= 0 {
		return c.ExpiresAt
	}
	return c.ExpiresAt.Add(time.Duration(c.GraceDays) * 24 * time.Hour)
}

// LeaseExpired reports whether the instance missed its check-in window: more
// than MaxOfflineHours since the last renew (which re-stamps iat). False when
// the lease is disabled or the plan is unlimited.
func (c *Claims) LeaseExpired(now time.Time) bool {
	if c.MaxOfflineHours <= 0 || c.Unlimited() || c.IssuedAt.IsZero() {
		return false
	}
	return now.After(c.IssuedAt.Add(time.Duration(c.MaxOfflineHours) * time.Hour))
}

// decodeAndVerifySig verifies the signature and decodes the claims WITHOUT
// checking the validity window. It returns the claims even on an expired token
// (sig valid) so the caller can decide grace policy — the only way to reach a
// non-nil Claims is a cryptographically authentic token.
func decodeAndVerifySig(token, pubKeyHex string) (*Claims, error) {
	pubKeyHex = strings.TrimSpace(pubKeyHex)
	if pubKeyHex == "" {
		return nil, ErrNoPubKey
	}
	pub, err := hex.DecodeString(pubKeyHex)
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("%w: trusted key is not %d-byte ed25519 hex", ErrMalformed, ed25519.PublicKeySize)
	}

	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) != 2 {
		return nil, fmt.Errorf("%w: expected <claims>.<sig>", ErrMalformed)
	}
	claimsBytes, err := enc.DecodeString(parts[0])
	if err != nil {
		return nil, fmt.Errorf("%w: claims not base64url: %v", ErrMalformed, err)
	}
	sig, err := enc.DecodeString(parts[1])
	if err != nil || len(sig) != ed25519.SignatureSize {
		return nil, fmt.Errorf("%w: signature not %d-byte base64url ed25519", ErrMalformed, ed25519.SignatureSize)
	}
	if !ed25519.Verify(ed25519.PublicKey(pub), claimsBytes, sig) {
		return nil, ErrBadSig
	}

	var c Claims
	if err := json.Unmarshal(claimsBytes, &c); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrMalformed, err)
	}
	if c.Version != tokenVersion {
		return nil, fmt.Errorf("%w %d", ErrVersion, c.Version)
	}
	if c.Type != claimType {
		return nil, fmt.Errorf("%w (typ %q)", ErrWrongType, c.Type)
	}
	if c.CustomerID == uuid.Nil {
		return nil, fmt.Errorf("%w: empty customer id", ErrMalformed)
	}
	if c.ExpiresAt.IsZero() {
		return nil, fmt.Errorf("%w: missing expiry", ErrMalformed)
	}
	return &c, nil
}

// Verify checks the signature AND the validity window at `now`, returning the
// decoded claims only when the token is authentic and currently valid.
// ErrExpired is returned (with non-nil claims) so the caller may still read
// the entitlements for a grace decision. The unlimited plan never expires.
func Verify(token, pubKeyHex string, now time.Time) (*Claims, error) {
	c, err := decodeAndVerifySig(token, pubKeyHex)
	if err != nil {
		return nil, err
	}
	if !c.IssuedAt.IsZero() && now.Before(c.IssuedAt.Add(-5*time.Minute)) {
		return c, ErrNotYetValid
	}
	if !c.Unlimited() && now.After(c.ExpiresAt) {
		return c, fmt.Errorf("%w (expired %s)", ErrExpired, c.ExpiresAt.UTC().Format(time.RFC3339))
	}
	return c, nil
}
