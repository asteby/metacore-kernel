package installer

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// FrontendDir returns the on-disk directory where an addon's frontend lives
// inside a given base path. Hosts pick the base (e.g. "./storage/metacore/
// addons"); the kernel owns the per-addon sub-layout.
func FrontendDir(basePath, addonKey string) string {
	return filepath.Join(basePath, addonKey)
}

// WriteFrontend materializes a bundle's frontend map into
// basePath/<addonKey>/<relPath>. It is idempotent — existing files are
// overwritten. Path traversal is rejected. If basePath is empty the call is a
// no-op (the host has opted out of on-disk frontends).
func WriteFrontend(basePath, addonKey string, files map[string][]byte) error {
	if basePath == "" {
		return nil // host opted out
	}
	dir := FrontendDir(basePath, addonKey)
	for name, data := range files {
		clean := filepath.Clean(name)
		if strings.HasPrefix(clean, "..") || strings.Contains(clean, "..") {
			return fmt.Errorf("frontend: path traversal in %q", name)
		}
		// The CLI keys files as "frontend/<rel>"; strip so disk layout is flat.
		clean = strings.TrimPrefix(clean, "frontend/")
		clean = strings.TrimPrefix(clean, string(filepath.Separator))
		out := filepath.Join(dir, clean)
		if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(out, data, 0o644); err != nil {
			return err
		}
	}
	return nil
}
