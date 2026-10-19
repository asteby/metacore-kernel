package wasm

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// edge_command host import limits (docs/wasm-abi.md § 25).
const (
	edgeCommandMaxReqBytes      = 64 * 1024
	edgeCommandDefaultTimeout   = 10 * time.Second
	edgeCommandMaxTimeout       = 60 * time.Second
	edgeCommandMaxKeyLen        = 200
	EdgeCommandEnvelopeVersion  = 1
	edgeCommandMaxResponseBytes = 1 << 20
	// Default rate limits (Host.WithEdgeCommandLimits): commands one
	// invocation may send, and commands per (org, addon, device) per minute.
	defaultEdgeCommandsPerInvocation = 50
	defaultEdgeCommandsPerDeviceMin  = 120
	// edgeCommandMinTimeout: below this the hard cap is too close to wait
	// for a device at all.
	edgeCommandMinTimeout = time.Second
)

// Edge command errors the embedder's EdgeCommander returns (wrapped is fine).
var (
	// ErrEdgeDeviceNotFound: no device with that id in the org.
	ErrEdgeDeviceNotFound = errors.New("edge device not found")
	// ErrEdgeOffline: the device's agent has no live session.
	ErrEdgeOffline = errors.New("edge device offline")
	// ErrEdgeTimeout: the agent did not answer in time. The command MAY have
	// run on the device: retry with the same idempotency key.
	ErrEdgeTimeout = errors.New("edge command timed out")
	// ErrEdgeIndeterminate: an earlier delivery of the key never got an
	// answer (timeout, crash): it may have run; never sent again.
	ErrEdgeIndeterminate = errors.New("edge command outcome unknown")
	// ErrEdgeIdempotencyConflict: the key was used with another command.
	ErrEdgeIdempotencyConflict = errors.New("edge command idempotency key reused with another command")
)

// EdgeDevice identifies the device class a command targets: the
// `edge_devices[]` declaration (AddonKey, DeviceKey) of the physical device.
type EdgeDevice struct {
	ID        string
	AddonKey  string
	DeviceKey string
}

// EdgeCommandRequest is what the host asks the embedder to deliver. Org and
// Addon come from the invocation, never from the guest.
type EdgeCommandRequest struct {
	Org      uuid.UUID
	Addon    string
	DeviceID string
	Kind     string
	Payload  json.RawMessage
	// IdempotencyKey, when set, makes the delivery at-most-once per (org,
	// addon, device, key): a repeat returns the recorded result without
	// sending the command again.
	IdempotencyKey string
	Timeout        time.Duration
	// MaxResponseBytes caps the device answer the host stores and returns
	// (the embedder must not record more).
	MaxResponseBytes int
	// Nonce is a fresh random value per delivery. With CommandID it is what a
	// device-side signature would cover (option B: the agent signs
	// command_id + nonce + data); the embedder carries it in the command frame.
	Nonce string
}

// EdgeCommandResult is the agent's answer as the host transported it.
type EdgeCommandResult struct {
	// CommandID is the host's audit id of the delivery.
	CommandID string `json:"command_id"`
	DeviceID  string `json:"device_id"`
	// OK is the agent's success flag; Error its message when !OK.
	OK    bool            `json:"ok"`
	Error string          `json:"error,omitempty"`
	Data  json.RawMessage `json:"data,omitempty"`
	// Replay: an earlier delivery with the same idempotency key.
	Replay     bool      `json:"replay,omitempty"`
	ReceivedAt time.Time `json:"received_at"`
	// Nonce echoes the delivery's nonce (EdgeCommandRequest.Nonce).
	Nonce string `json:"nonce,omitempty"`
}

// EdgeCommander is the embedder port of the edge_command import: Device
// resolves a device of the org (the host checks the grant against its class
// BEFORE anything is sent) and Send delivers the command through the device's
// agent connection, recording it for audit.
type EdgeCommander interface {
	Device(ctx context.Context, org uuid.UUID, deviceID string) (EdgeDevice, error)
	Send(ctx context.Context, req EdgeCommandRequest) (EdgeCommandResult, error)
}

type edgeCommandPayload struct {
	DeviceID       string          `json:"device_id"`
	Kind           string          `json:"kind"`
	Payload        json.RawMessage `json:"payload,omitempty"`
	IdempotencyKey string          `json:"idempotency_key,omitempty"`
	TimeoutMS      int             `json:"timeout_ms,omitempty"`
}

