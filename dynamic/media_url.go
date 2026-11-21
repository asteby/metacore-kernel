package dynamic

import (
	"context"
	"strings"

	"github.com/asteby/metacore-kernel/modelbase"
)

// StorageKeyConfigKey is the display_config key (modelbase.ColumnDef.StyleConfig)
// that opts an image/file column into server-side URL resolution: the column
// stores a RELATIVE storage key ("products/a.jpg") and list responses carry the
// URL the host builds for it (CDN, bucket or its own media endpoint) instead of
// the raw key.
const StorageKeyConfigKey = "storage_key"

// MediaURLResolver turns a relative storage key into the URL a client can load.
// The host owns the base URL (usually per organization), so the kernel never
// hardcodes a host or path. Return "" when the key cannot be served; the raw
// value is then left untouched. Hosts MUST NOT assume the resolver sees every
// value: only keys that are relative (no scheme, not rooted) reach it.
type MediaURLResolver func(ctx context.Context, user modelbase.AuthUser, key string) string

// StoragePrefixConfigKey is an optional display_config key next to storage_key:
// a folder the column's values live under ("products" for a column that stores
// just "a.jpg"). The resolver receives "<prefix>/<value>", so the folder
// convention is declared by the model, never hardcoded in the host.
const StoragePrefixConfigKey = "storage_prefix"

type storageCol struct{ key, prefix string }

// storageKeyColumns returns the columns that opted into URL resolution
// (CellStyle or Type "image"/"file" plus display_config.storage_key).
func storageKeyColumns(meta *modelbase.TableMetadata) []storageCol {
	if meta == nil {
		return nil
	}
	var out []storageCol
	for _, c := range meta.Columns {
		on, _ := c.StyleConfig[StorageKeyConfigKey].(bool)
		if on && isFileColumn(c.Key, c.Type, c.CellStyle) {
			p, _ := c.StyleConfig[StoragePrefixConfigKey].(string)
			out = append(out, storageCol{key: c.Key, prefix: strings.Trim(strings.TrimSpace(p), "/")})
		}
	}
	return out
}

// isRelativeStorageKey reports whether v is a bare storage key: non-empty, no
// scheme, not rooted, no traversal. Absolute URLs and rooted paths are values a
// client can already load, so they are never rewritten.
func isRelativeStorageKey(v string) bool {
	v = strings.TrimSpace(v)
	if v == "" || strings.HasPrefix(v, "/") || strings.HasPrefix(v, "\\") || strings.HasPrefix(v, "data:") {
		return false
	}
	if strings.Contains(v, "://") {
		return false
	}
	for _, seg := range strings.Split(strings.ReplaceAll(v, "\\", "/"), "/") {
		if seg == ".." {
			return false
		}
	}
	return true
}

// resolveMediaURLs rewrites, in place, the opted-in columns of rows from raw
// storage key to URL. Only list responses go through it: Get keeps the raw key
// so an edit form round-trips the stored value, not a derived URL.
func (s *Service) resolveMediaURLs(ctx context.Context, user modelbase.AuthUser, meta *modelbase.TableMetadata, rows []map[string]any) {
	if s.mediaURL == nil || len(rows) == 0 {
		return
	}
	cols := storageKeyColumns(meta)
	if len(cols) == 0 {
		return
	}
	for _, row := range rows {
		for _, col := range cols {
			k := col.key
			raw, ok := row[k].(string)
			if !ok || !isRelativeStorageKey(raw) {
				continue
			}
			key := strings.TrimSpace(raw)
			if col.prefix != "" {
				key = col.prefix + "/" + key
			}
			if u := s.mediaURL(ctx, user, key); u != "" {
				row[k] = u
			}
		}
	}
}
