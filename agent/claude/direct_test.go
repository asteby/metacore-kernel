package claude

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/asteby/metacore-kernel/agent/airuntime"

	"github.com/google/uuid"
)

func skipWindows(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("bash fake binary")
	}
}

// newDirectTest returns a runner over a fake claude script plus the dir where
// the script records what it saw.
func newDirectTest(t *testing.T, body string, timeout time.Duration, conc int) (*ClaudeDirectRunner, string) {
	t.Helper()
	skipWindows(t)
	rec := t.TempDir()
	bin := fakeClaudeScript(t, "REC='"+rec+"'\n"+body)
	r := NewClaudeDirectRunner(conc, timeout)
	r.Bin = func() string { return bin }
	r.HomeBase = filepath.Join(t.TempDir(), "homes")
	return r, rec
}

func readRec(t *testing.T, dir, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(b))
}

const recordScript = `cat > "$REC/stdin"
echo "$CLAUDE_CODE_OAUTH_TOKEN" > "$REC/token"
echo "$HOME" > "$REC/home"
echo "$@" > "$REC/args"
env > "$REC/env"
echo '{"type":"result","is_error":false,"result":"hola mundo"}'
`

func TestDirectChatInjectsTokenAndIsolatesHome(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://secret")
	t.Setenv("ANTHROPIC_API_KEY", "should-not-leak")
	r, rec := newDirectTest(t, recordScript, 10*time.Second, 2)
	res, err := r.Run(context.Background(), fakeToken, DirectChatRequest{
		Model: "haiku",
		Messages: []map[string]any{
			{"role": "system", "content": "eres aby"},
			{"role": "user", "content": "hola"},
		},
	})
	if err != nil || res.Content != "hola mundo" {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	if got := readRec(t, rec, "token"); got != fakeToken {
		t.Fatalf("token not injected: %q", got)
	}
	if home := readRec(t, rec, "home"); !strings.HasPrefix(home, r.HomeBase) {
		t.Fatalf("HOME not isolated: %q", home)
	}
	if in := readRec(t, rec, "stdin"); in != "user: hola" {
		t.Fatalf("stdin=%q", in)
	}
	args := readRec(t, rec, "args")
	for _, want := range []string{"-p", "--max-turns 1", "--output-format json", "--system-prompt-file", "--model haiku"} {
		if !strings.Contains(args, want) {
			t.Errorf("args %q missing %q", args, want)
		}
	}
	env := readRec(t, rec, "env")
	for _, bad := range []string{"DATABASE_URL", "ANTHROPIC_API_KEY", "should-not-leak"} {
		if strings.Contains(env, bad) {
			t.Errorf("env leaked %q", bad)
		}
	}
}

func TestDirectHomesDifferPerToken(t *testing.T) {
	r, _ := newDirectTest(t, recordScript, time.Second, 1)
	a, _ := r.tokenHome("sk-ant-oat01-aaaaaaaaaaaaaaaaaaaaaaaa")
	b, _ := r.tokenHome("sk-ant-oat01-bbbbbbbbbbbbbbbbbbbbbbbb")
	if a == b {
		t.Fatal("two orgs share a HOME")
	}
}

func TestDirectLogsNeverContainTokenOrPrompt(t *testing.T) {
	r, _ := newDirectTest(t, recordScript, 10*time.Second, 1)
	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)
	_, err := r.Run(context.Background(), fakeToken, DirectChatRequest{Messages: []map[string]any{{"role": "user", "content": "PROMPT-SECRETO-XYZ"}}})
	if err != nil {
		t.Fatal(err)
	}
	if out := buf.String(); strings.Contains(out, fakeToken) || strings.Contains(out, "PROMPT-SECRETO") || strings.Contains(out, "hola mundo") {
		t.Fatalf("log leaked: %s", out)
	}
}

func TestDirectErrorRedactsToken(t *testing.T) {
	r, _ := newDirectTest(t, `echo "boom `+fakeToken+`" >&2; exit 3`, 10*time.Second, 1)
	_, err := r.Run(context.Background(), fakeToken, DirectChatRequest{Messages: []map[string]any{{"role": "user", "content": "x"}}})
	if err == nil || strings.Contains(err.Error(), fakeToken) || !strings.Contains(err.Error(), "redacted") {
		t.Fatalf("err=%v", err)
	}
}

const degenerateJSON = `{"type":"result","stop_reason":"stop_sequence","usage":{"input_tokens":0,"output_tokens":0}}`

