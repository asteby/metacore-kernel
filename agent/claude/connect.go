package claude

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/asteby/metacore-kernel/agent/airuntime"

	"github.com/google/uuid"
)

// Claude "link + code" connector: runs `claude setup-token` under a PTY with a
// throwaway HOME, hands the OAuth URL to the browser user, feeds back the code
// they paste, and stores the resulting long-lived token on the org's
// org's credential store (mode claude_code). Never touches the host's
// own ~/.claude. The pasted code and the token are never logged or returned.

// Stable error codes (mapped to Spanish messages by the handler).
const (
	ClaudeErrBinaryMissing = "binary_missing"
	ClaudeErrInvalidCode   = "invalid_code"
	ClaudeErrTimeout       = "timeout"
	ClaudeErrExpired       = "expired"
	ClaudeErrBusy          = "busy"
	ClaudeErrInstall       = "install_failed"
	ClaudeErrInvalidToken  = "invalid_token"
)

type ClaudeConnectError struct {
	Code string
	Err  error
}

func (e *ClaudeConnectError) Error() string {
	if e.Err != nil {
		return e.Code + ": " + e.Err.Error()
	}
	return e.Code
}
func (e *ClaudeConnectError) Unwrap() error { return e.Err }

func claudeErr(code string, err error) error { return &ClaudeConnectError{Code: code, Err: err} }

// ClaudeErrorCode returns the typed code of err, or "".
func ClaudeErrorCode(err error) string {
	var ce *ClaudeConnectError
	if errors.As(err, &ce) {
		return ce.Code
	}
	return ""
}

// oauthErrSentinel is the match-result marker for "the CLI rejected the code".
const oauthErrSentinel = "\x00oauth-error"

var (
	ansiRe       = regexp.MustCompile(`\x1b\[[0-?]*[ -/]*[@-~]|\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)|\x1b[()][A-Za-z0-9]|\x1b[=>78]`)
	authURLRe    = regexp.MustCompile(`https://[^\s"'<>]*oauth/authorize[^\s"'<>]*`)
	tokenDoneRe  = regexp.MustCompile(`(sk-ant-oat01-[A-Za-z0-9_-]+)\s`)
	tokenValidRe = regexp.MustCompile(`^sk-ant-oat01-[A-Za-z0-9_-]{20,}$`)
	// What `claude setup-token` prints when the exchange of the pasted code
	// fails ("OAuth error: Request failed with status code 400 / Press Enter to
	// retry"): it never exits, so without this the caller would wait out the
	// whole token timeout.
	oauthErrRe = regexp.MustCompile(`(?i)oauth error|press\s*enter\s*to\s*retry`)
)

// startPTY is the per-OS pty launcher; a variable so tests can fake it.
var startPTY = startOnPTY

func stripANSI(s string) string { return ansiRe.ReplaceAllString(s, "") }

// ClaudeConnectStart is what the UI needs to send the user to the browser.
type ClaudeConnectStart struct {
	SessionID string    `json:"session_id"`
	URL       string    `json:"url"`
	ExpiresAt time.Time `json:"expires_at"`
}

type claudeSession struct {
	id      string
	orgID   uuid.UUID
	proc    ptyProc
	home    string
	expires time.Time
	started time.Time // when the authorize URL was issued
	timer   *time.Timer

	mu        sync.Mutex
	buf       strings.Builder
	exited    bool
	changed   chan struct{} // closed+replaced on every output/exit event
	submit    sync.Mutex    // serialises SubmitCode
	submitted bool
	closed    bool
}

func (s *claudeSession) snapshot() (string, bool, chan struct{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return stripANSI(s.buf.String()), s.exited, s.changed
}

func (s *claudeSession) notify() {
	close(s.changed)
	s.changed = make(chan struct{})
}

func (s *claudeSession) pump() {
	chunk := make([]byte, 4096)
	for {
		n, err := s.proc.Read(chunk)
		s.mu.Lock()
		if n > 0 {
			s.buf.Write(chunk[:n])
		}
		if err != nil {
			s.exited = true
		}
		s.notify()
		s.mu.Unlock()
		if err != nil {
			return
		}
	}
}

// waitFor polls the session output until match returns a non-empty value, the
// process exits (final attempt with final=true), or the timeout fires.
func (s *claudeSession) waitFor(timeout time.Duration, match func(out string, final bool) string) (string, bool) {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for {
		out, exited, ch := s.snapshot()
		if v := match(out, exited); v != "" {
			return v, true
		}
		if exited {
			return "", false
		}
		select {
		case <-ch:
		case <-deadline.C:
			return "", false
		}
	}
}

// CredentialStore persists the token a successful connect produced. The host
// implements it over its org AI configuration.
type CredentialStore interface {
	// CurrentEndpoint returns the endpoint and model already stored for the
	// org when it is in claude_code mode ("" otherwise), so connecting keeps
	// a custom proxy URL.
	CurrentEndpoint(orgID uuid.UUID) (baseURL, model string, err error)
	// SaveClaudeToken stores token as the org's claude_code credential.
	SaveClaudeToken(orgID, userID uuid.UUID, baseURL, model, token string) error
}

// ClaudeConnectManager keeps at most one live session per organization.
type ClaudeConnectManager struct {
	store CredentialStore

	// Injectable for tests.
	Bin          func() string
	Args         []string
	TTL          time.Duration
	URLTimeout   time.Duration
	TokenTimeout time.Duration
	// EnterDelay separates the pasted code from the Enter that submits it. The
	// CLI's input box treats one write of "code\r" as a paste and ignores the
	// trailing Enter (verified against the real binary: nothing happens), while
	// text followed ~1s later by "\r" submits.
	EnterDelay time.Duration

	mu    sync.Mutex
	byID  map[string]*claudeSession
	byOrg map[uuid.UUID]*claudeSession
}

func NewClaudeConnectManager(store CredentialStore) *ClaudeConnectManager {
	return &ClaudeConnectManager{
		store:        store,
		Bin:          func() string { return BinaryPath() },
		Args:         []string{"setup-token"},
		TTL:          10 * time.Minute,
		URLTimeout:   30 * time.Second,
		TokenTimeout: 60 * time.Second,
		EnterDelay:   800 * time.Millisecond,
		byID:         map[string]*claudeSession{},
		byOrg:        map[uuid.UUID]*claudeSession{},
	}
}

// Start launches the CLI and returns the authorize URL. A previous session of
// the same org is cancelled first.
func (m *ClaudeConnectManager) Start(orgID uuid.UUID) (*ClaudeConnectStart, error) {
	bin := m.Bin()
	if bin == "" {
		return nil, claudeErr(ClaudeErrBinaryMissing, errors.New("claude binary not found"))
	}
	if _, err := exec.LookPath(bin); err != nil {
		return nil, claudeErr(ClaudeErrBinaryMissing, err)
	}

	m.mu.Lock()
	if old := m.byOrg[orgID]; old != nil {
		m.removeLocked(old)
	}
	m.mu.Unlock()

	home, err := os.MkdirTemp(claudeTempBase(), "claude-connect-*")
	if err != nil {
		return nil, err
	}
	env := append(scrubClaudeEnv(os.Environ()), claudeHomeEnv(home)...)
	env = append(env,
		"TERM=xterm-256color",
		"BROWSER=true", // never try to open a browser on the server
		"NO_COLOR=1",
	)
	proc, err := startPTY(bin, m.Args, env, home, 1000)
	if err != nil {
		_ = os.RemoveAll(home)
		if errors.Is(err, exec.ErrNotFound) || errors.Is(err, os.ErrNotExist) {
			return nil, claudeErr(ClaudeErrBinaryMissing, err)
		}
		return nil, err
	}

	s := &claudeSession{
		id:      uuid.NewString(),
		orgID:   orgID,
		proc:    proc,
		home:    home,
		started: time.Now(),
		expires: time.Now().Add(m.TTL),
		changed: make(chan struct{}),
	}
	go s.pump()

	m.mu.Lock()
	m.byID[s.id] = s
	m.byOrg[orgID] = s
	s.timer = time.AfterFunc(m.TTL, func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		if m.byID[s.id] == s {
			m.removeLocked(s)
		}
	})
	m.mu.Unlock()

	url, ok := s.waitFor(m.URLTimeout, func(out string, _ bool) string {
		// Require a trailing delimiter so we never return a half-printed URL.
		loc := authURLRe.FindStringIndex(out)
		if loc == nil || loc[1] >= len(out) {
			return ""
		}
		return out[loc[0]:loc[1]]
	})
	if !ok {
		m.Cancel(s.id, orgID)
		return nil, claudeErr(ClaudeErrTimeout, errors.New("no authorize URL from claude"))
	}
	log.Printf("claude-connect: session started org=%s", orgID)
	return &ClaudeConnectStart{SessionID: s.id, URL: url, ExpiresAt: s.expires}, nil
}

// SubmitCode types the code the user copied from the browser and stores the
// token the CLI prints. Only success is reported; the token is not returned.
func (m *ClaudeConnectManager) SubmitCode(sessionID string, orgID, userID uuid.UUID, code string) error {
	code = strings.TrimSpace(code)
	m.mu.Lock()
	s := m.byID[sessionID]
	m.mu.Unlock()
	if s == nil || s.orgID != orgID || time.Now().After(s.expires) {
		return claudeErr(ClaudeErrExpired, errors.New("session expired or not found"))
	}
	if code == "" || strings.ContainsAny(code, "\r\n\x00") || len(code) > 2048 {
		return claudeErr(ClaudeErrInvalidCode, errors.New("malformed code"))
	}

	s.submit.Lock()
	defer s.submit.Unlock()
	if s.submitted {
		return claudeErr(ClaudeErrBusy, errors.New("code already submitted"))
	}
	s.submitted = true
	defer m.Cancel(sessionID, orgID) // always kill process + wipe HOME

	if _, err := s.proc.Write([]byte(code)); err != nil {
		return claudeErr(ClaudeErrInvalidCode, errors.New("could not send code"))
	}
	if m.EnterDelay > 0 {
		time.Sleep(m.EnterDelay)
	}
	if _, err := s.proc.Write([]byte("\r")); err != nil {
		return claudeErr(ClaudeErrInvalidCode, errors.New("could not send code"))
	}
	enterAt := time.Now()

	token, ok := s.waitFor(m.TokenTimeout, func(out string, final bool) string {
		if mm := tokenDoneRe.FindStringSubmatch(out); mm != nil {
			return mm[1]
		}
		if oauthErrRe.MatchString(out) {
			return oauthErrSentinel
		}
		if final {
			return tokenAnyRe.FindString(out)
		}
		return ""
	})
	if ok && token == oauthErrSentinel {
		// Diagnostics only (no secrets): does rejection depend on code age?
		log.Printf("claude-connect: code rejected org=%s session_age=%s exchange_took=%s",
			orgID, time.Since(s.started).Round(time.Millisecond), time.Since(enterAt).Round(time.Millisecond))
		return claudeErr(ClaudeErrInvalidCode, errors.New("claude rejected the code"))
	}
	if !ok {
		_, exited, _ := s.snapshot()
		if exited {
			return claudeErr(ClaudeErrInvalidCode, errors.New("claude exited without a token"))
		}
		return claudeErr(ClaudeErrTimeout, errors.New("timed out waiting for token"))
	}
	if !tokenValidRe.MatchString(token) {
		return claudeErr(ClaudeErrInvalidCode, errors.New("unexpected token format"))
	}

	if err := m.saveToken(orgID, userID, token); err != nil {
		return err
	}
	log.Printf("claude-connect: connected org=%s via=code session_age=%s exchange_took=%s",
		orgID, time.Since(s.started).Round(time.Millisecond), time.Since(enterAt).Round(time.Millisecond))
	return nil
}

// SubmitToken stores a long-lived token the user generated with
// `claude setup-token` on their own machine. The token is never logged or
// returned.
func (m *ClaudeConnectManager) SubmitToken(orgID, userID uuid.UUID, token string) error {
	token = strings.TrimSpace(token)
	if len(token) > 512 || !tokenValidRe.MatchString(token) {
		return claudeErr(ClaudeErrInvalidToken, errors.New("malformed token"))
	}
	if err := m.saveToken(orgID, userID, token); err != nil {
		return err
	}
	log.Printf("claude-connect: connected org=%s via=token", orgID)
	return nil
}

// saveToken persists token as the org's claude_code credential, keeping any
// configured endpoint/model.
func (m *ClaudeConnectManager) saveToken(orgID, userID uuid.UUID, token string) error {
	baseURL, model, err := m.store.CurrentEndpoint(orgID)
	if err != nil {
		return err
	}
	return m.store.SaveClaudeToken(orgID, userID, baseURL, model, token)
}

