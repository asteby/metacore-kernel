package sigv2

// statuslist.go: signed state lists (revocations) with a sequence number and an
// expiry, and a Tracker that enforces monotonicity and applies a configurable
// policy when no fresh list is available.
//
// The pre-existing marketplace revocation list carries neither a sequence nor
// an expiry (verified: only generated_at + signature), so a stale or frozen
// list cannot be told apart from a current one. This list can.

import (
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// Kinds of revoked subject.
const (
	KindLicense = "license" // ID = license id (lid)
	KindAddon   = "addon"   // ID = addon key, Version = version or "*"
	KindKey     = "key"     // ID = kid
	KindRelease = "release" // ID = release id, Version = version
)

// RevokedItem is one revocation.
type RevokedItem struct {
	Kind    string `json:"kind"`
	ID      string `json:"id"`
	Version string `json:"version,omitempty"`
	Reason  string `json:"reason,omitempty"`
}

// StatusDoc is the payload of a revocation list.
type StatusDoc struct {
	// Seq is monotonic: it grows whenever the content changes. The same Seq may
	// be re-issued with a later ValidUntil to refresh freshness.
	Seq        uint64        `json:"seq"`
	IssuedAt   time.Time     `json:"issued_at"`
	ValidUntil time.Time     `json:"valid_until"`
	Revoked    []RevokedItem `json:"revoked"`
}

var (
	ErrStatusExpired  = errors.New("sigv2: revocation list has expired")
	ErrStatusRollback = errors.New("sigv2: revocation list is older than the last one accepted")
	ErrStatusMalform  = errors.New("sigv2: revocation list is malformed")
)

// SealStatus signs a revocation list under DomRevocations.
func SealStatus(priv ed25519.PrivateKey, kid string, doc StatusDoc) (string, error) {
	if doc.Revoked == nil {
		doc.Revoked = []RevokedItem{}
	}
	b, err := json.Marshal(doc)
	if err != nil {
		return "", err
	}
	return Seal(priv, kid, DomRevocations, b)
}

func openStatus(token string, keys Keyring, now time.Time) (*StatusDoc, error) {
	// A revocation list has no legacy form: v2 is always required.
	o, err := Open(token, DomRevocations, keys, Policy{RequireV2: true}, now)
	if err != nil {
		return nil, err
	}
	var d StatusDoc
	if err := json.Unmarshal(o.Payload, &d); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrStatusMalform, err)
	}
	if d.Seq == 0 || d.IssuedAt.IsZero() || d.ValidUntil.IsZero() || !d.ValidUntil.After(d.IssuedAt) {
		return nil, fmt.Errorf("%w: seq/issued_at/valid_until", ErrStatusMalform)
	}
	if d.IssuedAt.After(now.Add(5 * time.Minute)) {
		return nil, fmt.Errorf("%w: issued in the future", ErrStatusMalform)
	}
	return &d, nil
}

// VerifyStatus verifies a list: signature, domain, shape and expiry. Seq
// monotonicity is the Tracker's job (it needs memory).
func VerifyStatus(token string, keys Keyring, now time.Time) (*StatusDoc, error) {
	d, err := openStatus(token, keys, now)
	if err != nil {
		return nil, err
	}
	if now.After(d.ValidUntil) {
		return d, fmt.Errorf("%w (valid until %s)", ErrStatusExpired, d.ValidUntil.UTC().Format(time.RFC3339))
	}
	return d, nil
}

// StalePolicy says what to do when there is no fresh list.
type StalePolicy string

const (
	// PolicyWarn keeps operating and reports a warning. THE DEFAULT.
	PolicyWarn StalePolicy = "warn"
	// PolicyReadOnly keeps reads and blocks writes.
	PolicyReadOnly StalePolicy = "readonly"
	// PolicyBlock stops operating (strictest).
	PolicyBlock StalePolicy = "block"
	// PolicyIgnore keeps operating silently.
	PolicyIgnore StalePolicy = "ignore"
)

// ParseStalePolicy parses a configuration value; anything unknown or empty is
// the default (PolicyWarn): a typo must never lock an instance out.
func ParseStalePolicy(s string) StalePolicy {
	switch StalePolicy(strings.ToLower(strings.TrimSpace(s))) {
	case PolicyReadOnly:
		return PolicyReadOnly
	case PolicyBlock:
		return PolicyBlock
	case PolicyIgnore:
		return PolicyIgnore
	}
	return PolicyWarn
}

// DefaultOfflineMargin is how long past valid_until a list is still treated as
// fresh before the stale policy applies (configurable; see TrackerConfig).
const DefaultOfflineMargin = 7 * 24 * time.Hour

