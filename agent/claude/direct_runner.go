package claude

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/asteby/metacore-kernel/agent/airuntime"
)

// Direct runner: ops talks to `claude -p` in-process instead of going through
// the external aby-proxy. It is exposed as an http.RoundTripper that speaks the
// same OpenAI chat-completions contract the proxy does, so the agent loop
// (langchaingo openai client, see AIService.createLLM) is untouched.
//
// Ported from the production proxy (run_claude / run_claude_tools /
// parse_claude_tool_stdout / messages_to_prompt). Deliberate differences, all
// for self-host safety: the plain-chat path also runs with Claude Code's own
// tools disabled (the proxy runs a full coding agent with
// --dangerously-skip-permissions), and the prompt always goes through stdin.

const (
	// RunnerProxy: HTTP to aby-proxy (or any OpenAI-compatible endpoint).
	RunnerProxy = "proxy"
	// RunnerDirect: `claude -p` executed by ops itself.
	RunnerDirect = "direct"

	abyProxyDirectValue = "direct" // ABY_PROXY_URL=direct forces the direct runner
	claudeDirectBaseURL = "http://claude-direct.invalid/v1"
)

// ---- runner selection ----

var (
	// claudeProxyProbe reports whether an aby-proxy answers at base. Injected
	// by tests.
	claudeProxyProbe = func(base string) bool {
		root := strings.TrimSuffix(strings.TrimRight(base, "/"), "/v1")
		ctx, cancel := context.WithTimeout(context.Background(), 800*time.Millisecond)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, root+"/health", nil)
		if err != nil {
			return false
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return false
		}
		resp.Body.Close()
		return resp.StatusCode < 500
	}
	claudeBinaryProbe = func() bool { return BinaryPath() != "" }

	probeMu    sync.Mutex
	probeCache = map[string]probeEntry{}
)

const claudeProbeTTL = 30 * time.Second

type probeEntry struct {
	alive bool
	at    time.Time
}

func cachedProxyAlive(base string) bool {
	probeMu.Lock()
	if e, ok := probeCache[base]; ok && time.Since(e.at) < claudeProbeTTL {
		probeMu.Unlock()
		return e.alive
	}
	probeMu.Unlock()
	alive := claudeProxyProbe(base)
	probeMu.Lock()
	probeCache[base] = probeEntry{alive: alive, at: time.Now()}
	probeMu.Unlock()
	return alive
}

// ResolveClaudeRunner decides how a claude_code org talks to Claude:
//
//  1. ABY_PROXY_URL=direct                      -> direct (forced)
//  2. a custom proxy URL (stored on the org or ABY_PROXY_URL=http…) -> proxy
//  3. default URL (127.0.0.1:8765) answering /health (cached 30s) -> proxy
//  4. default URL silent and a `claude` binary is present          -> direct
//  5. otherwise                                                     -> proxy
//     (nothing to run locally; the error then says the proxy is unreachable)
//
// Any other mode is always "proxy" (it is an HTTP endpoint by definition).
func ResolveClaudeRunner(rt airuntime.Runtime) string {
	if airuntime.NormalizeMode(rt.Mode) != airuntime.ModeClaudeCode {
		return RunnerProxy
	}
	base := strings.TrimSpace(rt.ProxyBaseURL)
	if strings.EqualFold(base, abyProxyDirectValue) {
		return RunnerDirect
	}
	if base != "" && base != airuntime.DefaultProxyURL {
		return RunnerProxy
	}
	if cachedProxyAlive(airuntime.DefaultProxyURL) {
		return RunnerProxy
	}
	if claudeBinaryProbe() {
		return RunnerDirect
	}
	return RunnerProxy
}

// ---- runner ----

// ClaudeDirectRunner executes `claude -p` with bounded concurrency/time.
type ClaudeDirectRunner struct {
	Bin      func() string // resolved lazily so an install is picked up
	Timeout  time.Duration
	HomeBase string // parent of the per-token isolated HOME dirs

	sem chan struct{}
}

