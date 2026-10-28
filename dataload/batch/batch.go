package batch

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"sort"
	"strings"
	"time"

	"github.com/asteby/metacore-kernel/dataload"
	"github.com/asteby/metacore-kernel/provenance"
	"github.com/google/uuid"
)

// Defaults of a Contract.
const (
	DefaultManifestName     = "manifest.json"
	DefaultMaxManifestBytes = 1 << 20
	DefaultMaxFileBytes     = 256 << 20
)

// Manifest is the producer's manifest.json. Files maps a flat file name to the
// sha256 (hex) of its content.
type Manifest struct {
	Files       map[string]string `json:"files"`
	GeneratedAt string            `json:"generated_at,omitempty"`
	Producer    string            `json:"producer,omitempty"`
	// Schema names the batch layout, for example "intel-export/v1".
	Schema string `json:"schema,omitempty"`
}

// BatchInfo describes the batch a File belongs to.
type BatchInfo struct {
	// ManifestSHA is the sha256 of manifest.json (the batch identity).
	ManifestSHA string
	GeneratedAt *time.Time
	Producer    string
	Schema      string
}

// File is one verified file of a batch, handed to FileSpec.Import.
type File struct {
	Name string
	// SHA256 is the verified hex digest of Body.
	SHA256 string
	Body   []byte
	Batch  BatchInfo
}

// FileReport is what an importer reports back.
type FileReport struct {
	// Warnings are free-text notes of the importer; they surface as
	// dataload.batch.file_warning messages.
	Warnings []string
	// Counts are summed by key across the files into the Ledger record.
	Counts map[string]int
}

// FileSpec declares one file of the contract.
type FileSpec struct {
	// Name is the fixed, flat file name of the contract (never read from the
	// manifest).
	Name     string
	Optional bool
	// MaxBytes caps this file; 0 means Contract.MaxFileBytes.
	MaxBytes int64
	// Import consumes the verified file. nil only verifies it. It must be
	// idempotent: a failure anywhere in the batch retries every file.
	Import func(ctx context.Context, f File) (FileReport, error)
}

// Contract is the consumer's side of the agreement.
type Contract struct {
	// ManifestName defaults to "manifest.json".
	ManifestName string
	// Files in import order.
	Files []FileSpec
	// MaxManifestBytes defaults to 1 MiB; MaxFileBytes to 256 MiB.
	MaxManifestBytes int64
	MaxFileBytes     int64
	// Provenance, when set, puts the returned provenance in the context handed
	// to every FileSpec.Import (the kernel never invents one).
	Provenance func(file string, b BatchInfo) provenance.Provenance
}

// Scope identifies whose batches a Ledger tracks: an organization and a kind
// (for example "intel-export"), so one table serves several contracts.
type Scope struct {
	OrgID uuid.UUID
	Kind  string
}

// Record is one processed manifest.
type Record struct {
	ManifestSHA  string
	ContentPrint string
	GeneratedAt  *time.Time
	// SameContent marks a manifest recorded without importing because its
	// files equal the last batch.
	SameContent bool
	Files       []string
	Counts      map[string]int
	Warnings    []dataload.Message
}

// Ledger remembers processed batches. Record must be idempotent for a
// (scope, ManifestSHA) pair (INSERT ... ON CONFLICT DO NOTHING).
type Ledger interface {
	// Done reports whether manifestSHA was already recorded for scope.
	Done(ctx context.Context, scope Scope, manifestSHA string) (bool, error)
	// LastContentPrint is the ContentPrint of the most recently recorded
	// manifest ("" when there is none).
	LastContentPrint(ctx context.Context, scope Scope) (string, error)
	Record(ctx context.Context, scope Scope, rec Record) error
}

// Result summarizes one revision.
type Result struct {
	ManifestSHA string `json:"manifest_sha256,omitempty"`
	// Imported: the batch was imported and recorded.
	Imported bool `json:"imported"`
	// Skipped: nothing to do (manifest already processed, or same content).
	Skipped bool `json:"skipped"`
	// SameContent: a new manifest with the files of the last batch.
	SameContent bool               `json:"same_content"`
	Files       []string           `json:"files,omitempty"`
	Counts      map[string]int     `json:"counts,omitempty"`
	Warnings    []dataload.Message `json:"warnings,omitempty"`
}

