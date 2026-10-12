package claude

import "testing"

func TestParseClaudeAuthStatus(t *testing.T) {
	raw := `{"loggedIn":true,"authMethod":"claude.ai","email":"danny@example.com"}`
	got := parseClaudeAuthStatus(raw, nil)
	if !got.Ready || got.Account != "danny@example.com" {
		t.Fatalf("got %+v", got)
	}
	if parseClaudeAuthStatus(`{"loggedIn":false}`, nil).Ready {
		t.Fatal("logged out claude must not be ready")
	}
	if parseClaudeAuthStatus("", nil).Ready {
		t.Fatal("missing claude must not be ready")
	}
}

func TestParseCodexLoginStatus(t *testing.T) {
	got := parseCodexLoginStatus("Logged in using ChatGPT\n", nil)
	if !got.Ready || got.Account != "ChatGPT" {
		t.Fatalf("got %+v", got)
	}
	if parseCodexLoginStatus("Not logged in", nil).Ready {
		t.Fatal("logged out codex must not be ready")
	}
}

func TestParseCursorAgentStatus(t *testing.T) {
	got := parseCursorAgentStatus("✓ Logged in as ytacxel@gmail.com\n", nil)
	if !got.Ready || got.Account != "ytacxel@gmail.com" {
		t.Fatalf("got %+v", got)
	}
	if parseCursorAgentStatus("Not logged in", nil).Ready {
		t.Fatal("logged out cursor must not be ready")
	}
}

func TestDetectLocalAgentSeatsParallel(t *testing.T) {
	got := detectLocalAgentSeats(
		func() (string, error) { return `{"loggedIn":true,"email":"a@b.c"}`, nil },
		func() (string, error) { return "not logged in", nil },
		func() (string, error) { return "Logged in as cursor@example.com", nil },
	)
	if !got.ClaudeCode.Ready || got.ClaudeCode.Account != "a@b.c" {
		t.Fatalf("claude %+v", got.ClaudeCode)
	}
	if got.Codex.Ready {
		t.Fatal("codex should be not ready")
	}
	if !got.Cursor.Ready || got.Cursor.Account != "cursor@example.com" {
		t.Fatalf("cursor %+v", got.Cursor)
	}
}
