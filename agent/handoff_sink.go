package agent

import (
	"context"
	"fmt"
	"strings"

	"github.com/asteby/metacore-kernel/agent/contextmgr"
	"github.com/asteby/metacore-kernel/support/contract"
)

// Defaults bounding the transcript handed to a HandoffSink.
const (
	DefaultHandoffMaxTurns  = 40
	DefaultHandoffTurnRunes = 2000
)

// HandoffRequest is what the host receives when the agent hands a
// conversation to a human. Transcript reuses the shared support contract turn.
type HandoffRequest struct {
	ConversationID string
	Reason         string
	Severity       string // one of contract.Severities
	Transcript     []contract.Turn
}

// HandoffResult is the sink's answer: a case reference and its state.
type HandoffResult struct {
	Reference string // case or folio reference, "" when the host has none
	Status    string // host-defined state, e.g. "open"
}

// HandoffSink is the optional port a host injects to route a handoff
// somewhere (a support desk, a queue). Without it the tool is unchanged.
type HandoffSink interface {
	Handoff(ctx context.Context, req HandoffRequest) (HandoffResult, error)
}

// HandoffOption configures NewHandoffToHumanTool.
type HandoffOption func(*HandoffToHumanTool)

// WithHandoffSink injects the sink. A nil sink keeps the default behavior.
func WithHandoffSink(s HandoffSink) HandoffOption {
	return func(t *HandoffToHumanTool) { t.sink = s }
}

// WithHandoffDescription replaces the tool description, so the host can tell
// the model who the human is. Empty keeps the default.
func WithHandoffDescription(d string) HandoffOption {
	return func(t *HandoffToHumanTool) { t.description = d }
}

// WithHandoffTranscriptLimits bounds the transcript: the last maxTurns turns,
// each cut to maxRunes runes. Values <= 0 keep the defaults.
func WithHandoffTranscriptLimits(maxTurns, maxRunes int) HandoffOption {
	return func(t *HandoffToHumanTool) {
		if maxTurns > 0 {
			t.maxTurns = maxTurns
		}
		if maxRunes > 0 {
			t.maxRunes = maxRunes
		}
	}
}

// WithHandoffRedactor sets a function applied to every turn's text before it
// is cut and handed to the sink (the host owns the redaction policy).
func WithHandoffRedactor(fn func(string) string) HandoffOption {
	return func(t *HandoffToHumanTool) { t.redact = fn }
}

// dispatch calls the sink once. A cancelled context is returned as an error; a
// sink failure or panic becomes a controlled tool result that does not disable
// the bot and carries no user message, so the conversation continues.
func (t *HandoffToHumanTool) dispatch(ctx context.Context, params map[string]interface{}, agentCtx *contextmgr.ConversationContext, reason string, ok map[string]interface{}) (out interface{}, err error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	req := HandoffRequest{
		Reason:     reason,
		Severity:   contract.NormalizeSeverity(strParam(params, "severity")),
		Transcript: t.transcript(agentCtx),
	}
	if agentCtx != nil {
		req.ConversationID = agentCtx.ConversationID.String()
	}

	var (
		res     HandoffResult
		sinkErr error
	)
	func() {
		defer func() {
			if r := recover(); r != nil {
				sinkErr = fmt.Errorf("handoff sink panic: %v", r)
			}
		}()
		res, sinkErr = t.sink.Handoff(ctx, req)
	}()
	if sinkErr != nil {
		if cerr := ctx.Err(); cerr != nil {
			return nil, cerr
		}
		return map[string]interface{}{
			"status": "handoff_failed",
			"reason": reason,
			"error":  "handoff could not be delivered",
		}, nil
	}
	if res.Reference != "" {
		ok["reference"] = res.Reference
	}
	if res.Status != "" {
		ok["case_status"] = res.Status
	}
	return ok, nil
}

// transcript returns the redacted, bounded tail of the conversation history.
func (t *HandoffToHumanTool) transcript(agentCtx *contextmgr.ConversationContext) []contract.Turn {
	if agentCtx == nil {
		return nil
	}
	hist := agentCtx.History
	var turns []contract.Turn
	for _, m := range hist {
		text := strings.TrimSpace(m.Content)
		if text == "" {
			continue
		}
		if t.redact != nil {
			text = t.redact(text)
		}
		turns = append(turns, contract.Turn{Role: m.Role, Text: TruncateRunes(text, t.maxRunes)})
	}
	if len(turns) > t.maxTurns {
		turns = turns[len(turns)-t.maxTurns:]
	}
	return turns
}
