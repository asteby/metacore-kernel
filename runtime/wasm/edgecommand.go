package wasm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// edge_command host import limits (docs/wasm-abi.md § 23).
const (
	edgeCommandMaxReqBytes      = 64 * 1024
	edgeCommandDefaultTimeout   = 10 * time.Second
	edgeCommandMaxTimeout       = 60 * time.Second
	edgeCommandMaxKeyLen        = 200
	EdgeCommandEnvelopeVersion  = 1
	edgeCommandMaxResponseBytes = 1 << 20
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
	dev, err := inv.edgeCommander.Device(ctx, orgID, req.DeviceID)
	if err != nil {
		return fail(edgeErrorCode(err), err.Error())
	}
	// Only devices of a class the CALLER declares, and only the commands it
	// was granted: a payment addon cannot drive another addon's printer.
	if dev.AddonKey != addonKey {
		inv.logf("audit edge_command addon=%s device=%s kind=%s org=%s decision=denied: device class of %s", addonKey, req.DeviceID, req.Kind, orgID, dev.AddonKey)
		return fail("forbidden", fmt.Sprintf("device %s belongs to addon %q", req.DeviceID, dev.AddonKey))
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
	inv.logf("audit edge_command addon=%s device=%s device_key=%s kind=%s org=%s key=%q decision=allowed", addonKey, req.DeviceID, dev.DeviceKey, req.Kind, orgID, req.IdempotencyKey)
	var res EdgeCommandResult
	// The wait on the device does not count against the guest's CPU budget.
	inv.deadline.excluding(func() {
		res, err = inv.edgeCommander.Send(ctx, EdgeCommandRequest{
			Org: orgID, Addon: addonKey, DeviceID: req.DeviceID, Kind: req.Kind, Payload: req.Payload,
			IdempotencyKey: req.IdempotencyKey, Timeout: timeout,
		})
	})
	if err != nil {
		return fail(edgeErrorCode(err), err.Error())
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

// WithEdgeCommander injects the embedder's EdgeCommander for the
// `metacore_host.edge_command` import. Unset = the import answers
// `edge_unavailable`.
func (h *Host) WithEdgeCommander(c EdgeCommander) *Host {
	h.edgeCommander = c
	return h
}