func NewClaudeDirectRunner(concurrency int, timeout time.Duration) *ClaudeDirectRunner {
	if concurrency < 1 {
		concurrency = 1
	}
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	return &ClaudeDirectRunner{
		Bin:      func() string { return BinaryPath() },
		Timeout:  timeout,
		HomeBase: filepath.Join(ToolsRoot(), "direct-home"),
		sem:      make(chan struct{}, concurrency),
	}
}

var (
	defaultDirectOnce   sync.Once
	defaultDirectRunner *ClaudeDirectRunner
)

// DefaultClaudeDirectRunner reads CLAUDE_DIRECT_CONCURRENCY (default 2) and
// CLAUDE_DIRECT_TIMEOUT (seconds or a Go duration, default 300s) once.
func DefaultClaudeDirectRunner() *ClaudeDirectRunner {
	defaultDirectOnce.Do(func() {
		conc := 2
		if v, err := strconv.Atoi(strings.TrimSpace(os.Getenv("CLAUDE_DIRECT_CONCURRENCY"))); err == nil && v > 0 {
			conc = v
		}
		timeout := 300 * time.Second
		if v := strings.TrimSpace(os.Getenv("CLAUDE_DIRECT_TIMEOUT")); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				timeout = time.Duration(n) * time.Second
			} else if d, err := time.ParseDuration(v); err == nil && d > 0 {
				timeout = d
			}
		}
		defaultDirectRunner = NewClaudeDirectRunner(conc, timeout)
	})
	return defaultDirectRunner
}

const maxDirectStdout = 16 << 20

// ClaudeDirectError carries a user-presentable message that never contains the
// token or the prompt.
type ClaudeDirectError struct{ Msg string }

func (e *ClaudeDirectError) Error() string { return e.Msg }

// tokenHome returns the isolated HOME for a token: one dir per org token, so
// sessions/config of two orgs never mix, and nothing of the host's ~/.claude is
// read. Empty token = host seat (inherit the real HOME).
func (r *ClaudeDirectRunner) tokenHome(token string) (string, error) {
	sum := sha256.Sum256([]byte(token))
	dir := filepath.Join(r.HomeBase, hex.EncodeToString(sum[:])[:16])
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return dir, nil
}

func directEnv(token, home string) []string {
	env := scrubClaudeEnv(os.Environ())
	if token != "" {
		env = append(env, claudeHomeEnv(home)...)
		env = append(env, airuntime.DirectEnv(airuntime.Runtime{ProxyAPIKey: token})...)
	} else {
		// Host seat: pass the real profile through so ~/.claude login works.
		for _, k := range []string{"HOME", "USERPROFILE", "APPDATA", "LOCALAPPDATA", "XDG_CONFIG_HOME"} {
			if v := os.Getenv(k); v != "" {
				env = append(env, k+"="+v)
			}
		}
	}
	return append(env, "NO_COLOR=1", "DISABLE_AUTOUPDATER=1")
}

