package dispatch

import (
	"encoding/json"
	"strconv"
	"time"
)

// EnvelopeSchema names the wire format of the event identity block the
// dispatcher stamps into every JSON-object payload it delivers.
const EnvelopeSchema = "metacore.event/1"

// EnvelopeField is the top-level payload key that carries the Envelope.
// Host-authored: a value the emitter wrote under this key is replaced.
//
// The envelope is ADDITIVE. The payload a subscriber received before
// metacore.event/1 (the canonical CRUD fields, or the guest's own object plus
// `event`, `actor_id`, `branch_id`, `emitter`) is delivered unchanged; the
// identity block rides next to it, so existing handlers keep working and new
// ones read `envelope.sequence`, `envelope.aggregate`, ... when they care.
const EnvelopeField = "envelope"

// Aggregate names the entity an event is about.
type Aggregate struct {
	Type string `json:"type"`
	ID   string `json:"id"`
}

// Envelope is the metacore.event/1 identity block. The host stamps every
// field; an emitter only chooses the event name, its payload and (optionally)
// the aggregate it concerns.
type Envelope struct {
	Schema string `json:"schema"`
	// ID is the publication id. It is stable across a relay re-publish and it
	// is what the delivery ledger's idempotency key is derived from.
	ID      string `json:"id"`
	Name    string `json:"name"`
	Version int    `json:"version"`
	// Source is the publisher as the BUS saw it: an addon key or "kernel".
	Source     string `json:"source"`
	OrgID      string `json:"org_id"`
	OccurredAt string `json:"occurred_at"`
	// Aggregate / Sequence order events about one entity: Sequence is a
	// per-aggregate monotonic counter, so a consumer can discard a stale
	// event by comparing it with the last one it applied.
	Aggregate *Aggregate `json:"aggregate,omitempty"`
	Sequence  int64      `json:"sequence,omitempty"`
	// CorrelationID ties every event of one user request together;
	// CausationID names the single event whose delivery produced this one.
	CorrelationID string `json:"correlation_id,omitempty"`
	CausationID   string `json:"causation_id,omitempty"`
	ActorID       string `json:"actor_id,omitempty"`
	BranchID      string `json:"branch_id,omitempty"`
	// Generation is 0 for the original delivery and N for the Nth Replay.
	Generation int `json:"generation,omitempty"`
}

// stampEnvelope sets the top-level EnvelopeField of a JSON-object payload,
// replacing whatever was there. Non-object payloads pass through untouched
// (the identity still rides on the ledger row and the delivery ctx).
func stampEnvelope(raw []byte, env Envelope) []byte {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil || obj == nil {
		return raw
	}
	b, err := json.Marshal(env)
	if err != nil {
		return raw
	}
	obj[EnvelopeField] = b
	out, err := json.Marshal(obj)
	if err != nil {
		return raw
	}
	return out
}

// withGeneration rewrites envelope.generation of an already-stamped payload
// (a Replay re-delivers the persisted bytes with a higher generation).
func withGeneration(raw []byte, gen int) []byte {
	if gen <= 0 {
		return raw
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil || obj == nil {
		return raw
	}
	var env map[string]json.RawMessage
	if err := json.Unmarshal(obj[EnvelopeField], &env); err != nil || env == nil {
		return raw
	}
	env["generation"] = json.RawMessage(strconv.Itoa(gen))
	eb, err := json.Marshal(env)
	if err != nil {
		return raw
	}
	obj[EnvelopeField] = eb
	out, err := json.Marshal(obj)
	if err != nil {
		return raw
	}
	return out
}

func rfc3339(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }
