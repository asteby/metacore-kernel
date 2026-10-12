package claude

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"
)

const (
	claudeCodeProviderName = "claude-code"
	claudeCodeOAuthBeta    = "oauth-2025-04-20"
	claudeCodeDefaultModel = "claude-haiku-4-5-20251001"
	claudeCodeHeavyModel   = "claude-sonnet-4-5"
)

// claudeCodeCredsFile is the Claude Code OAuth store .
// Override with CLAUDE_CODE_CREDENTIALS.
func claudeCodeCredsFile() string {
	if p := os.Getenv("CLAUDE_CODE_CREDENTIALS"); p != "" {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		home = "/home/ubuntu"
	}
	return filepath.Join(home, ".claude", ".credentials.json")
}

func claudeCodeBin() string {
	if b := os.Getenv("CLAUDE_CODE_BIN"); b != "" {
		return b
	}
	return "claude"
}

type claudeAiOauth struct {
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
	ExpiresAt    int64  `json:"expiresAt"` // unix ms
}

type claudeCredentialsFile struct {
	ClaudeAiOauth claudeAiOauth `json:"claudeAiOauth"`
}

var (
	claudeCodeMu sync.Mutex
)

// ClaudeCodeAvailable reports whether the host has a Claude Code OAuth seat we can use as API.
func ClaudeCodeAvailable() bool {
	_, _, err := readClaudeCodeToken(claudeCodeCredsFile())
	return err == nil
}

func readClaudeCodeToken(path string) (token string, expiresAt time.Time, err error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("claude-code credentials: %w", err)
	}
	var file claudeCredentialsFile
	if err := json.Unmarshal(raw, &file); err != nil {
		return "", time.Time{}, fmt.Errorf("claude-code credentials parse: %w", err)
	}
	tok := file.ClaudeAiOauth.AccessToken
	if tok == "" {
		return "", time.Time{}, fmt.Errorf("claude-code credentials: empty accessToken (run `claude` /login on the host)")
	}
	if file.ClaudeAiOauth.ExpiresAt > 0 {
		expiresAt = time.UnixMilli(file.ClaudeAiOauth.ExpiresAt)
	}
	return tok, expiresAt, nil
}

// EnsureClaudeCodeAccessToken returns a usable OAuth access token, refreshing via
// the Claude Code CLI (same mechanism Link uses) when the cached token is expired
// or about to expire.
func EnsureClaudeCodeAccessToken() (string, error) {
	claudeCodeMu.Lock()
	defer claudeCodeMu.Unlock()

	path := claudeCodeCredsFile()
	tok, exp, err := readClaudeCodeToken(path)
	if err != nil {
		return "", err
	}
	if !exp.IsZero() && time.Until(exp) > 2*time.Minute {
		return tok, nil
	}

	log.Printf("claude-code: access token expired/near-expiry — refreshing via CLI")
	if rerr := refreshClaudeCodeViaCLI(); rerr != nil {
		// Still usable for a few seconds? return it; else fail.
		if !exp.IsZero() && time.Now().Before(exp) && tok != "" {
			log.Printf("claude-code: refresh failed (%v); using token until %s", rerr, exp.Format(time.RFC3339))
			return tok, nil
		}
		return "", fmt.Errorf("claude-code token refresh failed: %w", rerr)
	}

	tok, _, err = readClaudeCodeToken(path)
	return tok, err
}

// refreshClaudeCodeViaCLI asks the Claude Code binary to make a tiny request.
// That path refreshes OAuth the same way Link's coder does (CLI owns the seat).
func refreshClaudeCodeViaCLI() error {
	bin := claudeCodeBin()
	ctxTimeout := 90 * time.Second
	done := make(chan error, 1)
	go func() {
		cmd := exec.Command(bin,
			"-p", "OK",
			"--output-format", "text",
			"--system-prompt", "Reply with exactly: OK",
			"--model", "haiku",
		)
		cmd.Dir = os.TempDir()
		cmd.Env = os.Environ()
		out, err := cmd.CombinedOutput()
		if err != nil {
			done <- fmt.Errorf("%v: %s", err, truncateRunes(string(out), 400))
			return
		}
		done <- nil
	}()
	select {
	case err := <-done:
		return err
	case <-time.After(ctxTimeout):
		return fmt.Errorf("claude CLI refresh timed out after %s", ctxTimeout)
	}
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
