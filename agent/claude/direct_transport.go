package claude

import (
	"bytes"
	"encoding/json"
	"fmt"
	"github.com/asteby/metacore-kernel/agent/airuntime"
	"io"
	"net/http"
	"strings"
	"time"
)

// claudeDirectTransport answers OpenAI-compatible chat-completions requests
// in-process by running `claude -p` (see ClaudeDirectRunner). It never touches
// the network, so it is a drop-in for the aby-proxy HTTP endpoint.
type claudeDirectTransport struct {
	runner *ClaudeDirectRunner
	token  string
}

// NewDirectHTTPClient returns an http.Client whose transport runs `claude -p`
// in-process behind the OpenAI chat-completions contract (use with DirectBaseURL).
func NewDirectHTTPClient(rt airuntime.Runtime) *http.Client {
	return &http.Client{Transport: &claudeDirectTransport{
		runner: DefaultClaudeDirectRunner(),
		token:  strings.TrimSpace(rt.ProxyAPIKey),
	}}
}

func jsonResponse(req *http.Request, status int, header http.Header, body []byte) *http.Response {
	h := http.Header{"Content-Type": []string{"application/json"}}
	for k, v := range header {
		h[k] = v
	}
	return &http.Response{
		Status: fmt.Sprintf("%d %s", status, http.StatusText(status)), StatusCode: status,
		Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1,
		Header: h, Body: io.NopCloser(bytes.NewReader(body)),
		ContentLength: int64(len(body)), Request: req,
	}
}

func errResponse(req *http.Request, status int, msg string) *http.Response {
	b, _ := json.Marshal(map[string]any{"error": map[string]any{"message": msg}})
	return jsonResponse(req, status, nil, b)
}

func (t *claudeDirectTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if !strings.HasSuffix(req.URL.Path, "/chat/completions") || req.Method != http.MethodPost {
		return errResponse(req, http.StatusNotFound, "claude direct runner only serves chat/completions"), nil
	}
	var body DirectChatRequest
	if req.Body != nil {
		raw, err := io.ReadAll(req.Body)
		req.Body.Close()
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(raw, &body); err != nil {
			return errResponse(req, http.StatusBadRequest, "invalid JSON body"), nil
		}
	}
	res, err := t.runner.Run(req.Context(), t.token, body)
	if err != nil {
		var de *ClaudeDirectError
		if ctxErr := req.Context().Err(); ctxErr != nil {
			return nil, ctxErr
		}
		if asDirect(err, &de) {
			return errResponse(req, http.StatusServiceUnavailable, de.Msg), nil
		}
		return errResponse(req, http.StatusServiceUnavailable, redact(err.Error(), t.token)), nil
	}

	msg := map[string]any{"role": "assistant", "content": res.Content}
	finish := "stop"
	if len(res.ToolCalls) > 0 {
		msg["tool_calls"] = res.ToolCalls
		finish = "tool_calls"
	}
	model := body.Model
	if model == "" {
		model = "auto"
	}
	promptTokens := estimateTokens(messagesToPrompt(body.Messages))
	callsJSON, _ := json.Marshal(res.ToolCalls)
	completion := estimateTokens(res.Content) + estimateTokens(string(callsJSON))
	id := fmt.Sprintf("chatcmpl-%x", time.Now().UnixNano())
	header := http.Header{"X-Aby-Backend": []string{"claude-direct"}}

	if body.Stream && len(res.ToolCalls) == 0 {
		// Single-chunk SSE: the run is not incremental, but streaming clients
		// get a valid stream.
		chunk := func(delta map[string]any, fr any) string {
			b, _ := json.Marshal(map[string]any{"id": id, "object": "chat.completion.chunk", "created": time.Now().Unix(), "model": model,
				"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": fr}}})
			return "data: " + string(b) + "\n\n"
		}
		sse := chunk(map[string]any{"role": "assistant", "content": res.Content}, nil) + chunk(map[string]any{}, "stop") + "data: [DONE]\n\n"
		header.Set("Content-Type", "text/event-stream")
		return jsonResponse(req, http.StatusOK, header, []byte(sse)), nil
	}

	out, _ := json.Marshal(map[string]any{
		"id": id, "object": "chat.completion", "created": time.Now().Unix(), "model": model,
		"choices": []any{map[string]any{"index": 0, "message": msg, "finish_reason": finish}},
		"usage":   map[string]any{"prompt_tokens": promptTokens, "completion_tokens": completion, "total_tokens": promptTokens + completion},
	})
	return jsonResponse(req, http.StatusOK, header, out), nil
}

func estimateTokens(s string) int {
	if n := len(s) / 4; n > 1 {
		return n
	}
	return 1
}

func asDirect(err error, target **ClaudeDirectError) bool {
	for err != nil {
		if de, ok := err.(*ClaudeDirectError); ok {
			*target = de
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}