// executeEdgeCommand implements `metacore_host.edge_command`: a guest sends a
// command to a physical device of its organization (a CashDro, a printer…)
// and receives the agent's answer TRANSPORTED BY THE HOST, so it is the
// device's answer and not what a browser relayed. Gated by the
// `edge:command <device_key>.<command_kind>` capability, and only on devices
// of a class (edge_devices[]) the calling addon declares. Never returns an
// error: every failure is a `{success:false}` envelope.
func executeEdgeCommand(ctx context.Context, inv *invocation, reqJSON []byte) []byte {
	start := time.Now()
	addonKey, orgID := "", uuid.Nil
	if inv != nil {
		addonKey, orgID = inv.addonKey, inv.orgID
	}
	fail := func(code, msg string) []byte {
		b, _ := json.Marshal(map[string]any{"success": false, "error": hostErrorBody(code, msg), "meta": edgeCommandMeta(addonKey, orgID, start)})
		return b
	}
	switch {
	case inv == nil:
		return fail("invalid_request", "invocation context missing")
	case orgID == uuid.Nil:
		return fail("no_active_org", "invocation has no bound orgID")
	case len(reqJSON) == 0 || len(reqJSON) > edgeCommandMaxReqBytes:
		return fail("invalid_request", fmt.Sprintf("request must be 1..%d bytes", edgeCommandMaxReqBytes))
	}
	var req edgeCommandPayload
	if err := json.Unmarshal(reqJSON, &req); err != nil {
		return fail("invalid_request", "malformed request JSON: "+err.Error())
	}
	req.DeviceID, req.Kind, req.IdempotencyKey = strings.TrimSpace(req.DeviceID), strings.TrimSpace(req.Kind), strings.TrimSpace(req.IdempotencyKey)
	switch {
	case req.DeviceID == "" || req.Kind == "":
		return fail("invalid_request", "device_id and kind are required")
	case len(req.IdempotencyKey) > edgeCommandMaxKeyLen:
		return fail("invalid_request", fmt.Sprintf("idempotency_key exceeds %d chars", edgeCommandMaxKeyLen))
	case len(req.Payload) > 0 && !json.Valid(req.Payload):
		return fail("invalid_request", "payload must be JSON")
	}
	if inv.edgeCommander == nil {
		return fail("edge_unavailable", "host has no edge commander configured")
	}
	if inv.tx != nil {
		// Waiting for a device (up to the timeout) inside the action's
		// transaction would hold its locks: refused, never silently slow.
		return fail("in_transaction", "edge_command cannot run inside a transactional invocation (InvokeInTx)")
	}
	dev, err := inv.edgeCommander.Device(ctx, orgID, req.DeviceID)
	if err != nil {
		return fail(edgeErrorCode(err), err.Error())
	}
	// Only devices of a class the CALLER declares, and only the commands it
	// was granted: a payment addon cannot drive another addon's printer.
	if dev.AddonKey != addonKey {
		inv.logf("audit edge_command addon=%s device=%s kind=%s org=%s decision=denied: device class of %s", addonKey, req.DeviceID, req.Kind, orgID, dev.AddonKey)
		return fail("forbidden", fmt.Sprintf("device %s is not a device of addon %q", req.DeviceID, addonKey))
	}
	if err := inv.caps.CanSendEdgeCommand(dev.DeviceKey, req.Kind); err != nil {
		inv.logf("audit edge_command addon=%s device=%s kind=%s org=%s decision=denied: %v", addonKey, req.DeviceID, req.Kind, orgID, err)
		return fail("forbidden", err.Error())
	}
	timeout := edgeCommandDefaultTimeout
	if req.TimeoutMS > 0 {
		timeout = time.Duration(req.TimeoutMS) * time.Millisecond
	}
	if timeout > edgeCommandMaxTimeout {
		timeout = edgeCommandMaxTimeout
	}
	// Never past the invocation's hard cap: the answer must come back as a
	// clean timeout (with its audit row), not as a killed guest.
	if left, ok := inv.deadline.hardRemaining(); ok {
		if left-edgeCommandMinTimeout < timeout {
			timeout = left - edgeCommandMinTimeout
		}
		if timeout < edgeCommandMinTimeout {
			return fail("timeout", "not enough invocation time left to wait for the device")
		}
	}
	if !inv.takeEdgeCommand() {
		return fail("rate_limited", fmt.Sprintf("an invocation may send at most %d edge commands", inv.edgeLimits.perInvocation()))
	}
	if !inv.edgeLimits.allow(orgID, addonKey, req.DeviceID, time.Now()) {
		return fail("rate_limited", fmt.Sprintf("device %s: more than %d commands per minute", req.DeviceID, inv.edgeLimits.perDevice()))
	}
	inv.logf("audit edge_command addon=%s device=%s device_key=%s kind=%s org=%s key=%q decision=allowed", addonKey, req.DeviceID, dev.DeviceKey, req.Kind, orgID, req.IdempotencyKey)
	var res EdgeCommandResult
	nonce := newEdgeNonce()
	// The wait on the device does not count against the guest's CPU budget.
	inv.deadline.excluding(func() {
		res, err = inv.edgeCommander.Send(ctx, EdgeCommandRequest{
			Org: orgID, Addon: addonKey, DeviceID: req.DeviceID, Kind: req.Kind, Payload: req.Payload,
			IdempotencyKey: req.IdempotencyKey, Timeout: timeout,
			MaxResponseBytes: edgeCommandMaxResponseBytes, Nonce: nonce,
		})
	})
	if err != nil {
		return fail(edgeErrorCode(err), err.Error())
	}
	if res.Nonce == "" {
		res.Nonce = nonce
	}
	if len(res.Data) > edgeCommandMaxResponseBytes {
		return fail("response_too_large", fmt.Sprintf("device answer exceeds %d bytes", edgeCommandMaxResponseBytes))
	}
	b, _ := json.Marshal(map[string]any{"success": true, "data": res, "meta": edgeCommandMeta(addonKey, orgID, start)})
	return b
}

