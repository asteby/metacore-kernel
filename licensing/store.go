package licensing

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Record is the persisted licensing snapshot: the raw token plus the decoded
// window/entitlements that let an offline instance keep running on its last
// verified claims when the hub pubkey is temporarily unreachable. The signature
// was verified before a Record is ever saved, so its window is trustworthy.
//
// It is deliberately storage-agnostic: ops persists it as a GORM singleton row,
// an appliance writes it to a JSON file. Embedders adapt their own type to/from
// Record through the LicenseStore interface.
type Record struct {
	// Token is the full base64url(claims).sig license string.
	Token string `json:"token"`
	// OrgID is the license customer id (claims.cid) as a string.
	OrgID string `json:"org_id,omitempty"`
	// Preset is the first granted preset key, if any.
	Preset string `json:"preset,omitempty"`
	// EntitledAddons is the flattened entitlement set (addons ∪ presets, or "*").
	EntitledAddons []string `json:"entitled_addons,omitempty"`

	IssuedAt    *time.Time `json:"issued_at,omitempty"`
	ExpiresAt   *time.Time `json:"expires_at,omitempty"`
	ActivatedAt *time.Time `json:"activated_at,omitempty"`
	ValidatedAt *time.Time `json:"validated_at,omitempty"`
	// Source records where the token came from: "env", "file", "admin", "renew".
	Source string `json:"source,omitempty"`
}

// Wildcard reports whether the persisted entitlement set grants "*".
func (r *Record) Wildcard() bool {
	for _, k := range r.EntitledAddons {
		if strings.TrimSpace(k) == "*" {
			return true
		}
	}
	return false
}

// LicenseStore is the minimal persistence contract an embedder implements. The
// license is a per-instance singleton, so Load returns the one Record (nil when
// none is installed) and Save upserts it. Implementations must be safe for
// concurrent use with the service's background loop.
type LicenseStore interface {
	// Load returns the persisted Record, or (nil, nil) when none exists.
	Load(ctx context.Context) (*Record, error)
	// Save upserts the singleton Record.
	Save(ctx context.Context, rec *Record) error
}

// MemoryStore is an in-memory LicenseStore for tests and ephemeral embedders.
type MemoryStore struct {
	mu  sync.RWMutex
	rec *Record
}

// NewMemoryStore returns an empty in-memory store.
func NewMemoryStore() *MemoryStore { return &MemoryStore{} }

// Load returns a copy of the stored record (nil when empty).
func (m *MemoryStore) Load(context.Context) (*Record, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.rec == nil {
		return nil, nil
	}
	cp := *m.rec
	return &cp, nil
}

// Save stores a copy of rec.
func (m *MemoryStore) Save(_ context.Context, rec *Record) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if rec == nil {
		m.rec = nil
		return nil
	}
	cp := *rec
	m.rec = &cp
	return nil
}

// FileStore persists the Record as a JSON file — the natural store for a
// self-contained appliance that has no database. Writes are atomic (temp file
// + rename) so a crash mid-save never corrupts the license.
type FileStore struct {
	path string
	mu   sync.Mutex
}

// NewFileStore returns a store backed by the JSON file at path.
func NewFileStore(path string) *FileStore { return &FileStore{path: path} }

// Load reads and decodes the Record (nil when the file is absent).
func (f *FileStore) Load(context.Context) (*Record, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, err := os.ReadFile(f.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	if len(strings.TrimSpace(string(b))) == 0 {
		return nil, nil
	}
	var rec Record
	if err := json.Unmarshal(b, &rec); err != nil {
		return nil, err
	}
	return &rec, nil
}

// Save atomically writes rec as JSON.
func (f *FileStore) Save(_ context.Context, rec *Record) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if rec == nil {
		if err := os.Remove(f.path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	b, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(f.path)
	if dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	tmp, err := os.CreateTemp(dir, ".license-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, f.path)
}
