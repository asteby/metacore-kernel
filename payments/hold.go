package payments

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

// A charge hold says "this document is being charged at X". It closes the
// window between a point of sale STARTING a physical charge (bills going into
// a cash recycler, a card on a terminal) and the payment being recorded: no
// other point of sale (cash desk, kiosk, storefront, another register) may
// charge the same document meanwhile. It lives at document level, has a TTL
// so a point of sale that dies mid-charge does not lock the document forever,
// and is identified by a token: only its owner renews, releases or charges
// under it.
//
// Atomicity is the store's job: one live hold per (org, document_type,
// document_id), enforced by a unique (partial) index or an advisory lock, and
// Acquire evaluated inside that serialization.

// HoldStatus is the lifecycle of a hold.
type HoldStatus string

const (
	HoldActive   HoldStatus = "active"
	HoldReleased HoldStatus = "released"
	// HoldConsumed: the charge it protected was recorded.
	HoldConsumed HoldStatus = "consumed"
	HoldExpired  HoldStatus = "expired"
)

// Hold TTL bounds. The default is what in-person charges need (a cash
// recycler operation, a terminal order of up to ~10 minutes is renewed).
const (
	DefaultHoldTTL = 300 * time.Second
	MinHoldTTL     = 30 * time.Second
	MaxHoldTTL     = time.Hour
)

// ClampHoldTTL bounds a configured TTL; zero or negative = DefaultHoldTTL.
func ClampHoldTTL(d time.Duration) time.Duration {
	switch {
	case d <= 0:
		return DefaultHoldTTL
	case d < MinHoldTTL:
		return MinHoldTTL
	case d > MaxHoldTTL:
		return MaxHoldTTL
	}
	return d
}

// MinHoldTokenLen is the shortest token Acquire accepts (NewHoldToken makes
// 64 hex characters: 256 bits).
const MinHoldTokenLen = 32

// NewHoldToken returns an unguessable hold token from crypto/rand.
func NewHoldToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic("payments: crypto/rand failed: " + err.Error())
	}
	return hex.EncodeToString(b)
}

// ErrHoldExpired: the hold is no longer active (expired, released, consumed).
var ErrHoldExpired = errors.New("payments: hold is no longer active")

// Hold is a document-level charge reservation.
type Hold struct {
	DocumentType string `json:"document_type"`
	DocumentID   string `json:"document_id"`
	// Token is secret to the owner (returned once by Acquire).
	Token string `json:"-"`
	// Holder names the point of sale ("pos:<register>", "kiosk:<station>",
	// "storefront:<checkout>") for the "being charged at X" message.
	Holder    string     `json:"holder"`
	Channel   Channel    `json:"channel,omitempty"`
	UserID    string     `json:"user_id,omitempty"`
	Status    HoldStatus `json:"status"`
	ExpiresAt time.Time  `json:"expires_at"`
}

// Live reports whether the hold still blocks other points of sale.
func (h Hold) Live(now time.Time) bool { return h.Status == HoldActive && now.Before(h.ExpiresAt) }

// Owns reports whether token is the hold's token (constant time).
func (h Hold) Owns(token string) bool {
	return token != "" && subtle.ConstantTimeCompare([]byte(h.Token), []byte(token)) == 1
}

// Hold errors.
var (
	// ErrHeld: another point of sale holds the document (HTTP 409, code
	// in_payment). The error is a *HeldError naming who and until when.
	ErrHeld = errors.New("payments: document is being charged elsewhere")
	// ErrHoldNotOwner: the token does not own the hold.
	ErrHoldNotOwner = errors.New("payments: hold token does not own the hold")
)

// HeldError carries the live hold that blocked the caller (without its token).
type HeldError struct {
	Holder    string
	ExpiresAt time.Time
}

func (e *HeldError) Error() string {
	return fmt.Sprintf("%v: held by %s until %s", ErrHeld, e.Holder, e.ExpiresAt.UTC().Format(time.RFC3339))
}

// Unwrap makes errors.Is(err, ErrHeld) hold.
func (e *HeldError) Unwrap() error { return ErrHeld }

// HoldRequest asks for a hold.
type HoldRequest struct {
	DocumentType, DocumentID string
	Holder                   string
	Channel                  Channel
	UserID                   string
	// Token renews the caller's own hold; empty asks for a new one.
	Token string
	TTL   time.Duration
}

// Acquire takes or renews the hold of a document given the current one (nil
// = none). The owner presenting its token renews it; a live hold of someone
// else is a *HeldError; an expired or finished hold is replaced. newToken
// returns the new token; nil = NewHoldToken. A token shorter than
// MinHoldTokenLen is refused.
func Acquire(current *Hold, req HoldRequest, now time.Time, newToken func() string) (Hold, error) {
	if strings.TrimSpace(req.DocumentType) == "" || strings.TrimSpace(req.DocumentID) == "" || strings.TrimSpace(req.Holder) == "" {
		return Hold{}, errors.New("payments: hold needs document_type, document_id and holder")
	}
	ttl := ClampHoldTTL(req.TTL)
	if current != nil && current.Live(now) {
		if current.Owns(req.Token) {
			h := *current
			h.ExpiresAt = now.Add(ttl)
			return h, nil
		}
		return Hold{}, &HeldError{Holder: current.Holder, ExpiresAt: current.ExpiresAt}
	}
	if newToken == nil {
		newToken = NewHoldToken
	}
	token := newToken()
	if len(strings.TrimSpace(token)) < MinHoldTokenLen {
		return Hold{}, fmt.Errorf("payments: hold token shorter than %d characters", MinHoldTokenLen)
	}
	return Hold{
		DocumentType: req.DocumentType, DocumentID: req.DocumentID, Token: token,
		Holder: req.Holder, Channel: req.Channel, UserID: req.UserID,
		Status: HoldActive, ExpiresAt: now.Add(ttl),
	}, nil
}

// CheckCharge must pass before a charge of the document is recorded: no live
// hold, or the caller presents its token.
func CheckCharge(current *Hold, token string, now time.Time) error {
	if current == nil || !current.Live(now) || current.Owns(token) {
		return nil
	}
	return &HeldError{Holder: current.Holder, ExpiresAt: current.ExpiresAt}
}

// Release ends a hold. Only the owner releases it, unless force (the document
// got paid: any hold on it is moot). Releasing nothing is not an error.
func Release(current *Hold, token string, force bool, now time.Time) (*Hold, error) {
	if current == nil || current.Status != HoldActive {
		return current, nil
	}
	h := *current
	switch {
	case !h.Live(now):
		h.Status = HoldExpired
	case force || h.Owns(token):
		h.Status = HoldReleased
	default:
		return current, ErrHoldNotOwner
	}
	return &h, nil
}

// Consume marks the hold as used by the recorded charge. Only the owner
// consumes, and only a hold still active and live: a stale copy of a hold
// that expired (another point of sale may hold the document now) or was
// released fails with ErrHoldExpired, so the overlap is detected.
func Consume(current *Hold, token string, now time.Time) (*Hold, error) {
	if current == nil {
		return nil, nil
	}
	if !current.Owns(token) {
		return current, ErrHoldNotOwner
	}
	if !current.Live(now) {
		return current, ErrHoldExpired
	}
	h := *current
	h.Status = HoldConsumed
	return &h, nil
}