func edgeErrorCode(err error) string {
	switch {
	case errors.Is(err, ErrEdgeDeviceNotFound):
		return "device_not_found"
	case errors.Is(err, ErrEdgeOffline):
		return "device_offline"
	case errors.Is(err, ErrEdgeTimeout), errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, ErrEdgeIdempotencyConflict):
		return "idempotency_conflict"
	case errors.Is(err, ErrEdgeIndeterminate):
		return "timeout"
	}
	return "edge_error"
}

func edgeCommandMeta(addonKey string, orgID uuid.UUID, start time.Time) map[string]any {
	meta := map[string]any{"addon": addonKey, "durationMs": time.Since(start).Milliseconds(), "envelopeVersion": EdgeCommandEnvelopeVersion}
	if orgID != uuid.Nil {
		meta["orgId"] = orgID.String()
	}
	return meta
}

func newEdgeNonce() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return ""
	}
	return hex.EncodeToString(b[:])
}

// edgeLimiter bounds edge_command per invocation and per (org, addon,
// device) per minute, so a guest (or a storm of event handlers) cannot flood
// a device. In-process: one window per host replica.
type edgeLimiter struct {
	perInv, perMin int
	mu             sync.Mutex
	windows        map[string][]time.Time
}

func newEdgeLimiter(perInvocation, perDevicePerMinute int) *edgeLimiter {
	if perInvocation <= 0 {
		perInvocation = defaultEdgeCommandsPerInvocation
	}
	if perDevicePerMinute <= 0 {
		perDevicePerMinute = defaultEdgeCommandsPerDeviceMin
	}
	return &edgeLimiter{perInv: perInvocation, perMin: perDevicePerMinute, windows: map[string][]time.Time{}}
}

func (l *edgeLimiter) perInvocation() int {
	if l == nil {
		return defaultEdgeCommandsPerInvocation
	}
	return l.perInv
}

func (l *edgeLimiter) perDevice() int {
	if l == nil {
		return defaultEdgeCommandsPerDeviceMin
	}
	return l.perMin
}

// allow records one command for the device and reports whether it fits in
// the last minute's budget.
func (l *edgeLimiter) allow(org uuid.UUID, addon, device string, now time.Time) bool {
	if l == nil {
		return true
	}
	key := org.String() + "|" + addon + "|" + device
	l.mu.Lock()
	defer l.mu.Unlock()
	cut := now.Add(-time.Minute)
	w := l.windows[key][:0]
	for _, t := range l.windows[key] {
		if t.After(cut) {
			w = append(w, t)
		}
	}
	if len(w) >= l.perMin {
		l.windows[key] = w
		return false
	}
	l.windows[key] = append(w, now)
	return true
}

// takeEdgeCommand counts one command against the invocation's budget.
func (i *invocation) takeEdgeCommand() bool {
	i.edgeSent++
	return i.edgeSent <= i.edgeLimits.perInvocation()
}

// WithEdgeCommandLimits overrides the edge_command rate limits (0 keeps the
// default: 50 per invocation, 120 per device per minute).
func (h *Host) WithEdgeCommandLimits(perInvocation, perDevicePerMinute int) *Host {
	h.edgeLimits = newEdgeLimiter(perInvocation, perDevicePerMinute)
	return h
}

// WithEdgeCommander injects the embedder's EdgeCommander for the
// `metacore_host.edge_command` import. Unset = the import answers
// `edge_unavailable`.
func (h *Host) WithEdgeCommander(c EdgeCommander) *Host {
	h.edgeCommander = c
	if h.edgeLimits == nil {
		h.edgeLimits = newEdgeLimiter(0, 0)
	}
	return h
}
