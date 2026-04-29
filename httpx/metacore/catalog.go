// catalog.go serves GET /api/metacore/catalog for an in-app addon
// browser. Reads .tar.gz bundles from a host-configured directory
// (Deps.CatalogDir, falling back to METACORE_CATALOG_DIR env var) and
// annotates each entry with the caller org's installation state.
package metacore

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/asteby/metacore-kernel/bundle"
	"github.com/asteby/metacore-kernel/manifest"
	"github.com/gofiber/fiber/v3"
)

// catalogEntry is the wire shape for /catalog rows.
type catalogEntry struct {
	Manifest    manifest.Manifest `json:"manifest"`
	Installable bool              `json:"installable"`
	Entitled    bool              `json:"entitled"`
	Installed   bool              `json:"installed"`
	Enabled     bool              `json:"enabled"`
	Version     string            `json:"version"`
}

// Catalog lists every bundle in the catalog directory and folds in the
// org's current install state.
//
// GET /api/metacore/catalog
func (h *Handler) Catalog(c fiber.Ctx) error {
	orgID, ok := orgIDFromCtx(c)
	if !ok {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{
			"success": false,
			"message": "organization context required",
		})
	}

	dir := h.deps.CatalogDir
	if dir == "" {
		dir = os.Getenv("METACORE_CATALOG_DIR")
	}
	if dir == "" {
		return c.JSON(fiber.Map{"items": []catalogEntry{}, "total": 0})
	}

	type row struct {
		AddonKey string `gorm:"column:addon_key"`
		Status   string `gorm:"column:status"`
	}
	var installed []row
	h.deps.Bridge.DB().Table("metacore_installations").
		Select("addon_key, status").
		Where("organization_id = ?", orgID).
		Scan(&installed)
	state := map[string]string{}
	for _, r := range installed {
		state[r.AddonKey] = r.Status
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return c.JSON(fiber.Map{"items": []catalogEntry{}, "total": 0})
		}
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"success": false, "message": err.Error(),
		})
	}

	items := make([]catalogEntry, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".tar.gz") {
			continue
		}
		f, err := os.Open(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		b, err := bundle.Read(f, 64<<20)
		f.Close()
		if err != nil {
			continue
		}
		status, has := state[b.Manifest.Key]
		items = append(items, catalogEntry{
			Manifest:    b.Manifest,
			Installable: true,
			Entitled:    true,
			Installed:   has,
			Enabled:     has && status == "enabled",
			Version:     b.Manifest.Version,
		})
	}
	return c.JSON(fiber.Map{"items": items, "total": len(items)})
}
