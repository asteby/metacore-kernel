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
)

// Bundle is the in-memory representation after reading a .tar.gz.
type Bundle struct {
	Manifest   manifest.Manifest
	Migrations []dynamic.File
	// Frontend holds static files keyed by bundle-relative path
	// (e.g. "frontend/remoteEntry.js"). Callers persist them where needed.
	Frontend map[string][]byte
	// Backend holds server-side artifacts keyed by bundle-relative path
	// (e.g. "backend/backend.wasm"). Populated when manifest.backend.runtime
	// selects an in-process runtime like "wasm".
	Backend map[string][]byte
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
			if err := json.Unmarshal(data, &b.Manifest); err != nil {
				return nil, fmt.Errorf("bundle: manifest.json: %w", err)
			}
		case strings.HasPrefix(h.Name, "migrations/") && strings.HasSuffix(h.Name, ".sql"):
			name := strings.TrimSuffix(path.Base(h.Name), ".sql")
			b.Migrations = append(b.Migrations, dynamic.File{Version: name, SQL: string(data)})
		case strings.HasPrefix(h.Name, "frontend/"):
			b.Frontend[h.Name] = data
		case strings.HasPrefix(h.Name, "backend/"):
			b.Backend[h.Name] = data
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
	return b, nil
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

	// 5. README.md
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
