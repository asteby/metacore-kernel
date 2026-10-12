package claude

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestReadClaudeCodeToken(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".credentials.json")
	exp := time.Now().Add(time.Hour).UnixMilli()
	payload := map[string]any{
		"claudeAiOauth": map[string]any{
			"accessToken":  "test-token-abc",
			"refreshToken": "refresh",
			"expiresAt":    exp,
		},
	}
	raw, _ := json.Marshal(payload)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	tok, expires, err := readClaudeCodeToken(path)
	if err != nil {
		t.Fatal(err)
	}
	if tok != "test-token-abc" {
		t.Fatalf("token=%q", tok)
	}
	if expires.UnixMilli() != exp {
		t.Fatalf("expires mismatch")
	}
}

func TestReadClaudeCodeTokenEmpty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".credentials.json")
	_ = os.WriteFile(path, []byte(`{"claudeAiOauth":{}}`), 0o600)
	if _, _, err := readClaudeCodeToken(path); err == nil {
		t.Fatal("expected error for empty token")
	}
}