// Cancel ends a session (idempotent). orgID must match.
func (m *ClaudeConnectManager) Cancel(sessionID string, orgID uuid.UUID) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.byID[sessionID]
	if s == nil || s.orgID != orgID {
		return false
	}
	m.removeLocked(s)
	return true
}

func (m *ClaudeConnectManager) removeLocked(s *claudeSession) {
	delete(m.byID, s.id)
	if m.byOrg[s.orgID] == s {
		delete(m.byOrg, s.orgID)
	}
	if s.timer != nil {
		s.timer.Stop()
	}
	s.mu.Lock()
	already := s.closed
	s.closed = true
	s.mu.Unlock()
	if already {
		return
	}
	s.proc.Kill()
	_ = s.proc.Close()
	_ = os.RemoveAll(s.home)
}

// ActiveSessions is for tests/diagnostics.
func (m *ClaudeConnectManager) ActiveSessions() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.byID)
}

// ---- binary status / install ----

func claudeToolCandidates() []string {
	bin := ToolsBinDir()
	if runtime.GOOS == "windows" {
		root := ToolsRoot()
		return []string{
			filepath.Join(bin, "claude.exe"),
			filepath.Join(root, "claude-home", ".local", "bin", "claude.exe"),
			filepath.Join(root, "claude.cmd"),
			filepath.Join(bin, "claude.cmd"),
		}
	}
	return []string{filepath.Join(bin, "claude")}
}

func fileExecutable(p string) bool {
	st, err := os.Stat(p)
	if err != nil || st.IsDir() {
		return false
	}
	if runtime.GOOS == "windows" { // no exec bits: judge by extension
		switch strings.ToLower(filepath.Ext(p)) {
		case ".exe", ".cmd", ".bat", ".com":
			return true
		}
		return false
	}
	return st.Mode()&0o111 != 0
}

// ClaudeToolsBinDir is where the installer places the claude executable.
func ToolsBinDir() string { return filepath.Join(ToolsRoot(), "bin") }

// claudeTempBase picks the parent for throwaway HOMEs: os.TempDir(), or the
// tools dir when the container filesystem is read-only.
func claudeTempBase() string {
	base := os.TempDir()
	if probe, err := os.MkdirTemp(base, ".probe-*"); err == nil {
		_ = os.Remove(probe)
		return base
	}
	alt := filepath.Join(ToolsRoot(), "tmp")
	_ = os.MkdirAll(alt, 0o700)
	return alt
}

type ClaudeBinaryStatus struct {
	Installed bool   `json:"installed"`
	Version   string `json:"version"`
	// Runner is how a claude_code org reaches Claude here: "proxy" (aby-proxy
	// over HTTP) or "direct" (the host runs `claude -p` itself). See ResolveClaudeRunner.
	Runner string `json:"runner"`
}

// claudeRunnerForStatus evaluates the runner rule for the default (unstored)
// proxy URL as seen from this process.
func claudeRunnerForStatus() string {
	rt := airuntime.Runtime{Mode: airuntime.ModeClaudeCode, ProxyBaseURL: airuntime.DefaultProxyURL}
	rt.ProxyBaseURL = airuntime.ApplyProxyEnv(os.Getenv, airuntime.ModeClaudeCode, "", rt.ProxyBaseURL)
	return ResolveClaudeRunner(rt)
}

func GetClaudeBinaryStatus() ClaudeBinaryStatus {
	bin := BinaryPath()
	runner := claudeRunnerForStatus()
	if bin == "" {
		return ClaudeBinaryStatus{Runner: runner}
	}
	out, err := runSeatProbe(bin, "--version")
	if err != nil {
		return ClaudeBinaryStatus{Installed: true, Runner: runner}
	}
	return ClaudeBinaryStatus{Installed: true, Runner: runner, Version: strings.TrimSpace(strings.SplitN(strings.TrimSpace(out), "\n", 2)[0])}
}

