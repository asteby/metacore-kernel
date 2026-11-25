package contract

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// DefaultWindow is the accepted clock difference of a callback (+-300 s).
const DefaultWindow = 300 * time.Second

// DedupeTTL is how long a receiver remembers delivery ids.
const DedupeTTL = 7 * 24 * time.Hour

const signaturePrefix = "sha256="

// Callback verification failures. They are for logging: a receiver answers a
// uniform 401 and never tells the caller the reason.
var (
	ErrNoSecret          = errors.New("support contract: empty callback secret")
	ErrBadSignature      = errors.New("support contract: invalid signature")
	ErrBadTimestamp      = errors.New("support contract: invalid timestamp header")
	ErrTimestampMismatch = errors.New("support contract: body timestamp does not match header")
	ErrOutsideWindow     = errors.New("support contract: timestamp outside window")
	ErrBadBody           = errors.New("support contract: callback body is not a JSON object with delivery_id")
	ErrDeliveryMismatch  = errors.New("support contract: delivery id does not match header")
	ErrDuplicate         = errors.New("support contract: duplicate delivery")
)

// SignBody returns "sha256=<hex HMAC-SHA256(body, secret)>", the value of the
// X-Hub-Signature header. The PHP equivalent is
// 'sha256=' . hash_hmac('sha256', $raw, $secret).
func SignBody(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return signaturePrefix + hex.EncodeToString(mac.Sum(nil))
}

// SignCallback returns the four headers of a callback delivery. body must be
// the exact bytes that will be sent and must carry the same "timestamp"
// (unix seconds of ts) and "delivery_id" as the headers.
func SignCallback(secret string, body []byte, ts time.Time, deliveryID, event string) http.Header {
	h := http.Header{}
	h.Set(HeaderHubSignature, SignBody(secret, body))
	h.Set(HeaderHubTimestamp, strconv.FormatInt(ts.Unix(), 10))
	h.Set(HeaderHubDelivery, deliveryID)
	h.Set(HeaderHubEvent, event)
	return h
}

// CallbackMeta is what VerifyCallback learns from a valid delivery.
type CallbackMeta struct {
	DeliveryID string
	Event      string
	Timestamp  int64
	// Contract is the "contract" field of the body (1 when absent).
	Contract int
}

// VerifyCallback authenticates a callback on the instance side: HMAC over the
// raw body (constant time), X-Hub-Timestamp equal to the signed "timestamp"
// and within +-window of now (window <= 0 means DefaultWindow), and a non-empty
// delivery_id that matches X-Hub-Delivery when that header is present.
// It does not deduplicate; see AcceptCallback.
func VerifyCallback(secret string, body []byte, h http.Header, now time.Time, window time.Duration) (*CallbackMeta, error) {
	if secret == "" {
		return nil, ErrNoSecret
	}
	if window <= 0 {
		window = DefaultWindow
	}
	sig := strings.TrimSpace(h.Get(HeaderHubSignature))
	if !strings.HasPrefix(sig, signaturePrefix) ||
		!hmac.Equal([]byte(SignBody(secret, body)), []byte(sig)) {
		return nil, ErrBadSignature
	}
	ts, err := strconv.ParseInt(strings.TrimSpace(h.Get(HeaderHubTimestamp)), 10, 64)
	if err != nil {
		return nil, ErrBadTimestamp
	}
	var b struct {
		Event      string `json:"event"`
		DeliveryID string `json:"delivery_id"`
		Timestamp  int64  `json:"timestamp"`
		Contract   int    `json:"contract"`
	}
	if err := json.Unmarshal(body, &b); err != nil || b.DeliveryID == "" {
		return nil, ErrBadBody
	}
	if b.Timestamp != ts {
		return nil, ErrTimestampMismatch
	}
	if d := now.Sub(time.Unix(ts, 0)); d > window || d < -window {
		return nil, ErrOutsideWindow
	}
	if hd := strings.TrimSpace(h.Get(HeaderHubDelivery)); hd != "" && hd != b.DeliveryID {
		return nil, ErrDeliveryMismatch
	}
	c := b.Contract
	if c < ContractV1 {
		c = ContractV1
	}
	return &CallbackMeta{DeliveryID: b.DeliveryID, Event: b.Event, Timestamp: ts, Contract: c}, nil
}

// DeliveryDeduper is the port receivers plug to drop repeated deliveries. The
// instance chooses the storage (a table with TTL in OPS, memory in tests).
type DeliveryDeduper interface {
	// FirstSeen records id for ttl and reports whether it had not been seen
	// (or had expired). It must be atomic: of two concurrent calls with the
	// same id exactly one returns true. On error the delivery is not
	// acknowledged and the Hub retries.
	FirstSeen(ctx context.Context, id string, ttl time.Duration) (bool, error)
}

// AcceptCallback is VerifyCallback followed by dedupe. The id is recorded only
// after the signature is valid, so an attacker cannot burn delivery ids. A
// repeated delivery returns ErrDuplicate together with its meta: the receiver
// should answer 2xx without processing it again.
func AcceptCallback(ctx context.Context, dd DeliveryDeduper, secret string, body []byte, h http.Header, now time.Time, window time.Duration) (*CallbackMeta, error) {
	m, err := VerifyCallback(secret, body, h, now, window)
	if err != nil {
		return nil, err
	}
	if dd == nil {
		return m, nil
	}
	first, err := dd.FirstSeen(ctx, m.DeliveryID, DedupeTTL)
	if err != nil {
		return nil, err
	}
	if !first {
		return m, ErrDuplicate
	}
	return m, nil
}

// AckProof is the proof of POST /v1/support/link/ack:
// hex(HMAC-SHA256(secret, instanceID || ts)) where || is plain concatenation
// and ts is the decimal X-Signature-Timestamp of the ack request. It proves the
// instance stored the secret it was given.
func AckProof(secret, instanceID, ts string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(instanceID))
	mac.Write([]byte(ts))
	return hex.EncodeToString(mac.Sum(nil))
}

// VerifyAckProof checks a proof in constant time. An empty secret never
// verifies.
func VerifyAckProof(secret, instanceID, ts, proof string) bool {
	if secret == "" {
		return false
	}
	return hmac.Equal([]byte(AckProof(secret, instanceID, ts)), []byte(strings.TrimSpace(proof)))
}