// ContentPrint fingerprints a batch's content: sha256 of the sorted
// "name:sha256" pairs. It ignores generated_at and key order.
func ContentPrint(files map[string]string) string {
	lines := make([]string, 0, len(files))
	for name, h := range files {
		lines = append(lines, name+":"+strings.ToLower(strings.TrimSpace(h)))
	}
	sort.Strings(lines)
	s := sha256.Sum256([]byte(strings.Join(lines, "\n")))
	return hex.EncodeToString(s[:])
}

func sha(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// Validate checks the contract: unique, flat, non-empty file names.
func (c Contract) Validate() error {
	seen := map[string]bool{}
	for _, f := range c.Files {
		if !flatName(f.Name) || seen[f.Name] {
			return dataload.NewError(dataload.CodeBatchContractInvalid, "file", f.Name)
		}
		seen[f.Name] = true
	}
	if c.ManifestName != "" && !flatName(c.ManifestName) {
		return dataload.NewError(dataload.CodeBatchContractInvalid, "file", c.ManifestName)
	}
	if seen[c.manifestName()] {
		return dataload.NewError(dataload.CodeBatchContractInvalid, "file", c.manifestName())
	}
	return nil
}

func flatName(n string) bool {
	return n != "" && n != "." && n != ".." && !strings.ContainsAny(n, "/\\\x00") && fs.ValidPath(n)
}

func (c Contract) manifestName() string {
	if c.ManifestName != "" {
		return c.ManifestName
	}
	return DefaultManifestName
}

func (c Contract) maxManifest() int64 {
	if c.MaxManifestBytes > 0 {
		return c.MaxManifestBytes
	}
	return DefaultMaxManifestBytes
}

func (c Contract) maxFile(f FileSpec) int64 {
	switch {
	case f.MaxBytes > 0:
		return f.MaxBytes
	case c.MaxFileBytes > 0:
		return c.MaxFileBytes
	}
	return DefaultMaxFileBytes
}

// Process runs one revision of the batch directory fsys for scope. See the
// package documentation for the exact protocol. A nil error with
// Result{Imported:false, Skipped:false} means there is no complete batch yet.
// On error nothing is recorded, so the next revision retries.
func Process(ctx context.Context, fsys fs.FS, c Contract, l Ledger, scope Scope) (Result, error) {
	var res Result
	if err := c.Validate(); err != nil {
		return res, err
	}
	raw, err := readFile(fsys, c.manifestName(), c.maxManifest())
	if errors.Is(err, fs.ErrNotExist) {
		return res, nil // no complete batch yet
	}
	if err != nil {
		return res, dataload.WrapError(err, dataload.CodeBatchManifestRead)
	}
	res.ManifestSHA = sha(raw)

	done, err := l.Done(ctx, scope, res.ManifestSHA)
	if err != nil {
		return res, dataload.WrapError(err, dataload.CodeBatchLedger)
	}
	if done {
		res.Skipped = true
		return res, nil
	}
	var m Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return res, dataload.WrapError(err, dataload.CodeBatchManifestInvalid)
	}
	if len(m.Files) == 0 {
		return res, dataload.NewError(dataload.CodeBatchManifestEmpty)
	}
	info := BatchInfo{ManifestSHA: res.ManifestSHA, Producer: m.Producer, Schema: m.Schema}
	if t, ok := parseTime(m.GeneratedAt); ok {
		info.GeneratedAt = &t
	}

	fp := ContentPrint(m.Files)
	last, err := l.LastContentPrint(ctx, scope)
	if err != nil {
		return res, dataload.WrapError(err, dataload.CodeBatchLedger)
	}
	if last == fp {
		// Same files as the last batch: record this manifest (so it is not
		// recomputed on every poll) without importing anything.
		rec := Record{ManifestSHA: res.ManifestSHA, ContentPrint: fp, GeneratedAt: info.GeneratedAt, SameContent: true}
		if err := l.Record(ctx, scope, rec); err != nil {
			return res, dataload.WrapError(err, dataload.CodeBatchRecord)
		}
		res.Skipped, res.SameContent = true, true
		return res, nil
	}

	declared := make(map[string]FileSpec, len(c.Files))
	for _, f := range c.Files {
		declared[f.Name] = f
	}
	unknown := make([]string, 0)
	for name := range m.Files {
		if _, ok := declared[name]; !ok {
			unknown = append(unknown, name)
		}
	}
	sort.Strings(unknown)
	for _, name := range unknown {
		res.Warnings = append(res.Warnings, dataload.NewMessage(dataload.CodeBatchUnknownFile, "file", name))
	}

	// Verify EVERY sha256 before importing anything.
	bodies := make(map[string][]byte, len(c.Files))
	hashes := make(map[string]string, len(c.Files))
	for _, spec := range c.Files {
		want, ok := m.Files[spec.Name]
		if !ok {
			if !spec.Optional {
				res.Warnings = append(res.Warnings, dataload.NewMessage(dataload.CodeBatchMissingFile, "file", spec.Name))
			}
			continue
		}
		limit := c.maxFile(spec)
		if st, err := fs.Stat(fsys, spec.Name); err == nil && st.Size() > limit {
			return res, dataload.NewError(dataload.CodeBatchTooLarge, "file", spec.Name, "size", st.Size(), "limit", limit)
		}
		b, err := readFile(fsys, spec.Name, limit)
		var tooBig *tooLargeError
		switch {
		case errors.As(err, &tooBig):
			return res, dataload.NewError(dataload.CodeBatchTooLarge, "file", spec.Name, "size", tooBig.size, "limit", limit)
		case err != nil:
			return res, dataload.WrapError(err, dataload.CodeBatchFileRead, "file", spec.Name)
		}
		got := sha(b)
		if !strings.EqualFold(strings.TrimSpace(want), got) {
			return res, dataload.NewError(dataload.CodeBatchSHA256Mismatch, "file", spec.Name, "got", got, "want", want)
		}
		bodies[spec.Name], hashes[spec.Name] = b, got
	}

	counts := map[string]int{}
	for _, spec := range c.Files {
		body, ok := bodies[spec.Name]
		if !ok {
			continue
		}
		if err := ctx.Err(); err != nil {
			return res, err
		}
		if spec.Import != nil {
			ictx := ctx
			if c.Provenance != nil {
				ictx = provenance.WithContext(ctx, c.Provenance(spec.Name, info))
			}
			rep, err := spec.Import(ictx, File{Name: spec.Name, SHA256: hashes[spec.Name], Body: body, Batch: info})
			if err != nil {
				return res, dataload.WrapError(err, dataload.CodeBatchImportFailed, "file", spec.Name)
			}
			for _, w := range rep.Warnings {
				res.Warnings = append(res.Warnings, dataload.NewMessage(dataload.CodeBatchFileWarning, "file", spec.Name, "text", w))
			}
			for k, v := range rep.Counts {
				counts[k] += v
			}
		}
		res.Files = append(res.Files, spec.Name)
	}
	counts["files"] = len(res.Files)
	rec := Record{ManifestSHA: res.ManifestSHA, ContentPrint: fp, GeneratedAt: info.GeneratedAt,
		Files: append([]string(nil), res.Files...), Counts: counts, Warnings: append([]dataload.Message(nil), res.Warnings...)}
	if err := l.Record(ctx, scope, rec); err != nil {
		return res, dataload.WrapError(err, dataload.CodeBatchRecord)
	}
	res.Imported, res.Counts = true, counts
	return res, nil
}

type tooLargeError struct{ size int64 }

func (e *tooLargeError) Error() string { return "file too large" }

// readFile reads name, failing with tooLargeError past max bytes.
func readFile(fsys fs.FS, name string, max int64) ([]byte, error) {
	f, err := fsys.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var buf bytes.Buffer
	n, err := io.Copy(&buf, io.LimitReader(f, max+1))
	if err != nil {
		return nil, err
	}
	if n > max {
		return nil, &tooLargeError{size: n}
	}
	return buf.Bytes(), nil
}

func parseTime(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, false
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05", "2006-01-02"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC(), true
		}
	}
	return time.Time{}, false
}