func TestIsDegenerateClaudeOutput(t *testing.T) {
	if !isDegenerateClaudeOutput([]byte(degenerateJSON)) {
		t.Fatal("zero-token stop_sequence envelope must be degenerate")
	}
	if isDegenerateClaudeOutput([]byte(`{"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":2}}`)) {
		t.Fatal("real reply flagged degenerate")
	}
	if isDegenerateClaudeOutput([]byte("not json")) {
		t.Fatal("non-JSON output flagged degenerate")
	}
}

func TestDirectRetriesDegenerateEnvelope(t *testing.T) {
	// First attempt: degenerate envelope + exit 1; second: a real answer.
	body := `cat > /dev/null
if [ ! -f "$REC/first" ]; then touch "$REC/first"; echo '` + degenerateJSON + `'; exit 1; fi
echo '{"type":"result","is_error":false,"result":"hola"}'
`
	r, _ := newDirectTest(t, body, 10*time.Second, 1)
	if _, err := r.Run(context.Background(), fakeToken, DirectChatRequest{Messages: []map[string]any{{"role": "user", "content": "x"}}}); err != nil {
		t.Fatalf("expected retry to succeed, got %v", err)
	}
}

func TestDirectDoesNotRetryRealFailure(t *testing.T) {
	body := `cat > /dev/null
echo x >> "$REC/count"; echo boom >&2; exit 3
`
	r, rec := newDirectTest(t, body, 10*time.Second, 1)
	if _, err := r.Run(context.Background(), fakeToken, DirectChatRequest{Messages: []map[string]any{{"role": "user", "content": "x"}}}); err == nil {
		t.Fatal("expected error")
	}
	if n := strings.Count(readRec(t, rec, "count"), "x"); n != 1 {
		t.Fatalf("real failure retried %d times", n)
	}
}

func TestDirectTimeout(t *testing.T) {
	r, _ := newDirectTest(t, "sleep 5", 300*time.Millisecond, 1)
	start := time.Now()
	_, err := r.Run(context.Background(), fakeToken, DirectChatRequest{Messages: []map[string]any{{"role": "user", "content": "x"}}})
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("err=%v", err)
	}
	if time.Since(start) > 4*time.Second {
		t.Fatal("did not kill on timeout")
	}
}

func TestDirectConcurrencyLimited(t *testing.T) {
	r, rec := newDirectTest(t, `printf s >> "$REC/order"; sleep 0.3; printf e >> "$REC/order"; echo '{"result":"ok"}'`, 10*time.Second, 1)
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = r.Run(context.Background(), fakeToken, DirectChatRequest{Messages: []map[string]any{{"role": "user", "content": "x"}}})
		}()
	}
	wg.Wait()
	if got := readRec(t, rec, "order"); got != "ses"+"ese" {
		t.Fatalf("runs overlapped: %q", got)
	}
}

func TestDirectConcurrencyCancelWhileQueued(t *testing.T) {
	r, _ := newDirectTest(t, "sleep 2", 10*time.Second, 1)
	go func() {
		_, _ = r.Run(context.Background(), fakeToken, DirectChatRequest{Messages: []map[string]any{{"role": "user", "content": "x"}}})
	}()
	time.Sleep(200 * time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if _, err := r.Run(ctx, fakeToken, DirectChatRequest{Messages: []map[string]any{{"role": "user", "content": "x"}}}); err == nil {
		t.Fatal("queued run should fail when ctx expires")
	}
}

func TestDirectLargePromptViaStdin(t *testing.T) {
	r, rec := newDirectTest(t, recordScript, 20*time.Second, 1)
	big := strings.Repeat("a", 500_000)
	if _, err := r.Run(context.Background(), fakeToken, DirectChatRequest{Messages: []map[string]any{{"role": "user", "content": big}}}); err != nil {
		t.Fatal(err)
	}
	if len(readRec(t, rec, "stdin")) < 500_000 || len(readRec(t, rec, "args")) > 1000 {
		t.Fatal("large prompt must travel on stdin, not argv")
	}
}

func chatThroughTransport(t *testing.T, r *ClaudeDirectRunner, body map[string]any) (int, map[string]any) {
	t.Helper()
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, DirectBaseURL+"/chat/completions", bytes.NewReader(raw))
	resp, err := (&claudeDirectTransport{runner: r, token: fakeToken}).RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	var out map[string]any
	_ = json.Unmarshal(b, &out)
	return resp.StatusCode, out
}

