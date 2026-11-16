package agent

import (
	"context"
	"encoding/json"
	"log/slog"
	"math"
	"unicode/utf8"

	"github.com/asteby/metacore-kernel/agent/contextmgr"
	"github.com/google/uuid"
)

// charsPerToken is the rough characters-per-token ratio of Spanish prose and
// JSON for current tokenizers. Estimates are for budgets and telemetry, not
// billing.
const charsPerToken = 3.5

// EstimateTokens approximates the tokens of s (characters / 3.5, rounded up).
func EstimateTokens(s string) int {
	if s == "" {
		return 0
	}
	return int(math.Ceil(float64(utf8.RuneCountInString(s)) / charsPerToken))
}

// PromptBudget is the approximate size, in estimated tokens, of one LLM call
// split by section. It carries sizes only — never prompt or message content.
type PromptBudget struct {
	ConversationID uuid.UUID `json:"conversation_id"`
	// Iteration is the loop iteration of the call (1-based).
	Iteration int `json:"iteration"`
	// System is the system prompt.
	System int `json:"system_tokens"`
	// Tools is the JSON of the native tool definitions (0 when the call
	// offers no tools); ToolCount is how many were offered.
	Tools     int `json:"tools_tokens"`
	ToolCount int `json:"tool_count"`
	// History is every non-tool message, including the current user message,
	// loop nudges and the assistant's tool calls; HistoryMessages counts them.
	History         int `json:"history_tokens"`
	HistoryMessages int `json:"history_messages"`
	// TurnContext is the per-turn block attached to the user message.
	TurnContext int `json:"turn_context_tokens"`
	// ToolResults is the tool output fed back during the turn.
	ToolResults int `json:"tool_result_tokens"`
	// Total is the sum of the sections.
	Total int `json:"total_tokens"`
}

// promptBudgeter measures each call of a turn; the system prompt and tool
// definitions are measured once.
type promptBudgeter struct {
	system    int
	tools     int
	toolCount int
}

func newPromptBudgeter(systemPrompt string, toolDefs []map[string]interface{}) *promptBudgeter {
	b := &promptBudgeter{system: EstimateTokens(systemPrompt), toolCount: len(toolDefs)}
	if len(toolDefs) > 0 {
		if raw, err := json.Marshal(toolDefs); err == nil {
			b.tools = EstimateTokens(string(raw))
		}
	}
	return b
}

// measure sizes one call. messages are the loop's messages (without the turn
// context, which is measured on its own).
func (b *promptBudgeter) measure(iteration int, messages []map[string]interface{}, defs []map[string]interface{}, turnContext string) PromptBudget {
	pb := PromptBudget{Iteration: iteration, System: b.system, TurnContext: EstimateTokens(turnContext)}
	if len(defs) > 0 {
		pb.Tools, pb.ToolCount = b.tools, b.toolCount
	}
	for _, m := range messages {
		n := estimateMessageTokens(m)
		if m["role"] == "tool" {
			pb.ToolResults += n
			continue
		}
		pb.History += n
		pb.HistoryMessages++
	}
	pb.Total = pb.System + pb.Tools + pb.History + pb.TurnContext + pb.ToolResults
	return pb
}

// estimateMessageTokens sizes a message's text and tool calls (images are
// not counted).
func estimateMessageTokens(m map[string]interface{}) int {
	n := 0
	if c, ok := m["content"].(string); ok {
		n += EstimateTokens(c)
	}
	if tc, ok := m["tool_calls"]; ok && tc != nil {
		if raw, err := json.Marshal(tc); err == nil {
			n += EstimateTokens(string(raw))
		}
	}
	return n
}

// reportBudget logs the call's budget in one structured line and hands it to
// the host hook.
func (o *AgentOrchestrator) reportBudget(ctx context.Context, convCtx *contextmgr.ConversationContext, pb PromptBudget) {
	if convCtx != nil {
		pb.ConversationID = convCtx.ConversationID
	}
	slog.Info("agent prompt budget",
		"conversation_id", pb.ConversationID.String(),
		"iteration", pb.Iteration,
		"system_tokens", pb.System,
		"tools_tokens", pb.Tools,
		"tool_count", pb.ToolCount,
		"history_tokens", pb.History,
		"history_messages", pb.HistoryMessages,
		"turn_context_tokens", pb.TurnContext,
		"tool_result_tokens", pb.ToolResults,
		"total_tokens", pb.Total,
	)
	if o.onBudget != nil {
		o.onBudget(ctx, pb)
	}
}

// withTurnContext returns messages with the turn context appended to the
// message at idx. The loop's own slice and maps are not modified, so the
// block is sent with every call of the turn but never stored in history.
func withTurnContext(messages []map[string]interface{}, idx int, turnContext string) []map[string]interface{} {
	if turnContext == "" || idx < 0 || idx >= len(messages) {
		return messages
	}
	out := make([]map[string]interface{}, len(messages))
	copy(out, messages)
	msg := make(map[string]interface{}, len(messages[idx]))
	for k, v := range messages[idx] {
		msg[k] = v
	}
	content, _ := msg["content"].(string)
	msg["content"] = content + "\n\n" + turnContext
	out[idx] = msg
	return out
}

// trimHistoryToBudget keeps the newest messages whose estimated tokens fit
// budget. The last message (the current user message) is always kept, and
// the kept history never starts with an assistant reply (providers expect the
// conversation to open with the user).
func trimHistoryToBudget(messages []map[string]interface{}, budget int) []map[string]interface{} {
	if budget <= 0 || len(messages) <= 1 {
		return messages
	}
	last := len(messages) - 1
	used := estimateMessageTokens(messages[last])
	start := last
	for i := last - 1; i >= 0; i-- {
		n := estimateMessageTokens(messages[i])
		if used+n > budget {
			break
		}
		used += n
		start = i
	}
	for start < last && messages[start]["role"] != "user" {
		start++
	}
	return messages[start:]
}

// resultCap is the tool result cap of a turn: its override when set (negative
// disables the cap), else the orchestrator policy.
func (o *AgentOrchestrator) resultCap(override int) int {
	switch {
	case override < 0:
		return 0
	case override > 0:
		return override
	}
	return o.maxResultBytes()
}