// degenerateClaudeEnvelope is the shape of the zero-token, exit-1 response the
// CLI occasionally returns under heavy concurrent account load (many Claude
// Code sessions sharing one seat): valid JSON, but stop_reason=stop_sequence
// with no input/output tokens at all — never a real answer, just a stalled
// turn. A genuine reply (even "Hola") always has non-zero usage.
type degenerateClaudeEnvelope struct {
	StopReason string `json:"stop_reason"`
	Usage      struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

func isDegenerateClaudeOutput(out []byte) bool {
	var env degenerateClaudeEnvelope
	if err := json.Unmarshal(bytes.TrimSpace(out), &env); err != nil {
		return false
	}
	return env.StopReason == "stop_sequence" && env.Usage.InputTokens == 0 && env.Usage.OutputTokens == 0
}

const maxDirectAttempts = 3

// runCLI runs claude with the given args, prompt on stdin, and returns stdout.
// Retries up to maxDirectAttempts times when the CLI returns the degenerate
// zero-token envelope above — a transient stall, not a real failure — so a
// seat under concurrent load doesn't surface "no responde" to the user for
// something a second attempt resolves.
func (r *ClaudeDirectRunner) runCLI(ctx context.Context, token string, args []string, prompt string) (string, error) {
	bin := r.Bin()
	if bin == "" {
		return "", &ClaudeDirectError{"claude binary not found (install it from Organization > AI)"}
	}
	select {
	case r.sem <- struct{}{}:
		defer func() { <-r.sem }()
	case <-ctx.Done():
		return "", ctx.Err()
	}
	ctx, cancel := context.WithTimeout(ctx, r.Timeout)
	defer cancel()

	dir := os.TempDir()
	home := ""
	if token != "" {
		h, err := r.tokenHome(token)
		if err != nil {
			return "", &ClaudeDirectError{"cannot prepare isolated HOME: " + redact(err.Error(), token)}
		}
		home, dir = h, h
	}

	var lastErr error
	for attempt := 1; attempt <= maxDirectAttempts; attempt++ {
		cmd := exec.CommandContext(ctx, bin, args...)
		cmd.Dir = dir
		cmd.Env = directEnv(token, home)
		cmd.Stdin = strings.NewReader(prompt)
		cmd.WaitDelay = 3 * time.Second
		var out, errb cappedBuffer
		out.max, errb.max = maxDirectStdout, 4<<10
		cmd.Stdout, cmd.Stderr = &out, &errb

		start := time.Now()
		err := cmd.Run()
		degenerate := err != nil && isDegenerateClaudeOutput(out.buf.Bytes())
		// No prompt, no token, no output in the log: only shape and timing.
		log.Printf("claude-direct: attempt=%d exit=%v degenerate=%v dur=%s out=%dB", attempt, err == nil, degenerate, time.Since(start).Round(time.Millisecond), out.buf.Len())
		if ctx.Err() == context.DeadlineExceeded {
			return "", &ClaudeDirectError{fmt.Sprintf("claude timed out after %s", r.Timeout)}
		}
		if pe := ctx.Err(); pe != nil && errors.Is(pe, context.Canceled) {
			return "", pe
		}
		if err == nil {
			return out.buf.String(), nil
		}
		detail := strings.TrimSpace(errb.buf.String())
		if detail == "" {
			detail = strings.TrimSpace(out.buf.String())
		}
		lastErr = &ClaudeDirectError{redact(fmt.Sprintf("claude failed: %v: %s", err, truncateRunes(detail, 500)), token)}
		if !degenerate || attempt == maxDirectAttempts {
			return "", lastErr
		}
		select {
		case <-time.After(400 * time.Millisecond):
		case <-ctx.Done():
			return "", lastErr
		}
	}
	return "", lastErr
}

func redact(s, token string) string {
	if token != "" {
		s = strings.ReplaceAll(s, token, "[redacted]")
	}
	return tokenAnyRe.ReplaceAllString(s, "[redacted]")
}

type cappedBuffer struct {
	buf bytes.Buffer
	max int
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	if room := b.max - b.buf.Len(); room > 0 {
		if len(p) > room {
			b.buf.Write(p[:room])
		} else {
			b.buf.Write(p)
		}
	}
	return len(p), nil
}

// ---- request translation (OpenAI chat -> claude -p) ----

// DirectChatRequest is the subset of the OpenAI chat-completions body we use.
type DirectChatRequest struct {
	Model    string           `json:"model"`
	Messages []map[string]any `json:"messages"`
	Tools    []map[string]any `json:"tools"`
	Stream   bool             `json:"stream"`
}

// DirectChatResult is what one turn produced.
type DirectChatResult struct {
	Content   string
	ToolCalls []map[string]any
}

var directModelAliases = map[string]string{
	"claude-opus-5-5": "claude-opus-5-5", "claude-sonnet-5": "claude-sonnet-5",
	"claude-haiku-4-5": "claude-haiku-4-5-20251001", "claude-fable-5-1": "claude-fable-5-1",
	"opus": "opus", "sonnet": "sonnet", "haiku": "haiku",
}

// directModelArgs: --model only for a concrete model; "auto"/"" = CLI default.
func directModelArgs(model string) []string {
	m := strings.ToLower(strings.TrimSpace(model))
	if v, ok := directModelAliases[m]; ok {
		return []string{"--model", v}
	}
	if strings.HasPrefix(m, "claude-") {
		return []string{"--model", m}
	}
	return nil
}

// Run executes one turn. With tools it is one function-calling turn with
// Claude Code's own tools off and a JSON schema for the output.
func (r *ClaudeDirectRunner) Run(ctx context.Context, token string, req DirectChatRequest) (DirectChatResult, error) {
	if len(req.Messages) == 0 {
		return DirectChatResult{}, &ClaudeDirectError{"messages field is required"}
	}
	system, rest := splitSystemMessages(req.Messages)
	prompt := messagesToPrompt(rest)
	if prompt == "" {
		prompt = "(mensaje vacío)"
	}
	tools := toolFunctions(req.Tools)
	if len(tools) > 0 {
		system = toolSystemPrompt(system, tools)
	}

	args := []string{"-p", "--max-turns", "1", "--tools", "", "--no-session-persistence", "--output-format", "json"}
	if len(tools) > 0 {
		schema, _ := json.Marshal(toolCallSchema)
		args = append(args, "--json-schema", string(schema))
	}
	if strings.TrimSpace(system) != "" {
		// Via file: a system prompt with a big tool catalog would blow argv limits.
		f, err := os.CreateTemp(r.sysDir(token), "sys-*.txt")
		if err != nil {
			return DirectChatResult{}, &ClaudeDirectError{"cannot write system prompt: " + redact(err.Error(), token)}
		}
		defer os.Remove(f.Name())
		_, werr := f.WriteString(system)
		f.Close()
		if werr != nil {
			return DirectChatResult{}, &ClaudeDirectError{"cannot write system prompt"}
		}
		args = append(args, "--system-prompt-file", f.Name())
	}
	args = append(args, directModelArgs(req.Model)...)

	stdout, err := r.runCLI(ctx, token, args, prompt)
	if err != nil {
		return DirectChatResult{}, err
	}
	if len(tools) > 0 {
		c, calls, err := parseClaudeToolStdout(stdout)
		return DirectChatResult{Content: c, ToolCalls: calls}, err
	}
	c, err := parseClaudeChatStdout(stdout)
	return DirectChatResult{Content: c}, err
}

func (r *ClaudeDirectRunner) sysDir(token string) string {
	if token != "" {
		if h, err := r.tokenHome(token); err == nil {
			return h
		}
	}
	return os.TempDir()
}

func contentText(m map[string]any) string {
	switch c := m["content"].(type) {
	case nil:
		return ""
	case string:
		return c
	case []any:
		var b strings.Builder
		for _, p := range c {
			part, _ := p.(map[string]any)
			switch part["type"] {
			case "image_url", "image":
				b.WriteString("\n[imagen adjunta omitida: el runner directo no procesa imágenes]\n")
			default:
				if t, ok := part["text"].(string); ok {
					b.WriteString(t)
				}
			}
		}
		return b.String()
	default:
		return fmt.Sprint(c)
	}
}

func splitSystemMessages(msgs []map[string]any) (string, []map[string]any) {
	var sys []string
	var rest []map[string]any
	for _, m := range msgs {
		if m["role"] == "system" {
			if t := strings.TrimSpace(contentText(m)); t != "" {
				sys = append(sys, t)
			}
			continue
		}
		rest = append(rest, m)
	}
	return strings.Join(sys, "\n\n"), rest
}

// messagesToPrompt flattens the chat keeping tool turns (assistant tool_calls
// and role=tool results) so the next function-calling turn has them.
func messagesToPrompt(msgs []map[string]any) string {
	var lines []string
	for _, m := range msgs {
		role, _ := m["role"].(string)
		if role == "" {
			role = "user"
		}
		content := contentText(m)
		if role == "tool" || role == "function" {
			name, _ := m["name"].(string)
			id, _ := m["tool_call_id"].(string)
			lines = append(lines, fmt.Sprintf("tool_result name=%s id=%s: %s", name, id, content))
			continue
		}
		lines = append(lines, role+": "+content)
		if tc, ok := m["tool_calls"]; ok && tc != nil {
			b, _ := json.Marshal(tc)
			lines = append(lines, "tool_calls: "+string(b))
		}
	}
	return strings.Join(lines, "\n")
}

var toolCallSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"content": map[string]any{"type": "string"},
		"tool_calls": map[string]any{
			"type": "array",
			"items": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"name":      map[string]any{"type": "string"},
					"arguments": map[string]any{"type": "object"},
				},
				"required": []string{"name", "arguments"},
			},
		},
	},
	"required": []string{"content", "tool_calls"},
}