func TestDirectTransportChatShape(t *testing.T) {
	r, _ := newDirectTest(t, recordScript, 10*time.Second, 1)
	code, out := chatThroughTransport(t, r, map[string]any{"model": "auto", "messages": []any{map[string]any{"role": "user", "content": "hi"}}})
	ch := out["choices"].([]any)[0].(map[string]any)
	if code != 200 || ch["finish_reason"] != "stop" || ch["message"].(map[string]any)["content"] != "hola mundo" {
		t.Fatalf("%d %v", code, out)
	}
}

func TestDirectTransportToolCalling(t *testing.T) {
	r, rec := newDirectTest(t, `cat > "$REC/stdin"; echo "$@" > "$REC/args"
echo '{"is_error":false,"result":"","structured_output":{"content":"","tool_calls":[{"name":"final_answer","arguments":{"text":"listo"}}]}}'`, 10*time.Second, 1)
	code, out := chatThroughTransport(t, r, map[string]any{
		"messages": []any{
			map[string]any{"role": "system", "content": "sys"},
			map[string]any{"role": "user", "content": "q"},
			map[string]any{"role": "assistant", "content": "", "tool_calls": []any{map[string]any{"id": "c1"}}},
			map[string]any{"role": "tool", "name": "search", "tool_call_id": "c1", "content": "res"},
		},
		"tools": []any{map[string]any{"type": "function", "function": map[string]any{"name": "final_answer", "description": "d", "parameters": map[string]any{"type": "object"}}}},
	})
	ch := out["choices"].([]any)[0].(map[string]any)
	if code != 200 || ch["finish_reason"] != "tool_calls" {
		t.Fatalf("%d %v", code, out)
	}
	tc := ch["message"].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)
	fn := tc["function"].(map[string]any)
	if fn["name"] != "final_answer" || !strings.Contains(fn["arguments"].(string), "listo") || tc["type"] != "function" {
		t.Fatalf("%v", tc)
	}
	if args := readRec(t, rec, "args"); !strings.Contains(args, "--json-schema") {
		t.Fatalf("args=%s", args)
	}
	if in := readRec(t, rec, "stdin"); !strings.Contains(in, "tool_result name=search id=c1: res") || !strings.Contains(in, "tool_calls:") {
		t.Fatalf("stdin=%s", in)
	}
}

func TestDirectTransportErrorIsOpenAIShaped(t *testing.T) {
	r, _ := newDirectTest(t, `echo '{"is_error":true,"result":"Usage limit reached"}'`, 10*time.Second, 1)
	code, out := chatThroughTransport(t, r, map[string]any{"messages": []any{map[string]any{"role": "user", "content": "x"}}})
	msg := out["error"].(map[string]any)["message"].(string)
	if code != 503 || !strings.Contains(msg, "Usage limit") {
		t.Fatalf("%d %v", code, out)
	}
}

func TestDirectTransportStreamSSE(t *testing.T) {
	r, _ := newDirectTest(t, recordScript, 10*time.Second, 1)
	raw, _ := json.Marshal(map[string]any{"stream": true, "messages": []any{map[string]any{"role": "user", "content": "hi"}}})
	resp, err := (&claudeDirectTransport{runner: r}).RoundTrip(httptest.NewRequest(http.MethodPost, DirectBaseURL+"/chat/completions", bytes.NewReader(raw)))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(b), "hola mundo") || !strings.HasSuffix(strings.TrimSpace(string(b)), "[DONE]") {
		t.Fatalf("%s", b)
	}
}

func TestParseToolStdoutFallbacks(t *testing.T) {
	// fenced JSON in result, no structured_output
	c, calls, err := parseClaudeToolStdout(`{"result":"` + "```json\\n{\\\"content\\\":\\\"hi\\\",\\\"tool_calls\\\":[{\\\"name\\\":\\\"x\\\",\\\"arguments\\\":{}}]}\\n```" + `"}`)
	if err != nil || c != "hi" || len(calls) != 1 {
		t.Fatalf("%q %v %v", c, calls, err)
	}
	// plain prose -> content only
	c, calls, _ = parseClaudeToolStdout(`{"result":"solo texto"}`)
	if c != "solo texto" || calls != nil {
		t.Fatalf("%q %v", c, calls)
	}
}

// ---- runner detection ----