// Action is the verdict for the host.
type Action string

const (
	ActionAllow    Action = "allow"
	ActionWarn     Action = "warn"
	ActionReadOnly Action = "readonly"
	ActionBlock    Action = "block"
)

// Verdict is the result of Evaluate.
type Verdict struct {
	// Fresh is true while a verified list is within valid_until + margin.
	Fresh  bool
	Action Action
	// Reason is human-readable, empty when Fresh.
	Reason string
}

// TrackerConfig configures a Tracker.
type TrackerConfig struct {
	Keys   Keyring
	Policy StalePolicy   // zero → PolicyWarn
	Margin time.Duration // zero → DefaultOfflineMargin; negative → 0
	Now    func() time.Time
}

// Tracker remembers the last accepted list, refuses older ones and answers
// "is X revoked" and "do we have a fresh list". Safe for concurrent use. A
// revocation already known is enforced even when the list has gone stale
// (fail-safe): staleness only changes the verdict, never forgets.
type Tracker struct {
	cfg   TrackerConfig
	mu    sync.RWMutex
	doc   *StatusDoc
	token string
}

// NewTracker builds a Tracker.
func NewTracker(cfg TrackerConfig) *Tracker {
	if cfg.Policy == "" {
		cfg.Policy = PolicyWarn
	}
	if cfg.Margin == 0 {
		cfg.Margin = DefaultOfflineMargin
	} else if cfg.Margin < 0 {
		cfg.Margin = 0
	}
	if cfg.Now == nil {
		cfg.Now = func() time.Time { return time.Now().UTC() }
	}
	return &Tracker{cfg: cfg}
}

// Offer verifies and accepts a list. It rejects an expired list
// (ErrStatusExpired) and one that is older than what was already accepted
// (ErrStatusRollback): a lower seq, or the same seq with an earlier valid_until.
func (t *Tracker) Offer(token string) error {
	now := t.cfg.Now()
	d, err := VerifyStatus(token, t.cfg.Keys, now)
	if err != nil {
		return err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.doc != nil {
		if d.Seq < t.doc.Seq || (d.Seq == t.doc.Seq && d.ValidUntil.Before(t.doc.ValidUntil)) {
			return fmt.Errorf("%w (have seq %d, got %d)", ErrStatusRollback, t.doc.Seq, d.Seq)
		}
	}
	t.doc, t.token = d, strings.TrimSpace(token)
	return nil
}

// Snapshot returns the last accepted list token, for the host to persist.
func (t *Tracker) Snapshot() string {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.token
}

// Restore loads a previously persisted list at start-up. The signature is
// verified, but expiry is NOT checked (an old list is still evidence of past
// revocations and it pins the seq floor against rollback).
func (t *Tracker) Restore(token string) error {
	d, err := openStatus(token, t.cfg.Keys, t.cfg.Now())
	if err != nil {
		return err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.doc, t.token = d, strings.TrimSpace(token)
	return nil
}

// Seq is the last accepted sequence number (0 = none).
func (t *Tracker) Seq() uint64 {
	t.mu.RLock()
	defer t.mu.RUnlock()
	if t.doc == nil {
		return 0
	}
	return t.doc.Seq
}

// IsRevoked reports whether (kind, id, version) is on the last accepted list.
// A list entry with Version "" or "*" covers every version.
func (t *Tracker) IsRevoked(kind, id, version string) bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	if t.doc == nil {
		return false
	}
	for _, r := range t.doc.Revoked {
		if r.Kind != kind || r.ID != id {
			continue
		}
		if r.Version == "" || r.Version == "*" || r.Version == version {
			return true
		}
	}
	return false
}

// Evaluate applies the stale policy at now.
func (t *Tracker) Evaluate(now time.Time) Verdict {
	t.mu.RLock()
	d := t.doc
	t.mu.RUnlock()
	if d != nil && !now.After(d.ValidUntil.Add(t.cfg.Margin)) {
		return Verdict{Fresh: true, Action: ActionAllow}
	}
	reason := "no revocation list has been obtained yet"
	if d != nil {
		reason = fmt.Sprintf("revocation list (seq %d) expired %s", d.Seq, d.ValidUntil.UTC().Format(time.RFC3339))
	}
	switch t.cfg.Policy {
	case PolicyBlock:
		return Verdict{Action: ActionBlock, Reason: reason}
	case PolicyReadOnly:
		return Verdict{Action: ActionReadOnly, Reason: reason}
	case PolicyIgnore:
		return Verdict{Action: ActionAllow, Reason: reason}
	}
	return Verdict{Action: ActionWarn, Reason: reason}
}