func toolFunctions(tools []map[string]any) []map[string]any {
	var out []map[string]any
	for _, t := range tools {
		fn := t
		if f, ok := t["function"].(map[string]any); ok {
			fn = f
		}
		if n, _ := fn["name"].(string); n != "" {
			out = append(out, fn)
		}
	}
	return out
}

func toolSystemPrompt(system string, tools []map[string]any) string {
	var names, blocks []string
	for _, fn := range tools {
		name, _ := fn["name"].(string)
		names = append(names, name)
		params := fn["parameters"]
		if params == nil {
			params = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		pj, _ := json.Marshal(params)
		desc, _ := fn["description"].(string)
		blocks = append(blocks, fmt.Sprintf("### %s\n%s\nparameters: %s", name, desc, pj))
	}
	finalRule := "Si no hace falta ninguna herramienta, deja tool_calls vacío y escribe la respuesta en content."
	for _, n := range names {
		if n == "final_answer" {
			finalRule = "Si la herramienta final_answer está disponible y ya puedes responder al usuario, invócala. No dejes la respuesta solo en content."
		}
	}
	if strings.TrimSpace(system) == "" {
		system = "(sin system prompt)"
	}
	return fmt.Sprintf(`Eres el modelo de function-calling del agente. No tienes shell, ni archivos, ni herramientas de Claude Code.
Solo puedes invocar las herramientas listadas. La salida debe cumplir el JSON schema: content (string) y tool_calls (lista de objetos con name y arguments).

Reglas:
- arguments es un objeto JSON, no un string.
- Puedes llamar varias herramientas en el mismo turno.
- No inventes nombres: solo %s.
- %s
- No narres que vas a llamar una herramienta; llámala.

HERRAMIENTAS:
%s

INSTRUCCIONES DEL AGENTE:
%s
`, strings.Join(names, ", "), finalRule, strings.Join(blocks, "\n\n"), system)
}

// ---- output parsing ----

type claudeJSONOut struct {
	IsError          bool            `json:"is_error"`
	Result           json.RawMessage `json:"result"`
	TerminalReason   string          `json:"terminal_reason"`
	StructuredOutput json.RawMessage `json:"structured_output"`
}

func (o claudeJSONOut) resultString() (string, bool) {
	var s string
	if len(o.Result) > 0 && json.Unmarshal(o.Result, &s) == nil {
		return s, true
	}
	return "", false
}

func parseClaudeEnvelope(stdout string) (claudeJSONOut, error) {
	var o claudeJSONOut
	if err := json.Unmarshal([]byte(strings.TrimSpace(stdout)), &o); err != nil {
		return o, &ClaudeDirectError{"claude returned non-JSON output"}
	}
	if o.IsError {
		msg, _ := o.resultString()
		if msg == "" {
			msg = o.TerminalReason
		}
		if msg == "" {
			msg = "unknown claude error"
		}
		return o, &ClaudeDirectError{"claude api error: " + truncateRunes(msg, 500)}
	}
	return o, nil
}

func parseClaudeChatStdout(stdout string) (string, error) {
	o, err := parseClaudeEnvelope(stdout)
	if err != nil {
		return "", err
	}
	s, ok := o.resultString()
	if !ok {
		return "", &ClaudeDirectError{"claude output missing 'result'"}
	}
	return s, nil
}

var fenceRe = regexp.MustCompile("(?s)^```(?:json)?\\s*|\\s*```$")
var braceRe = regexp.MustCompile(`(?s)\{.*\}`)

func coerceToolObject(raw json.RawMessage) map[string]any {
	if len(raw) == 0 {
		return nil
	}
	var obj map[string]any
	if json.Unmarshal(raw, &obj) == nil && obj != nil {
		return obj
	}
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return nil
	}
	text := strings.TrimSpace(s)
	if strings.HasPrefix(text, "```") {
		text = strings.TrimSpace(fenceRe.ReplaceAllString(text, ""))
	}
	if json.Unmarshal([]byte(text), &obj) == nil && obj != nil {
		return obj
	}
	if m := braceRe.FindString(text); m != "" {
		obj = nil
		if json.Unmarshal([]byte(m), &obj) == nil && obj != nil {
			return obj
		}
	}
	return nil
}