func withProbes(t *testing.T, proxyAlive, binary bool) {
	t.Helper()
	oldP, oldB := claudeProxyProbe, claudeBinaryProbe
	claudeProxyProbe = func(string) bool { return proxyAlive }
	claudeBinaryProbe = func() bool { return binary }
	probeMu.Lock()
	probeCache = map[string]probeEntry{}
	probeMu.Unlock()
	t.Cleanup(func() { claudeProxyProbe, claudeBinaryProbe = oldP, oldB })
}

func TestResolveClaudeRunner(t *testing.T) {
	cc := func(base string) airuntime.Runtime {
		return airuntime.Runtime{Mode: airuntime.ModeClaudeCode, ProxyBaseURL: base}
	}
	cases := []struct {
		name       string
		alive, bin bool
		rt         airuntime.Runtime
		want       string
	}{
		{"forced direct even if proxy alive", true, true, cc("direct"), RunnerDirect},
		{"custom url never probed", false, true, cc("http://proxy.internal:9/v1"), RunnerProxy},
		{"default url alive", true, true, cc(airuntime.DefaultProxyURL), RunnerProxy},
		{"default url dead + binary", false, true, cc(airuntime.DefaultProxyURL), RunnerDirect},
		{"default empty dead + binary", false, true, cc(""), RunnerDirect},
		{"default dead, no binary", false, false, cc(airuntime.DefaultProxyURL), RunnerProxy},
		{"other mode", false, true, airuntime.Runtime{Mode: airuntime.ModeCodex, ProxyBaseURL: airuntime.DefaultProxyURL}, RunnerProxy},
	}
	for _, c := range cases {
		withProbes(t, c.alive, c.bin)
		if got := ResolveClaudeRunner(c.rt); got != c.want {
			t.Errorf("%s: got %s want %s", c.name, got, c.want)
		}
	}
}

func TestApplyAbyProxyEnvDirect(t *testing.T) {
	t.Setenv("ABY_PROXY_URL", "direct")
	if got := airuntime.ApplyProxyEnv(os.Getenv, airuntime.ModeClaudeCode, "", airuntime.DefaultProxyURL); got != "direct" {
		t.Fatal(got)
	}
	rt := airuntime.FromConfig(airuntime.Config{Mode: airuntime.ModeClaudeCode, OrganizationID: uuid.New()}, os.Getenv)
	withProbes(t, true, false)
	if ResolveClaudeRunner(rt) != RunnerDirect {
		t.Fatal("ABY_PROXY_URL=direct must select direct")
	}
}

func TestProxyProbeIsCached(t *testing.T) {
	n := 0
	oldP := claudeProxyProbe
	claudeProxyProbe = func(string) bool { n++; return false }
	defer func() { claudeProxyProbe = oldP }()
	probeMu.Lock()
	probeCache = map[string]probeEntry{}
	probeMu.Unlock()
	for i := 0; i < 5; i++ {
		cachedProxyAlive("http://x/v1")
	}
	if n != 1 {
		t.Fatalf("probe ran %d times", n)
	}
}

func TestDefaultProxyProbeAgainstHealth(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" {
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	if !claudeProxyProbe(srv.URL + "/v1") {
		t.Fatal("alive proxy not detected")
	}
	srv.Close()
	if claudeProxyProbe(srv.URL + "/v1") {
		t.Fatal("dead proxy reported alive")
	}
}

func TestScrubClaudeEnvAndHomeEnv(t *testing.T) {
	out := scrubClaudeEnv([]string{"PATH=/bin", "ANTHROPIC_API_KEY=x", "CLAUDE_CODE_OAUTH_TOKEN=y", "DB=z", "HTTPS_PROXY=p"})
	if len(out) != 2 {
		t.Fatalf("%v", out)
	}
	if h := claudeHomeEnv("/h"); len(h) < 3 || !strings.HasPrefix(h[0], "HOME=") {
		t.Fatalf("%v", h)
	}
}

func TestDirectEnvHostSeatWhenNoToken(t *testing.T) {
	t.Setenv("HOME", "/real/home")
	env := strings.Join(directEnv("", ""), "\n")
	if !strings.Contains(env, "HOME=/real/home") || strings.Contains(env, "CLAUDE_CODE_OAUTH_TOKEN") {
		t.Fatal(env)
	}
}

// The CLI never exits on a rejected code: it prints "OAuth error … Press Enter
// to retry". The connector must report invalid_code right away instead of
// waiting out the token timeout (the 60 s 504 seen in production).
