package claude

import (
	"context"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/asteby/metacore-kernel/agent/airuntime"

	"github.com/google/uuid"
)

const fakeToken = "sk-ant-oat01-FAKEFAKEFAKEFAKEFAKE_abc-123"

func fakeClaudeScript(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "fakeclaude")
	if err := os.WriteFile(p, []byte("#!/bin/bash\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

const okScript = `echo "Open: https://claude.com/cai/oauth/authorize?code=true&client_id=x&state=abc"
printf 'Paste code here if prompted > '
read -r line
if [ "$line" = "good-code" ]; then echo; echo "Your token: ` + fakeToken + `"; sleep 30; else echo "Invalid code"; exit 1; fi
`

// memStore is an in-memory CredentialStore.
type memStore struct {
	mu   sync.Mutex
	cfgs map[uuid.UUID]airuntime.Config
	fail error
}

func (s *memStore) get(org uuid.UUID) airuntime.Config {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cfgs[org]
}

func (s *memStore) put(org uuid.UUID, mode, base, model string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cfgs[org] = airuntime.Config{OrganizationID: org, Mode: mode, ProxyBaseURL: base, ProxyModel: model}
}

func (s *memStore) CurrentEndpoint(org uuid.UUID) (string, string, error) {
	if s.fail != nil {
		return "", "", s.fail
	}
	c := s.get(org)
	if airuntime.NormalizeMode(c.Mode) == airuntime.ModeClaudeCode {
		return c.ProxyBaseURL, c.ProxyModel, nil
	}
	return "", "", nil
}

func (s *memStore) SaveClaudeToken(org, _ uuid.UUID, base, model, token string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cfgs[org] = airuntime.Config{OrganizationID: org, Mode: airuntime.ModeClaudeCode, ProxyBaseURL: base, ProxyModel: model, ProxyAPIKey: token}
	return nil
}

func newConnectTest(t *testing.T, script string) (*ClaudeConnectManager, *memStore) {
	t.Helper()
	store := &memStore{cfgs: map[uuid.UUID]airuntime.Config{}}
	m := NewClaudeConnectManager(store)
	bin := fakeClaudeScript(t, script)
	m.Bin = func() string { return bin }
	m.Args = nil
	m.URLTimeout = 5 * time.Second
	m.TokenTimeout = 5 * time.Second
	return m, store
}

func TestClaudeConnectHappyPath(t *testing.T) {
	m, db := newConnectTest(t, okScript)
	org, user := uuid.New(), uuid.New()
	st, err := m.Start(org)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(st.URL, "https://claude.com/cai/oauth/authorize?") || st.SessionID == "" {
		t.Fatalf("bad start: %+v", st)
	}
	home := m.byID[st.SessionID].home
	if err := m.SubmitCode(st.SessionID, org, user, "good-code"); err != nil {
		t.Fatal(err)
	}
	cfg := db.get(org)
	if cfg.Mode != airuntime.ModeClaudeCode || cfg.ProxyAPIKey != fakeToken {
		t.Fatalf("cfg=%+v", cfg)
	}
	if rt := airuntime.FromConfig(cfg, os.Getenv); rt.ProxyAPIKey != fakeToken {
		t.Fatal("runtime must carry the key")
	}
	if m.ActiveSessions() != 0 {
		t.Fatal("session should be gone")
	}
	if _, err := os.Stat(home); !os.IsNotExist(err) {
		t.Fatal("temp HOME not removed")
	}
}

func TestClaudeConnectInvalidCode(t *testing.T) {
	m, db := newConnectTest(t, okScript)
	org := uuid.New()
	st, err := m.Start(org)
	if err != nil {
		t.Fatal(err)
	}
	err = m.SubmitCode(st.SessionID, org, uuid.New(), "wrong")
	if ClaudeErrorCode(err) != ClaudeErrInvalidCode {
		t.Fatalf("err=%v", err)
	}
	cfg := db.get(org)
	if cfg.ProxyAPIKey != "" {
		t.Fatal("nothing should be saved")
	}
	if m.ActiveSessions() != 0 {
		t.Fatal("session should be cleaned")
	}
}

func TestClaudeConnectTokenTimeout(t *testing.T) {
	m, _ := newConnectTest(t, `echo "https://claude.com/cai/oauth/authorize?x=1"
read -r line
sleep 30
`)
	m.TokenTimeout = 400 * time.Millisecond
	org := uuid.New()
	st, err := m.Start(org)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.SubmitCode(st.SessionID, org, uuid.New(), "x"); ClaudeErrorCode(err) != ClaudeErrTimeout {
		t.Fatalf("err=%v", err)
	}
	if m.ActiveSessions() != 0 {
		t.Fatal("not cleaned")
	}
}

func TestClaudeConnectCancelAndOnePerOrg(t *testing.T) {
	m, _ := newConnectTest(t, okScript)
	org := uuid.New()
	a, err := m.Start(org)
	if err != nil {
		t.Fatal(err)
	}
	b, err := m.Start(org)
	if err != nil {
		t.Fatal(err)
	}
	if m.ActiveSessions() != 1 {
		t.Fatalf("active=%d", m.ActiveSessions())
	}
	if err := m.SubmitCode(a.SessionID, org, uuid.New(), "good-code"); ClaudeErrorCode(err) != ClaudeErrExpired {
		t.Fatalf("old session must be expired, got %v", err)
	}
	// other org cannot touch it
	if m.Cancel(b.SessionID, uuid.New()) {
		t.Fatal("cross-org cancel")
	}
	if !m.Cancel(b.SessionID, org) || m.ActiveSessions() != 0 {
		t.Fatal("cancel failed")
	}
}

func TestClaudeConnectTTLExpiry(t *testing.T) {
	m, _ := newConnectTest(t, okScript)
	m.TTL = 200 * time.Millisecond
	org := uuid.New()
	st, err := m.Start(org)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(500 * time.Millisecond)
	if err := m.SubmitCode(st.SessionID, org, uuid.New(), "good-code"); ClaudeErrorCode(err) != ClaudeErrExpired {
		t.Fatalf("err=%v", err)
	}
}

func TestClaudeConnectBinaryMissing(t *testing.T) {
	m, _ := newConnectTest(t, okScript)
	m.Bin = func() string { return "" }
	if _, err := m.Start(uuid.New()); ClaudeErrorCode(err) != ClaudeErrBinaryMissing {
		t.Fatalf("err=%v", err)
	}
}

func TestClaudeConnectPreservesEndpointAndNoTokenInLogs(t *testing.T) {
	var logs strings.Builder
	logSink(t, &logs)
	m, db := newConnectTest(t, okScript)
	org := uuid.New()
	db.put(org, airuntime.ModeClaudeCode, "http://127.0.0.1:9999/v1", "my-model")
	st, err := m.Start(org)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.SubmitCode(st.SessionID, org, uuid.New(), "good-code"); err != nil {
		t.Fatal(err)
	}
	cfg := db.get(org)
	if cfg.ProxyBaseURL != "http://127.0.0.1:9999/v1" || cfg.ProxyModel != "my-model" {
		t.Fatalf("endpoint lost: %+v", cfg)
	}
	if strings.Contains(logs.String(), fakeToken) || strings.Contains(logs.String(), "good-code") {
		t.Fatalf("secret leaked in logs: %s", logs.String())
	}
}

func TestClaudeInstallInjectable(t *testing.T) {
	old := claudeInstallCmd
	defer func() { claudeInstallCmd = old }()
	claudeInstallCmd = func(ctx context.Context, _ string) (*exec.Cmd, error) {
		return exec.CommandContext(ctx, "false"), nil
	}
	if _, err := InstallClaudeBinary(); ClaudeErrorCode(err) != ClaudeErrInstall {
		t.Fatalf("err=%v", err)
	}
}

func logSink(t *testing.T, w io.Writer) {
	prev := log.Writer()
	log.SetOutput(w)
	t.Cleanup(func() { log.SetOutput(prev) })
}

func TestClaudeBinaryResolvesFromToolsDir(t *testing.T) {
	tools := t.TempDir()
	t.Setenv("METACORE_TOOLS_DIR", tools)
	t.Setenv("CLAUDE_CODE_BIN", "")
	t.Setenv("PATH", t.TempDir()) // no claude on PATH
	if ToolsBinDir() != filepath.Join(tools, "bin") {
		t.Fatal("tools bin dir")
	}
	if p := BinaryPath(); strings.HasPrefix(p, tools) {
		t.Fatalf("unexpected %s", p)
	}
	if err := os.MkdirAll(filepath.Join(tools, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(tools, "bin", "claude")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\necho 9.9.9\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := BinaryPath(); got != bin {
		t.Fatalf("got %q want %q", got, bin)
	}
	if st := GetClaudeBinaryStatus(); !st.Installed || st.Version != "9.9.9" {
		t.Fatalf("status=%+v", st)
	}
}

func TestClaudeToolsRootDefaults(t *testing.T) {
	t.Setenv("METACORE_TOOLS_DIR", "")
	t.Setenv("STORAGE_PATH", "/data")
	if ToolsRoot() != "/data/tools" {
		t.Fatal(ToolsRoot())
	}
}

func TestProxyURLEnvOverride(t *testing.T) {
	t.Setenv("ABY_PROXY_URL", "http://host.docker.internal:8765/v1")
	rt := airuntime.FromConfig(airuntime.Config{Mode: airuntime.ModeClaudeCode}, os.Getenv)
	if rt.ProxyBaseURL != "http://host.docker.internal:8765/v1" {
		t.Fatal(rt.ProxyBaseURL)
	}
	rt = airuntime.FromConfig(airuntime.Config{Mode: airuntime.ModeClaudeCode, ProxyBaseURL: "http://custom/v1"}, os.Getenv)
	if rt.ProxyBaseURL != "http://custom/v1" {
		t.Fatal("custom stored url must win")
	}
	rt = airuntime.FromConfig(airuntime.Config{Mode: airuntime.ModeOpenAI}, os.Getenv)
	if rt.ProxyBaseURL != "https://api.openai.com/v1" {
		t.Fatal("env must not affect other modes")
	}
}

func TestScrubClaudeEnvAllowList(t *testing.T) {
	in := []string{
		"PATH=/usr/bin", "HTTPS_PROXY=http://p:1", "DATABASE_URL=postgres://secret",
		"JWT_SECRET=s", "ANTHROPIC_API_KEY=k", "CLAUDE_CODE_OAUTH_TOKEN=t", "HOME=/root",
	}
	got := strings.Join(scrubClaudeEnv(in), "|")
	for _, want := range []string{"PATH=/usr/bin", "HTTPS_PROXY=http://p:1"} {
		if !strings.Contains(got, want) {
			t.Fatalf("expected %q kept, got %q", want, got)
		}
	}
	for _, bad := range []string{"DATABASE_URL", "JWT_SECRET", "ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN", "HOME="} {
		if strings.Contains(got, bad) {
			t.Fatalf("expected %q dropped, got %q", bad, got)
		}
	}
}

func TestClaudeSubmitTokenSavesAndNeverLogs(t *testing.T) {
	var logs strings.Builder
	logSink(t, &logs)
	m, db := newConnectTest(t, okScript)
	org := uuid.New()
	db.put(org, airuntime.ModeClaudeCode, "http://127.0.0.1:9999/v1", "my-model")
	if err := m.SubmitToken(org, uuid.New(), "  "+fakeToken+"\n"); err != nil {
		t.Fatal(err)
	}
	cfg := db.get(org)
	if rt := airuntime.FromConfig(cfg, os.Getenv); rt.ProxyAPIKey != fakeToken {
		t.Fatalf("token not stored: %q", rt.ProxyAPIKey)
	}
	if cfg.ProxyBaseURL != "http://127.0.0.1:9999/v1" || cfg.ProxyModel != "my-model" {
		t.Fatalf("endpoint lost: %+v", cfg)
	}
	if strings.Contains(logs.String(), fakeToken) || !strings.Contains(logs.String(), "connected org="+org.String()+" via=token") {
		t.Fatalf("bad logs: %s", logs.String())
	}
}

func TestClaudeSubmitTokenRejectsMalformed(t *testing.T) {
	m, db := newConnectTest(t, okScript)
	org := uuid.New()
	for _, tok := range []string{"", "abc", "sk-ant-oat01-short", fakeToken + " extra", "sk-ant-oat01-FAKEFAKEFAKE\nFAKEFAKEFAKE_abc", fakeToken + "\x00", fakeToken + strings.Repeat("a", 600)} {
		err := m.SubmitToken(org, uuid.New(), tok)
		if ClaudeErrorCode(err) != ClaudeErrInvalidToken {
			t.Fatalf("token %q: err=%v", tok, err)
		}
	}
	if cfg := db.get(org); cfg.ProxyAPIKey != "" {
		t.Fatal("malformed token was stored")
	}
}

func TestClaudeCodeRejectionAndSuccessLogDiagnostics(t *testing.T) {
	var logs strings.Builder
	logSink(t, &logs)
	m, _ := newConnectTest(t, okScript)
	org := uuid.New()
	rej := fakeClaudeScript(t, `echo "https://claude.com/cai/oauth/authorize?x=1"
read -r line
echo "OAuth error: Request failed with status code 400"; sleep 30
`)
	m.Bin = func() string { return rej }
	st, err := m.Start(org)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.SubmitCode(st.SessionID, org, uuid.New(), "x"); ClaudeErrorCode(err) != ClaudeErrInvalidCode {
		t.Fatalf("err=%v", err)
	}
	if !strings.Contains(logs.String(), "code rejected org="+org.String()+" session_age=") || !strings.Contains(logs.String(), "exchange_took=") {
		t.Fatalf("missing diagnostics: %s", logs.String())
	}
	good := fakeClaudeScript(t, okScript)
	m.Bin = func() string { return good }
	st, _ = m.Start(org)
	if err := m.SubmitCode(st.SessionID, org, uuid.New(), "good-code"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logs.String(), "via=code session_age=") || strings.Contains(logs.String(), fakeToken) {
		t.Fatalf("logs: %s", logs.String())
	}
}
func TestBinaryStatusReportsRunner(t *testing.T) {
	withProbes(t, false, true)
	t.Setenv("CLAUDE_CODE_BIN", "/nonexistent/claude")
	if st := GetClaudeBinaryStatus(); st.Runner != RunnerDirect {
		t.Fatalf("%+v", st)
	}
}

// ---- connector over a fake PTY (platform independent) ----

type fakePTY struct {
	r      *io.PipeReader
	w      *io.PipeWriter
	in     chan string
	killed bool
}

func (f *fakePTY) Read(b []byte) (int, error) { return f.r.Read(b) }
func (f *fakePTY) Write(b []byte) (int, error) {
	f.in <- string(b)
	return len(b), nil
}
func (f *fakePTY) Close() error { f.w.Close(); return nil }
func (f *fakePTY) Kill()        { f.killed = true }

func TestConnectOverFakePTY(t *testing.T) {
	m, db := newConnectTest(t, "true")
	tb, _ := exec.LookPath("true")
	m.Bin = func() string { return tb }
	old := startPTY
	defer func() { startPTY = old }()
	var fp *fakePTY
	var envSeen []string
	startPTY = func(bin string, args, env []string, dir string, cols uint16) (ptyProc, error) {
		envSeen = env
		pr, pw := io.Pipe()
		fp = &fakePTY{r: pr, w: pw, in: make(chan string, 4)}
		go func() {
			pw.Write([]byte("Open: https://claude.com/cai/oauth/authorize?code=true&state=abc \r\n"))
			// The real input box ignores an Enter that arrives in the same
			// write as the pasted text: the code and the Enter must be separate
			// writes, or nothing is submitted.
			text := <-fp.in
			enter := <-fp.in
			if text == "good-code" && enter == "\r" {
				pw.Write([]byte("Your token: " + fakeToken + "\r\n"))
			}
		}()
		return fp, nil
	}
	m.EnterDelay = 10 * time.Millisecond
	org, user := uuid.New(), uuid.New()
	st, err := m.Start(org)
	if err != nil || !strings.Contains(st.URL, "oauth/authorize") {
		t.Fatalf("%+v %v", st, err)
	}
	for _, kv := range envSeen {
		if strings.HasPrefix(kv, "ANTHROPIC_") {
			t.Fatalf("env not scrubbed: %s", kv)
		}
	}
	if err := m.SubmitCode(st.SessionID, org, user, "good-code"); err != nil {
		t.Fatal(err)
	}
	cfg := db.get(org)
	if cfg.ProxyAPIKey != fakeToken || !fp.killed {
		t.Fatalf("token stored=%v killed=%v", cfg.ProxyAPIKey == fakeToken, fp.killed)
	}
}

func TestConnectRejectedCodeFailsFast(t *testing.T) {
	m, _ := newConnectTest(t, "true")
	tb, _ := exec.LookPath("true")
	m.Bin = func() string { return tb }
	m.TokenTimeout = 30 * time.Second
	m.EnterDelay = 10 * time.Millisecond
	old := startPTY
	defer func() { startPTY = old }()
	startPTY = func(bin string, args, env []string, dir string, cols uint16) (ptyProc, error) {
		pr, pw := io.Pipe()
		fp := &fakePTY{r: pr, w: pw, in: make(chan string, 4)}
		go func() {
			pw.Write([]byte("Open: https://claude.com/cai/oauth/authorize?code=true&state=abc \r\n"))
			<-fp.in
			<-fp.in
			pw.Write([]byte("OAuth error: Request failed with status code 400\r\nPress Enter to retry.\r\n"))
		}()
		return fp, nil
	}
	org, user := uuid.New(), uuid.New()
	st, err := m.Start(org)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	err = m.SubmitCode(st.SessionID, org, user, "bad-code")
	if ClaudeErrorCode(err) != ClaudeErrInvalidCode {
		t.Fatalf("want invalid_code, got %v", err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("took %s: must not wait for the token timeout", d)
	}
}
