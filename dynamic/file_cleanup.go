package dynamic

import (
	"context"
	"strings"

	"github.com/asteby/metacore-kernel/modelbase"
)

// FileDeleter is the host-supplied sink that disposes of file/image assets a
// dynamic record referenced once that reference goes away — i.e. when the record
// is deleted, or when a file/image column's value is replaced on update.
//
// The kernel is STORAGE-AGNOSTIC: it knows WHICH stored values are orphaned, but
// not where (disk, S3, CDN) or how they are physically removed. So it never
// touches a filesystem. It only collects the orphaned values (the strings the
// upload flow saved into the column — e.g. "/storage/Brand/x.jpeg") and hands
// them to the host's FileDeleter, which resolves them to the real backing store
// and removes them, applying its own safety guards (path containment, etc.).
//
// `model` is the model name the values belong to (some hosts namespace storage
// per model). `paths` are the raw column values, de-duplicated and never empty
// when invoked. The callback MUST be best-effort and non-fatal: it returns no
// error because a failed asset cleanup must never roll back or fail the CRUD
// operation that already committed. Invoked synchronously after the DB commit.
type FileDeleter func(ctx context.Context, model string, paths []string)

// fileColumnKeys returns the set of column keys in `meta` that hold a file or
// image asset value — the columns whose values must be routed to the FileDeleter
// when orphaned. Detection mirrors the SDK's renderer selection so it stays in
// lock-step with what the upload UI actually treats as a file field:
//
//   - an explicit display hint (CellStyle) of "image" / "file" / "media-gallery",
//   - or a column Type of the same (some hosts carry the hint in Type),
//   - or, as a fallback for un-hinted columns, the same name heuristic the
//     derivation uses to promote a plain-text column to a file picker
//     (imageColumn: logo/image/avatar/…).
//
// The fallback matters because a column can store an uploaded file URL without
// ever declaring a CellStyle (the form still renders an uploader via the name
// heuristic in DeriveFormFields), and those are exactly the values that leak.
func fileColumnKeys(meta *modelbase.TableMetadata) map[string]struct{} {
	keys := make(map[string]struct{})
	if meta == nil {
		return keys
	}
	for _, c := range meta.Columns {
		if isFileColumn(c.Key, c.Type, c.CellStyle) {
			keys[c.Key] = struct{}{}
		}
	}
	return keys
}

// isFileColumn reports whether a column (by its key, storage/UI type and display
// hint) holds a file/image asset value. Pure and host-agnostic.
func isFileColumn(key, colType, cellStyle string) bool {
	switch strings.ToLower(cellStyle) {
	case "image", "file", "media-gallery":
		return true
	}
	switch strings.ToLower(colType) {
	case "image", "file", "media-gallery":
		return true
	}
	// Un-hinted text column that the form would still render as an uploader.
	return imageColumn(key)
}

// extractFileValues pulls every stored asset value out of a single column value.
// It handles the three shapes a file/image column can hold:
//
//   - a bare string ("/storage/Brand/x.jpeg") — the common image/file case,
//   - a media-gallery array of objects ([]{"url": "..."}), the shape the gallery
//     field stores,
//   - a media-gallery array of bare strings ([]"..."), a tolerated variant.
//
// Empty / whitespace-only values are skipped. The kernel does NOT judge whether
// a value is "local" (that's the host's storage concern, applied in its
// FileDeleter guard); it returns every non-empty asset reference and lets the
// host decide what it owns.
func extractFileValues(value any) []string {
	var out []string
	add := func(s string) {
		if t := strings.TrimSpace(s); t != "" {
			out = append(out, t)
		}
	}
	switch v := value.(type) {
	case string:
		add(v)
	case []any:
		for _, item := range v {
			switch it := item.(type) {
			case string:
				add(it)
			case map[string]any:
				if u, ok := it["url"].(string); ok {
					add(u)
				}
			}
		}
	case []string:
		for _, s := range v {
			add(s)
		}
	}
	return out
}

// collectOrphanedFromDelete returns every file/image asset value referenced by a
// row that is being deleted, given the row snapshot and its column metadata.
// Returns nil when nothing is orphaned (no file columns, or all empty).
func collectOrphanedFromDelete(snapshot map[string]any, meta *modelbase.TableMetadata) []string {
	if len(snapshot) == 0 {
		return nil
	}
	fileCols := fileColumnKeys(meta)
	if len(fileCols) == 0 {
		return nil
	}
	var orphaned []string
	for key := range fileCols {
		orphaned = append(orphaned, extractFileValues(snapshot[key])...)
	}
	return dedupeNonEmpty(orphaned)
}

// collectOrphanedFromUpdate returns the file/image asset values that were
// present before the update but are no longer referenced after it, for every
// file column whose value changed. `before` and `after` are the pre/post row
// snapshots; a value that survives into `after` (e.g. an unchanged logo, or a
// gallery item kept across the edit) is NOT orphaned and is excluded.
func collectOrphanedFromUpdate(before, after map[string]any, meta *modelbase.TableMetadata) []string {
	if len(before) == 0 {
		return nil
	}
	fileCols := fileColumnKeys(meta)
	if len(fileCols) == 0 {
		return nil
	}
	var orphaned []string
	for key := range fileCols {
		oldVals := extractFileValues(before[key])
		if len(oldVals) == 0 {
			continue
		}
		stillReferenced := make(map[string]struct{})
		for _, v := range extractFileValues(after[key]) {
			stillReferenced[v] = struct{}{}
		}
		for _, v := range oldVals {
			if _, kept := stillReferenced[v]; !kept {
				orphaned = append(orphaned, v)
			}
		}
	}
	return dedupeNonEmpty(orphaned)
}

// dedupeNonEmpty returns the unique, non-empty values of in, preserving first
// occurrence order. Returns nil for an empty result so callers can compare
// against nil to mean "nothing to clean up".
func dedupeNonEmpty(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(in))
	var out []string
	for _, s := range in {
		if s == "" {
			continue
		}
		if _, dup := seen[s]; dup {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