func parseClaudeToolStdout(stdout string) (string, []map[string]any, error) {
	o, err := parseClaudeEnvelope(stdout)
	if err != nil {
		return "", nil, err
	}
	structured := coerceToolObject(o.StructuredOutput)
	if structured == nil {
		structured = coerceToolObject(o.Result)
	}
	if _, ok := structured["tool_calls"]; structured == nil || !ok {
		text, _ := o.resultString()
		return text, nil, nil
	}
	content := ""
	switch c := structured["content"].(type) {
	case nil:
	case string:
		content = c
	default:
		b, _ := json.Marshal(c)
		content = string(b)
	}
	return content, openAIToolCalls(structured["tool_calls"]), nil
}

var toolCallSeq uint64
var toolCallSeqMu sync.Mutex

func openAIToolCalls(v any) []map[string]any {
	var calls []any
	switch t := v.(type) {
	case []any:
		calls = t
	case map[string]any:
		calls = []any{t}
	default:
		return nil
	}
	var out []map[string]any
	for i, c := range calls {
		call, _ := c.(map[string]any)
		if call == nil {
			continue
		}
		fn := call
		if f, ok := call["function"].(map[string]any); ok {
			fn = f
		}
		name, _ := fn["name"].(string)
		if name == "" {
			name, _ = call["tool_name"].(string)
		}
		if name == "" {
			continue
		}
		args := fn["arguments"]
		if args == nil {
			args = call["parameters"]
		}
		argText := "{}"
		switch a := args.(type) {
		case string:
			argText = a
		case map[string]any:
			b, _ := json.Marshal(a)
			argText = string(b)
		}
		id, _ := call["id"].(string)
		if id == "" {
			toolCallSeqMu.Lock()
			toolCallSeq++
			id = fmt.Sprintf("call_%s_%d_%08x", name, i, toolCallSeq)
			toolCallSeqMu.Unlock()
		}
		out = append(out, map[string]any{
			"id": id, "type": "function",
			"function": map[string]any{"name": name, "arguments": argText},
		})
	}
	return out
}

// BinaryPath resolves the claude executable ("" when absent). Hosts that
// install the CLI in a managed tools directory replace it at start-up
// (it is read on every call, so assign it before serving traffic).
var BinaryPath = func() string {
	if b := os.Getenv("CLAUDE_CODE_BIN"); b != "" {
		if p, err := exec.LookPath(b); err == nil {
			return p
		}
		return ""
	}
	if p, err := exec.LookPath("claude"); err == nil {
		return p
	}
	return ""
}
