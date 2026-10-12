package claude

import (
	"testing"

	"github.com/asteby/metacore-kernel/agent/airuntime"

	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// LocalAgentSeat is one CLI login the host can use for the org agent.
// Ready means the seat is logged in and can operate. Otherwise the UI
// shows it as not ready.
type LocalAgentSeat struct {
	Ready   bool   `json:"ready"`
	Account string `json:"account,omitempty"`
}

// LocalAgentSeats is what Organización → Proveedor IA renders on the
// Claude Code, Codex and Cursor cards.
type LocalAgentSeats struct {
	ClaudeCode LocalAgentSeat `json:"claude_code"`
	Codex      LocalAgentSeat `json:"codex"`
	Cursor     LocalAgentSeat `json:"cursor"`
}

type seatProbe func() (string, error)

var (
	seatProbeClaude seatProbe = func() (string, error) { return runSeatProbe(claudeCodeBin(), "auth", "status") }
	seatProbeCodex  seatProbe = func() (string, error) { return runSeatProbe(resolveLocalBin("codex"), "login", "status") }
	seatProbeCursor seatProbe = func() (string, error) { return runSeatProbe(resolveLocalBin("agent", "cursor-agent"), "status") }

	seatCacheMu sync.Mutex
	seatCacheAt time.Time
	seatCache   LocalAgentSeats
)

const seatCacheTTL = 20 * time.Second

// DetectLocalAgentSeats reports which local CLIs are logged in.
// The result is cached briefly so opening the settings page does not
// spawn three CLIs on every poll.
func DetectLocalAgentSeats() LocalAgentSeats {
	seatCacheMu.Lock()
	defer seatCacheMu.Unlock()
	if time.Since(seatCacheAt) < seatCacheTTL && !seatCacheAt.IsZero() {
		return seatCache
	}
	seatCache = detectLocalAgentSeats(seatProbeClaude, seatProbeCodex, seatProbeCursor)
	seatCacheAt = time.Now()
	return seatCache
}

func detectLocalAgentSeats(claude, codex, cursor seatProbe) LocalAgentSeats {
	var wg sync.WaitGroup
	var out LocalAgentSeats
	wg.Add(3)
	go func() {
		defer wg.Done()
		raw, err := claude()
		out.ClaudeCode = parseClaudeAuthStatus(raw, err)
	}()
	go func() {
		defer wg.Done()
		raw, err := codex()
		out.Codex = parseCodexLoginStatus(raw, err)
	}()
	go func() {
		defer wg.Done()
		raw, err := cursor()
		out.Cursor = parseCursorAgentStatus(raw, err)
	}()
	wg.Wait()
	return out
}

func parseClaudeAuthStatus(raw string, _ error) LocalAgentSeat {
	blob := jsonObject(raw)
	if blob == "" {
		return LocalAgentSeat{}
	}
	var status struct {
		LoggedIn bool   `json:"loggedIn"`
		Email    string `json:"email"`
	}
	if json.Unmarshal([]byte(blob), &status) != nil || !status.LoggedIn {
		return LocalAgentSeat{}
	}
	return LocalAgentSeat{Ready: true, Account: strings.TrimSpace(status.Email)}
}

var (
	codexLoggedInRe  = regexp.MustCompile(`(?i)logged in(?:\s+using\s+(.+))?`)
	cursorLoggedInRe = regexp.MustCompile(`(?i)logged in(?:\s+as\s+(\S+))?`)
)

func parseCodexLoginStatus(raw string, err error) LocalAgentSeat {
	if err != nil && strings.TrimSpace(raw) == "" {
		return LocalAgentSeat{}
	}
	if strings.Contains(strings.ToLower(raw), "not logged in") {
		return LocalAgentSeat{}
	}
	m := codexLoggedInRe.FindStringSubmatch(raw)
	if m == nil {
		return LocalAgentSeat{}
	}
	account := ""
	if len(m) > 1 {
		account = strings.TrimSpace(m[1])
	}
	return LocalAgentSeat{Ready: true, Account: account}
}

func parseCursorAgentStatus(raw string, err error) LocalAgentSeat {
	if err != nil && strings.TrimSpace(raw) == "" {
		return LocalAgentSeat{}
	}
	if strings.Contains(strings.ToLower(raw), "not logged in") {
		return LocalAgentSeat{}
	}
	m := cursorLoggedInRe.FindStringSubmatch(raw)
	if m == nil {
		return LocalAgentSeat{}
	}
	account := ""
	if len(m) > 1 {
		account = strings.TrimSpace(m[1])
	}
	return LocalAgentSeat{Ready: true, Account: account}
}

func jsonObject(raw string) string {
	start := strings.Index(raw, "{")
	end := strings.LastIndex(raw, "}")
	if start < 0 || end <= start {
		return ""
	}
	return raw[start : end+1]
}

func runSeatProbe(bin string, args ...string) (string, error) {
	if strings.TrimSpace(bin) == "" {
		return "", exec.ErrNotFound
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = os.Environ()
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func resolveLocalBin(names ...string) string {
	home, _ := os.UserHomeDir()
	dirs := []string{
		filepath.Join(home, ".local", "bin"),
	}
	for _, name := range names {
		if p, err := exec.LookPath(name); err == nil {
			return p
		}
		for _, dir := range dirs {
			p := filepath.Join(dir, name)
			if st, err := os.Stat(p); err == nil && !st.IsDir() {
				return p
			}
		}
	}
	return ""
}

// LocalSeats implements airuntime.SeatDetector over the CLIs logged in on this
// machine: the first ready seat wins (Claude Code, then Codex, then Cursor).
type LocalSeats struct{}

// ReadySeat implements airuntime.SeatDetector. Skipped under `go test` so CI
// never depends on a logged-in seat.
func (LocalSeats) ReadySeat() string {
	if testing.Testing() {
		return ""
	}
	seats := DetectLocalAgentSeats()
	switch {
	case seats.ClaudeCode.Ready:
		return airuntime.ModeClaudeCode
	case seats.Codex.Ready:
		return airuntime.ModeCodex
	case seats.Cursor.Ready:
		return airuntime.ModeCursor
	}
	return ""
}