// claudeInstallCmd builds the install command (toolsRoot is the persistent
// tools dir); a variable so tests run offline.
//
// Preferred: Anthropic's official native installer (curl -fsSL
// https://claude.ai/install.sh | bash). It installs under $HOME/.local/bin, so
// HOME is pointed at <tools>/claude-home (no sudo, persistent) and the result
// is linked into <tools>/bin. If curl/bash are unavailable but npm is, fall
// back to `npm install -g @anthropic-ai/claude-code --prefix <tools>` (a plain
// -g would need root); that puts the binary in <tools>/bin directly.
var claudeInstallCmd = func(ctx context.Context, toolsRoot string) (*exec.Cmd, error) {
	if err := os.MkdirAll(filepath.Join(toolsRoot, "bin"), 0o755); err != nil {
		return nil, claudeErr(ClaudeErrInstall, err)
	}
	if runtime.GOOS == "windows" {
		return claudeInstallCmdWindows(ctx, toolsRoot)
	}
	_, cerr := exec.LookPath("curl")
	_, berr := exec.LookPath("bash")
	if cerr == nil && berr == nil {
		h := filepath.Join(toolsRoot, "claude-home")
		_ = os.MkdirAll(h, 0o755)
		script := `curl -fsSL https://claude.ai/install.sh | bash && ln -sf "$HOME/.local/bin/claude" "` + filepath.Join(toolsRoot, "bin", "claude") + `"`
		cmd := exec.CommandContext(ctx, "bash", "-c", script)
		cmd.Env = append(scrubClaudeEnv(os.Environ()), "HOME="+h)
		return cmd, nil
	}
	if npm, err := exec.LookPath("npm"); err == nil {
		return exec.CommandContext(ctx, npm, "install", "-g", "@anthropic-ai/claude-code", "--prefix", toolsRoot), nil
	}
	return nil, claudeErr(ClaudeErrInstall, errors.New("neither curl+bash nor npm available"))
}

// claudeInstallCmdWindows: official PowerShell installer
// (`irm https://claude.ai/install.ps1 | iex`, installs claude.exe under
// %USERPROFILE%\.local\bin; USERPROFILE is pointed at <tools>\claude-home and
// the exe copied to <tools>\bin). NOT validated on a real Windows host.
// Fallback when PowerShell is missing: npm --prefix.
func claudeInstallCmdWindows(ctx context.Context, toolsRoot string) (*exec.Cmd, error) {
	if ps, err := exec.LookPath("powershell"); err == nil {
		h := filepath.Join(toolsRoot, "claude-home")
		_ = os.MkdirAll(h, 0o755)
		script := `$ErrorActionPreference='Stop'; irm https://claude.ai/install.ps1 | iex; ` +
			`Copy-Item -Force (Join-Path $env:USERPROFILE '.local\bin\claude.exe') '` +
			filepath.Join(toolsRoot, "bin", "claude.exe") + `'`
		cmd := exec.CommandContext(ctx, ps, "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command", script)
		cmd.Env = append(scrubClaudeEnv(os.Environ()), "USERPROFILE="+h, "HOME="+h,
			"LOCALAPPDATA="+filepath.Join(h, "AppData", "Local"), "APPDATA="+filepath.Join(h, "AppData", "Roaming"))
		return cmd, nil
	}
	if npm, err := exec.LookPath("npm"); err == nil { // fallback (npm.cmd shim)
		return exec.CommandContext(ctx, npm, "install", "-g", "@anthropic-ai/claude-code", "--prefix", toolsRoot), nil
	}
	return nil, claudeErr(ClaudeErrInstall, errors.New("neither powershell nor npm available"))
}

var claudeInstallMu sync.Mutex

// InstallClaudeBinary installs Claude Code for the service user (no sudo).
func InstallClaudeBinary() (ClaudeBinaryStatus, error) {
	if !claudeInstallMu.TryLock() {
		return ClaudeBinaryStatus{}, claudeErr(ClaudeErrBusy, errors.New("install already running"))
	}
	defer claudeInstallMu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	cmd, err := claudeInstallCmd(ctx, ToolsRoot())
	if err != nil {
		return ClaudeBinaryStatus{}, err
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		log.Printf("claude-connect: install failed: %v: %s", err, truncateRunes(stripANSI(string(out)), 300))
		return ClaudeBinaryStatus{}, claudeErr(ClaudeErrInstall, fmt.Errorf("installer: %w", err))
	}
	st := GetClaudeBinaryStatus()
	if !st.Installed {
		return st, claudeErr(ClaudeErrInstall, errors.New("installer finished but claude not found"))
	}
	return st, nil
}
