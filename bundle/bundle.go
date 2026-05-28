// Package bundle reads and writes the portable addon distribution format.
//
// A bundle is a tar.gz containing:
//
//   manifest.json              (required — parsed into manifest.Manifest)
//   migrations/0001_init.sql   (optional — applied via dynamic.Apply)
//   migrations/0002_*.sql
//   frontend/remoteEntry.js    (optional — federated UI)
//   frontend/assets/*          (optional — static assets)
//   README.md                  (optional)
//
// Bundles are self-describing and may be hosted by any marketplace, or even
// side-loaded by an admin via upload.
package bundle

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/asteby/metacore-kernel/dynamic"
	"github.com/asteby/metacore-kernel/manifest"
	v3 "github.com/asteby/metacore-kernel/manifest/v3"
)

// parseManifest ingests a raw manifest.json into the legacy manifest.Manifest
// shape, dual-reading both the legacy (v2) and the Module Contract v3 formats.
//
// It peeks the raw JSON for an `apiVersion` field — the legacy struct has no
// such field, while every v3 manifest sets "apiVersion": "asteby.com/v3". When
// present the bytes are validated against the v3 contract via v3.Parse and then
// mapped into the legacy shape via manifest.FromV3, so all existing consumers
// (which are deeply coupled to manifest.Manifest) keep working unchanged.
// Otherwise the legacy unmarshal path is preserved verbatim.
func parseManifest(data []byte, dst *manifest.Manifest) error {
	var probe struct {
		APIVersion string `json:"apiVersion"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return err
	}
	if probe.APIVersion != "" {
		m, err := v3.Parse(data)
		if err != nil {
			return err
		}
		*dst = manifest.FromV3(m)
		return nil
	}
	return json.Unmarshal(data, dst)
}

// Bundle is the in-memory representation after reading a .tar.gz.
type Bundle struct {
	Manifest   manifest.Manifest
	// RawManifest holds the verbatim manifest.json bytes as they appeared in
	// the archive, BEFORE the dual-read v2/v3 normalisation that produces the
	// legacy Manifest above. It is the only place the original v3 document
	// survives — manifest.FromV3 intentionally drops kind:Preset / kind:Theme
	// blocks (the legacy Manifest has no field for them). Consumers that need
	// the v3-only surface (e.g. preset.Resolve to read preset.addons[]) parse
	// these bytes with v3.Parse. Empty for in-memory bundles built via Write.
	RawManifest []byte
	Migrations []dynamic.File
	// Frontend holds static files keyed by bundle-relative path
	// (e.g. "frontend/remoteEntry.js"). Callers persist them where needed.
	Frontend map[string][]byte
	// Backend holds server-side artifacts keyed by bundle-relative path
	// (e.g. "backend/backend.wasm"). Populated when manifest.backend.runtime
	// selects an in-process runtime like "wasm".
	Backend map[string][]byte
	// Locales holds the raw, unflattened locale JSON files keyed by bundle-
	// relative path (e.g. "locales/es-MX.json"). The v3 manifest's `i18n.bundles`
	// declares which paths to look for; Read inlines their bytes here AND
	// flattens their (possibly nested) JSON into `Manifest.I18n[<locale>]` so
	// `/v1/addons/{key}/i18n/{lang}.json` can serve them to host frontends
	// without re-reading the archive on every request.
	Locales map[string][]byte
	// Readme is the raw README.md content, if any.
	Readme string
	// RawSize is the total decompressed byte count (useful for quotas).
	RawSize int64
	// Raw holds the ORIGINAL compressed tar.gz bytes consumed by Read. It is
	// captured so signature verifiers can recompute the publish-time SHA-256
	// digest without re-reading the source. Zero-cost for callers that ignore
	// it; populated transparently by Read via a tee buffer. Empty when the
	// Bundle was constructed in-memory (e.g. tests calling Write).
	Raw []byte
	// EntryDigests maps each regular entry path inside the tarball to its
	// hex-encoded SHA-256 digest, computed over the decompressed bytes as the
	// archive is read. It is the per-file granularity that the global Ed25519
	// signature does NOT provide — the security package compares this against
	// manifest.Signature.Checksums to detect tampering of individual entries.
	// Keys match the in-archive path verbatim (e.g. "manifest.json",
	// "migrations/0001_init.sql", "frontend/remoteEntry.js"). Empty when the
	// Bundle was constructed in-memory.
	EntryDigests map[string]string
}

// Read decompresses a bundle stream and returns its parsed representation.
// It enforces a max decompressed size to defend against zip-bomb inputs.
func Read(r io.Reader, maxBytes int64) (*Bundle, error) {
	if maxBytes <= 0 {
		maxBytes = 64 << 20 // 64 MiB default
	}
	// Tee the compressed input into a buffer so signature verifiers can hash
	// the byte-exact tarball after Read returns. The buffer cap matches the
	// per-bundle decompression budget; gzip itself is bounded by its own
	// stream, so the compressed side is always strictly smaller.
	var raw bytes.Buffer
	tee := io.TeeReader(r, &raw)
	gz, err := gzip.NewReader(tee)
	if err != nil {
		return nil, fmt.Errorf("bundle: gzip: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	b := &Bundle{
		Frontend:     map[string][]byte{},
		Backend:      map[string][]byte{},
		Locales:      map[string][]byte{},
		EntryDigests: map[string]string{},
	}
	var total int64
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("bundle: tar: %w", err)
		}
		if h.Typeflag != tar.TypeReg {
			continue
		}
		// Reject absolute paths and any `..` component. substring-only check
		// would miss `foo/../bar` and accept `/etc/passwd` — widen both.
		if strings.HasPrefix(h.Name, "/") {
			return nil, fmt.Errorf("bundle: absolute path %q", h.Name)
		}
		for _, part := range strings.Split(h.Name, "/") {
			if part == ".." {
				return nil, fmt.Errorf("bundle: path traversal in %q", h.Name)
			}
		}
		// Size is enforced against bytes ACTUALLY READ, not the self-reported
		// header (which a crafted tar can lie about). Cap each entry to the
		// remaining budget so a single bomb cannot saturate RAM.
		remaining := maxBytes - total
		if remaining <= 0 {
			return nil, fmt.Errorf("bundle: decompressed size exceeds %d bytes", maxBytes)
		}
		data, err := io.ReadAll(io.LimitReader(tr, remaining+1))
		if err != nil {
			return nil, err
		}
		total += int64(len(data))
		if total > maxBytes {
			return nil, fmt.Errorf("bundle: decompressed size exceeds %d bytes", maxBytes)
		}
		// Hash every regular entry (not just the ones we route into typed
		// fields below) so the security package can spot extra files inserted
		// post-signing as well as tampered known files.
		sum := sha256.Sum256(data)
		b.EntryDigests[h.Name] = hex.EncodeToString(sum[:])
		switch {
		case h.Name == "manifest.json":
			if err := parseManifest(data, &b.Manifest); err != nil {
				return nil, fmt.Errorf("bundle: manifest.json: %w", err)
			}
			// Preserve the verbatim bytes so v3-only consumers (preset/theme
			// resolution) can re-parse the original document the legacy
			// Manifest drops. Copy because `data` aliases a reused buffer.
			b.RawManifest = append([]byte(nil), data...)
		case strings.HasPrefix(h.Name, "migrations/") && strings.HasSuffix(h.Name, ".sql"):
			name := strings.TrimSuffix(path.Base(h.Name), ".sql")
			b.Migrations = append(b.Migrations, dynamic.File{Version: name, SQL: string(data)})
		case strings.HasPrefix(h.Name, "frontend/"):
			b.Frontend[h.Name] = data
		case strings.HasPrefix(h.Name, "backend/"):
			b.Backend[h.Name] = data
		case strings.HasPrefix(h.Name, "locales/") && strings.HasSuffix(h.Name, ".json"):
			// Captured for v3 i18n bundle resolution below. Keep the verbatim
			// bytes so a later step can flatten the (possibly nested) JSON into
			// the legacy `Manifest.I18n[locale]` map without re-streaming the
			// archive. Non-JSON files under locales/ are ignored on purpose
			// (e.g. README, .gitkeep) so an extra doc never crashes Read.
			b.Locales[h.Name] = data
		case h.Name == "README.md":
			b.Readme = string(data)
		}
	}
	b.RawSize = total
	// Drain any trailing bytes (gzip footer, tar padding) so Raw matches the
	// full input stream exactly. tar.Reader stops at the end-of-archive marker
	// without consuming the gzip footer, which would otherwise leave Raw a few
	// bytes short and break sha256 reproducibility.
	if _, err := io.Copy(io.Discard, tee); err != nil {
		return nil, fmt.Errorf("bundle: drain: %w", err)
	}
	b.Raw = raw.Bytes()
	if b.Manifest.Key == "" {
		return nil, fmt.Errorf("bundle: manifest.json missing or empty")
	}
	// Apply migrations in deterministic lexicographic order.
	sort.Slice(b.Migrations, func(i, j int) bool {
		return b.Migrations[i].Version < b.Migrations[j].Version
	})
	// v3 i18n bundle inlining. FromV3.mapI18n only emits empty inner maps
	// because the manifest carries paths, not content — load the actual files
	// from the archive here so consumers reading `Manifest.I18n[locale]` get
	// the translations without a second pass over the bundle. Best effort: a
	// missing or malformed locale file degrades to an empty map for that locale
	// instead of failing the whole bundle read.
	if len(b.Locales) > 0 && len(b.RawManifest) > 0 {
		hydrateManifestI18n(&b.Manifest, b.RawManifest, b.Locales)
	}
	return b, nil
}

// hydrateManifestI18n loads the locale JSON files referenced by a v3 manifest's
// `i18n.bundles` block into `m.I18n[locale]` as a flat dotted-key map. Nested
// JSON objects (e.g. {"accounting": {"nav": {"group": "Contabilidad"}}}) are
// flattened into {"accounting.nav.group": "Contabilidad"} so the hub's
// `/v1/addons/{key}/i18n/{lang}.json` endpoint can serve the bundle directly,
// and host i18next instances can register it without a custom transform.
//
// Behaviour:
//   - The exact bundle locale (e.g. "es-MX") is loaded as-is.
//   - The base language (e.g. "es") is mirrored when not already present, so a
//     host requesting "es" still resolves an "es-MX"-only bundle.
//   - Non-string leaves (numbers, booleans, arrays) are dropped — i18next only
//     consumes string values for `t()`.
//   - Errors on individual files keep the map populated for everything else.
//
// The legacy manifest is mutated in place. RawManifest is parsed inline because
// `manifest.Manifest` (the v2 shape) drops the `i18n.bundles` block during
// FromV3 — only the file PATHS know which locale maps to which file.
func hydrateManifestI18n(m *manifest.Manifest, rawManifest []byte, locales map[string][]byte) {
	parsed, err := v3.Parse(rawManifest)
	if err != nil || parsed == nil || parsed.I18n == nil || len(parsed.I18n.Bundles) == 0 {
		return
	}
	if m.I18n == nil {
		m.I18n = map[string]map[string]string{}
	}
	for _, bndl := range parsed.I18n.Bundles {
		if bndl.Locale == "" || bndl.Path == "" {
			continue
		}
		raw, ok := locales[bndl.Path]
		if !ok {
			continue
		}
		var doc map[string]any
		if err := json.Unmarshal(raw, &doc); err != nil {
			continue
		}
		flat := map[string]string{}
		flattenLocale("", doc, flat)
		if len(flat) == 0 {
			continue
		}
		m.I18n[bndl.Locale] = flat
		// Mirror to the base language tag (e.g. "es" from "es-MX") so callers
		// asking for the bare language still resolve. Only when not already
		// populated by an explicit bundle entry for the base tag.
		if dash := strings.IndexByte(bndl.Locale, '-'); dash > 0 {
			base := bndl.Locale[:dash]
			if _, exists := m.I18n[base]; !exists {
				m.I18n[base] = flat
			}
		}
	}
}

// flattenLocale walks a nested locale object and emits flat dotted keys for
// every string leaf into `out`. Non-string leaves are skipped — i18next only
// uses string values for translations; arrays/numbers/booleans in a locale
// bundle are addon-specific extensions that don't belong in the host's t()
// lookup table.
func flattenLocale(prefix string, in any, out map[string]string) {
	switch v := in.(type) {
	case map[string]any:
		for k, child := range v {
			key := k
			if prefix != "" {
				key = prefix + "." + k
			}
			flattenLocale(key, child, out)
		}
	case string:
		if prefix != "" {
			out[prefix] = v
		}
	}
}

// epoch is the fixed modification time stamped into every bundle entry so
// that builds are byte-for-byte reproducible.
var epoch = time.Unix(0, 0).UTC()

// Write serializes a Bundle into a deterministic tar.gz stream.
//
// Entries are emitted in a stable order: manifest.json, migrations/* (sorted
// by Version), frontend/* (sorted by key), README.md. All entries use a
// fixed mtime (unix 0) so identical inputs produce byte-identical outputs.
func Write(w io.Writer, b *Bundle) error {
	if b == nil {
		return fmt.Errorf("bundle: nil")
	}
	gz := gzip.NewWriter(w)
	// Clear gzip header fields that vary (mtime, OS, name) for reproducibility.
	gz.ModTime = epoch
	tw := tar.NewWriter(gz)

	write := func(name string, data []byte) error {
		if strings.Contains(name, "..") {
			return fmt.Errorf("bundle: path traversal in %q", name)
		}
		h := &tar.Header{
			Name:     name,
			Mode:     0o644,
			Size:     int64(len(data)),
			ModTime:  epoch,
			Typeflag: tar.TypeReg,
			Format:   tar.FormatPAX,
		}
		if err := tw.WriteHeader(h); err != nil {
			return err
		}
		_, err := tw.Write(data)
		return err
	}

	// 1. manifest.json
	mb, err := json.MarshalIndent(&b.Manifest, "", "  ")
	if err != nil {
		return fmt.Errorf("bundle: marshal manifest: %w", err)
	}
	if err := write("manifest.json", mb); err != nil {
		return err
	}

	// 2. migrations — sorted by Version for deterministic output.
	migs := make([]dynamic.File, len(b.Migrations))
	copy(migs, b.Migrations)
	sort.Slice(migs, func(i, j int) bool { return migs[i].Version < migs[j].Version })
	for _, m := range migs {
		name := path.Join("migrations", m.Version+".sql")
		if err := write(name, []byte(m.SQL)); err != nil {
			return err
		}
	}

	// 3. frontend — sorted by path.
	fkeys := make([]string, 0, len(b.Frontend))
	for k := range b.Frontend {
		fkeys = append(fkeys, k)
	}
	sort.Strings(fkeys)
	for _, k := range fkeys {
		name := k
		if !strings.HasPrefix(name, "frontend/") {
			name = path.Join("frontend", k)
		}
		if err := write(name, b.Frontend[k]); err != nil {
			return err
		}
	}

	// 4. backend — sorted by path. Carries in-process artifacts like .wasm.
	bkeys := make([]string, 0, len(b.Backend))
	for k := range b.Backend {
		bkeys = append(bkeys, k)
	}
	sort.Strings(bkeys)
	for _, k := range bkeys {
		name := k
		if !strings.HasPrefix(name, "backend/") {
			name = path.Join("backend", k)
		}
		if err := write(name, b.Backend[k]); err != nil {
			return err
		}
	}

	// 5. locales — sorted by path so Write is byte-deterministic. Callers can
	// build a bundle in-memory with locale files and round-trip it through
	// Read without losing the i18n payload.
	lkeys := make([]string, 0, len(b.Locales))
	for k := range b.Locales {
		lkeys = append(lkeys, k)
	}
	sort.Strings(lkeys)
	for _, k := range lkeys {
		name := k
		if !strings.HasPrefix(name, "locales/") {
			name = path.Join("locales", k)
		}
		if err := write(name, b.Locales[k]); err != nil {
			return err
		}
	}

	// 6. README.md
	if b.Readme != "" {
		if err := write("README.md", []byte(b.Readme)); err != nil {
			return err
		}
	}

	if err := tw.Close(); err != nil {
		return err
	}
	return gz.Close()
}
