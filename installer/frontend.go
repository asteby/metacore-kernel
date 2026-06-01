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
// basePath/<addonKey>/<relPath>. It performs an ATOMIC REPLACE of the addon's
// frontend directory: the previous version's directory is removed before the
// new files are written, so stale assets never linger.
//
// This matters for federated (Module Federation) frontends. Each build emits
// content-hashed chunks (e.g. register-Dx9ODcZe.js); on upgrade the new build
// emits DIFFERENT hashed names. A plain filename overwrite leaves the old
// chunks on disk forever, and — worse — if anything ever causes the served
// remoteEntry.js to keep referencing an orphaned chunk, the host serves stale
// addon UI from a version the operator already upgraded away from. Removing the
// directory first guarantees serve == latest write.
//
// Path traversal is rejected. If basePath is empty the call is a no-op (the
// host has opted out of on-disk frontends).
func WriteFrontend(basePath, addonKey string, files map[string][]byte) error {
	if basePath == "" {
		return nil // host opted out
	}
	dir := FrontendDir(basePath, addonKey)
	// Clear the previous materialization so old content-hashed chunks and any
	// stale remoteEntry.js cannot survive an upgrade. Guard against an empty
	// addonKey (which would resolve `dir` to basePath itself and wipe every
	// addon) by refusing to remove anything when the cleaned key is empty.
	if cleanKey := filepath.Clean(addonKey); cleanKey == "" || cleanKey == "." || strings.Contains(cleanKey, "..") || filepath.IsAbs(cleanKey) || strings.ContainsRune(cleanKey, filepath.Separator) {
		return fmt.Errorf("frontend: invalid addon key %q", addonKey)
	}
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("frontend: clear stale dir: %w", err)
	}
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
