package guest

import (
	"encoding/json"
	"fmt"
)

// AI completion from a wasm backend: the host-implemented capability
// `ai.complete` reached through the `capability_query` import. The addon
// declares `ai:complete <feature>` (with its reason) in the manifest; the host
// bills the org's Pulse Sparks and applies the addon's caps. The guest never
// holds LLM keys and never names the org or the addon: the host stamps both
// from the invocation. Reference: docs/capabilities-ai.md.
//
// The types mirror capability.AIRequest / capability.AIResult field by field
// (TinyGo guests do not import the host packages); a test keeps them in sync.

// AIMessage is one conversation turn: role system | user | assistant.
type AIMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// AIResponseFormat asks for a structured answer: text | json_object |
// json_schema (with Schema).
type AIResponseFormat struct {
	Type   string          `json:"type"`
	Name   string          `json:"name,omitempty"`
	Schema json.RawMessage `json:"schema,omitempty"`
}

// AIRequest is the ai.complete input. Feature must be granted by the
// manifest; send Messages or Prompt.
type AIRequest struct {
	Feature        string            `json:"feature"`
	Messages       []AIMessage       `json:"messages,omitempty"`
	Prompt         string            `json:"prompt,omitempty"`
	System         string            `json:"system,omitempty"`
	Tier           string            `json:"tier,omitempty"`
	ResponseFormat *AIResponseFormat `json:"response_format,omitempty"`
	MaxTokens      int               `json:"max_tokens,omitempty"`
	Temperature    *float64          `json:"temperature,omitempty"`
	// IdempotencyKey: same key, same result, charged once. Derive it from
	// what you process (ticket:<id>:triage:<version>) so a retried
	// invocation does not pay twice.
	IdempotencyKey string `json:"idempotency_key,omitempty"`
}

// AIUsage is the token count the hub reports.
type AIUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// AIResult is the ai.complete output. Status "ok" carries Content (and JSON
// for a JSON response_format); any other status — insufficient_pulse,
// quota_exceeded, budget_exceeded, rate_limited, disabled, no_wallet,
// unavailable, invalid_output — is a NORMAL outcome: keep working without AI.
type AIResult struct {
	Status            string          `json:"status"`
	Content           string          `json:"content,omitempty"`
	JSON              json.RawMessage `json:"json,omitempty"`
	Usage             AIUsage         `json:"usage"`
	CostSparks        int64           `json:"cost_sparks"`
	Model             string          `json:"model,omitempty"`
	RequestID         string          `json:"request_id,omitempty"`
	Message           string          `json:"message,omitempty"`
	RetryAfterSeconds int             `json:"retry_after_seconds,omitempty"`
	BilledTo          string          `json:"billed_to,omitempty"`
	Replay            bool            `json:"replay,omitempty"`
}

// OK reports whether the result carries an answer.
func (r AIResult) OK() bool { return r.Status == "ok" }

// AIError is a caller bug AIComplete reports as an error: forbidden (the
// manifest does not grant the feature, or lacks ai:complete) or
// validation_error / invalid_input (the request breaks the contract).
// Balance and availability problems are never an AIError.
type AIError struct {
	Code    string
	Message string
}

func (e *AIError) Error() string {
	if e == nil {
		return "guest: <nil ai error>"
	}
	return fmt.Sprintf("ai.complete: %s: %s", e.Code, e.Message)
}

// aiCapability is the host-implemented capability key.
const aiCapability = "ai.complete"

// encodeAIQuery builds the capability_query request for req.
func encodeAIQuery(req AIRequest) ([]byte, error) {
	in, err := json.Marshal(req)
	if err != nil {
		return nil, &AIError{Code: "invalid_input", Message: "guest: marshal request: " + err.Error()}
	}
	return json.Marshal(struct {
		Capability string          `json:"capability"`
		Input      json.RawMessage `json:"input"`
	}{aiCapability, in})
}

type aiQueryEnvelope struct {
	Success bool `json:"success"`
	Data    *struct {
		Results []struct {
			Provider string          `json:"provider"`
			Output   json.RawMessage `json:"output"`
		} `json:"results"`
		Errors []struct {
			Provider string `json:"provider"`
			Code     string `json:"code"`
			Message  string `json:"message"`
		} `json:"errors"`
	} `json:"data,omitempty"`
	Error *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// aiCallerBug are the codes that mean "fix the addon", not "no AI now".
var aiCallerBug = map[string]bool{"forbidden": true, "validation_error": true, "invalid_input": true, "invalid_request": true}

// decodeAIEnvelope turns the capability_query envelope into an AIResult. A
// host without AI (no querier, no provider) is an "unavailable" result, not
// an error, so a guest built today keeps working on any host.
func decodeAIEnvelope(buf []byte) (AIResult, error) {
	if len(buf) == 0 {
		return AIResult{Status: "unavailable", Message: "empty host answer"}, nil
	}
	var env aiQueryEnvelope
	if err := json.Unmarshal(buf, &env); err != nil {
		return AIResult{}, fmt.Errorf("guest: decode capability_query envelope: %w", err)
	}
	fail := func(code, msg string) (AIResult, error) {
		if aiCallerBug[code] {
			return AIResult{}, &AIError{Code: code, Message: msg}
		}
		return AIResult{Status: "unavailable", Message: msg}, nil
	}
	if !env.Success {
		if env.Error == nil {
			return fail("unknown", "capability_query failed")
		}
		return fail(env.Error.Code, env.Error.Message)
	}
	if env.Data == nil {
		return fail("unknown", "capability_query answered no data")
	}
	if len(env.Data.Results) > 0 {
		var res AIResult
		if err := json.Unmarshal(env.Data.Results[0].Output, &res); err != nil {
			return AIResult{}, fmt.Errorf("guest: decode ai.complete output: %w", err)
		}
		return res, nil
	}
	if len(env.Data.Errors) > 0 {
		return fail(env.Data.Errors[0].Code, env.Data.Errors[0].Message)
	}
	return fail("unknown", "ai.complete answered nothing")
}
