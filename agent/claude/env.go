package claude

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
)

// tokenAnyRe matches a Claude Code OAuth token anywhere in a string (used to
// redact it from errors and logs).
var tokenAnyRe = regexp.MustCompile(`sk-ant-oat01-[A-Za-z0-9_-]+`)

// claudeEnvAllow is the only part of the host environment a spawned CLI sees.
var claudeEnvAllow = map[string]bool{
	"PATH": true, "LANG": true, "LC_ALL": true, "TZ": true, "TMPDIR": true,
	"SSL_CERT_FILE": true, "SSL_CERT_DIR": true, "NODE_EXTRA_CA_CERTS": true,
	"HTTP_PROXY": true, "HTTPS_PROXY": true, "NO_PROXY": true,
	"http_proxy": true, "https_proxy": true, "no_proxy": true,
}

// claudeEnvAllowWindows is the extra (upper-cased: Windows env names are
// case-insensitive) set a native Windows child needs to start at all.
var claudeEnvAllowWindows = map[string]bool{
	"SYSTEMROOT": true, "SYSTEMDRIVE": true, "WINDIR": true, "COMSPEC": true,
	"PATHEXT": true, "TEMP": true, "TMP": true, "PROGRAMFILES": true,
	"PROGRAMFILES(X86)": true, "PROGRAMDATA": true, "PROCESSOR_ARCHITECTURE": true,
	"NUMBER_OF_PROCESSORS": true, "OS": true,
}

// claudeHomeEnv points every "home" variable of the platform at dir so the CLI
// reads and writes its config there and never touches the real user profile.
func claudeHomeEnv(home string) []string {
	env := []string{
		"HOME=" + home,
		"XDG_CONFIG_HOME=" + filepath.Join(home, ".config"),
		"CLAUDE_CONFIG_DIR=" + filepath.Join(home, ".claude"),
	}
	if runtime.GOOS == "windows" {
		env = append(env,
			"USERPROFILE="+home,
			"APPDATA="+filepath.Join(home, "AppData", "Roaming"),
			"LOCALAPPDATA="+filepath.Join(home, "AppData", "Local"),
		)
	}
	return env
}

// scrubClaudeEnv keeps only the allow-listed variables of env.
func scrubClaudeEnv(env []string) []string {
	out := make([]string, 0, len(claudeEnvAllow))
	for _, kv := range env {
		k := kv
		if i := strings.IndexByte(kv, '='); i >= 0 {
			k = kv[:i]
		}
		if runtime.GOOS == "windows" {
			k = strings.ToUpper(k)
		}
		if claudeEnvAllow[k] || (runtime.GOOS == "windows" && claudeEnvAllowWindows[k]) {
			out = append(out, kv)
		}
	}
	return out
}

// ToolsRoot is the persistent directory for CLI homes and tools:
// METACORE_TOOLS_DIR, else $STORAGE_PATH/tools, else ~/.metacore/tools.
func ToolsRoot() string {
	if d := strings.TrimSpace(os.Getenv("METACORE_TOOLS_DIR")); d != "" {
		return d
	}
	if d := strings.TrimSpace(os.Getenv("STORAGE_PATH")); d != "" {
		return filepath.Join(d, "tools")
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		home = os.TempDir()
	}
	return filepath.Join(home, ".metacore", "tools")
}
