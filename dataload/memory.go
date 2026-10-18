package dataload

import (
	"bytes"
	"context"
	"io"
	"sort"
	"strings"
	"sync"
	"time"
)

// Implementaciones en memoria de las interfaces de persistencia. Sirven para
// pruebas, desarrollo y como referencia de la semántica que deben respetar las
// versiones reales de ops/addons. No son para producción.

// MemoryTarget es un Target en memoria.
type MemoryTarget struct {
	mu      sync.Mutex
	rows    map[string]Existing // org|entity|key
	applies int
	// FailApply, si no es nil, se invoca antes de cada lote y puede devolver un error.
	FailApply func(batch int) error
}

func NewMemoryTarget() *MemoryTarget { return &MemoryTarget{rows: map[string]Existing{}} }

func mk(parts ...string) string { return strings.Join(parts, "\x00") }

func (t *MemoryTarget) Lookup(_ context.Context, org, entity string, keys []string) (map[string]Existing, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := map[string]Existing{}
	for _, k := range keys {
		if e, ok := t.rows[mk(org, entity, k)]; ok {
			out[k] = e
		}
	}
	return out, nil
}

func (t *MemoryTarget) Apply(_ context.Context, org, entity string, changes []Change) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.applies++
	n := t.applies
	if t.FailApply != nil {
		t.mu.Unlock() // el gancho puede bloquear (pruebas); no retiene el candado
		err := t.FailApply(n)
		t.mu.Lock()
		if err != nil {
			return err
		}
	}
	for _, c := range changes {
		t.rows[mk(org, entity, c.Key)] = Existing{Record: c.Record, Hash: c.Hash}
	}
	return nil
}

// Get devuelve lo guardado para una identidad.
func (t *MemoryTarget) Get(org, entity, key string) (Existing, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	e, ok := t.rows[mk(org, entity, key)]
	return e, ok
}

// Len cuenta los registros de una organización y entidad.
func (t *MemoryTarget) Len(org, entity string) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	n := 0
	for k := range t.rows {
		if strings.HasPrefix(k, mk(org, entity)+"\x00") {
			n++
		}
	}
	return n
}

// MemoryLedger es un Ledger en memoria.
type MemoryLedger struct {
	mu   sync.Mutex
	runs []Run // en orden de inserción
}

func NewMemoryLedger() *MemoryLedger { return &MemoryLedger{} }

func (l *MemoryLedger) Save(_ context.Context, run Run) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	for i := range l.runs {
		if l.runs[i].ID == run.ID {
			run.Lease = l.runs[i].Lease // el arrendamiento solo lo mueve el contrato de LeaseLedger
			if run.State.Terminal() {
				run.Lease = nil
			}
			l.runs[i] = run
			return nil
		}
	}
	if run.State.Terminal() {
		run.Lease = nil
	}
	l.runs = append(l.runs, run)
	return nil
}

var _ LeaseLedger = (*MemoryLedger)(nil)

func (l *MemoryLedger) SaveLeased(_ context.Context, run Run, owner string, now time.Time, ttl time.Duration) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	for i := range l.runs {
		if l.runs[i].ID != run.ID || l.runs[i].Org != run.Org {
			continue
		}
		cur := l.runs[i]
		if cur.State.Terminal() || cur.Lease == nil || cur.Lease.Owner != owner {
			return ErrLeaseLost
		}
		run.Lease = &RunLease{Owner: owner, Heartbeat: now, Until: now.Add(ttl)}
		if run.State.Terminal() {
			run.Lease = nil
		}
		l.runs[i] = run
		return nil
	}
	return ErrRunNotFound
}

func (l *MemoryLedger) Heartbeat(_ context.Context, org, id, owner string, now time.Time, ttl time.Duration) (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for i := range l.runs {
		r := &l.runs[i]
		if r.ID == id && r.Org == org {
			if r.State.Terminal() || r.Lease == nil || r.Lease.Owner != owner {
				return false, nil
			}
			r.Lease = &RunLease{Owner: owner, Heartbeat: now, Until: now.Add(ttl)}
			return true, nil
		}
	}
	return false, ErrRunNotFound
}

func (l *MemoryLedger) ClaimStale(_ context.Context, org string, now time.Time, owner string, ttl time.Duration, limit int) ([]Run, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []Run
	for i := range l.runs {
		r := &l.runs[i]
		if limit > 0 && len(out) >= limit {
			break
		}
		if (org != "" && r.Org != org) || r.Mode != ModeApply || (r.State != RunRunning && r.State != RunQueued) {
			continue
		}
		if r.Lease != nil && r.Lease.Until.After(now) {
			continue
		}
		r.Lease = &RunLease{Owner: owner, Heartbeat: now, Until: now.Add(ttl)}
		cp := Checkpoint{}
		if r.Checkpoint != nil {
			cp = *r.Checkpoint
		}
		cp.Resumes++
		r.Checkpoint = &cp
		out = append(out, *r)
	}
	return out, nil
}

func (l *MemoryLedger) FindActive(_ context.Context, org, key string, now time.Time) (Run, bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for i := len(l.runs) - 1; i >= 0; i-- {
		r := l.runs[i]
		if r.Org == org && r.Key == key && r.Mode == ModeApply && (r.State == RunRunning || r.State == RunQueued) && r.Lease != nil && r.Lease.Until.After(now) {
			return r, true, nil
		}
	}
	return Run{}, false, nil
}

func (l *MemoryLedger) Get(_ context.Context, org, id string) (Run, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, r := range l.runs {
		if r.ID == id && r.Org == org {
			return r, nil
		}
	}
	return Run{}, ErrRunNotFound
}

func (l *MemoryLedger) FindApplied(_ context.Context, org, key string) (Run, bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for i := len(l.runs) - 1; i >= 0; i-- {
		r := l.runs[i]
		if r.Org == org && r.Key == key && r.Mode == ModeApply && r.State == RunSucceeded && r.ReplayOf == "" {
			return r, true, nil
		}
	}
	return Run{}, false, nil
}

func (l *MemoryLedger) List(_ context.Context, org string, limit int) ([]Run, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []Run
	for i := len(l.runs) - 1; i >= 0; i-- {
		if l.runs[i].Org == org {
			out = append(out, l.runs[i])
			if limit > 0 && len(out) == limit {
				break
			}
		}
	}
	return out, nil
}

// MemoryManifest es un Manifest en memoria.
type MemoryManifest struct {
	mu sync.Mutex
	m  map[string]ManifestEntry
}

func NewMemoryManifest() *MemoryManifest { return &MemoryManifest{m: map[string]ManifestEntry{}} }

func (m *MemoryManifest) Put(_ context.Context, e ManifestEntry) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.m[mk(e.Org, e.SourceURL)] = e
	return nil
}

func (m *MemoryManifest) ByURL(_ context.Context, org, url string) (ManifestEntry, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.m[mk(org, url)]
	return e, ok, nil
}

func (m *MemoryManifest) List(_ context.Context, org string) ([]ManifestEntry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []ManifestEntry
	for _, e := range m.m {
		if e.Org == org {
			out = append(out, e)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].SourceURL < out[j].SourceURL })
	return out, nil
}

// MemoryBackupStore es un BackupStore en memoria, direccionado por sha256. Un
// objeto que ya existe nunca se reescribe (inmutabilidad).
type MemoryBackupStore struct {
	mu      sync.Mutex
	Objects map[string][]byte // storedName -> bytes
	Puts    int               // escrituras reales
}

func NewMemoryBackupStore() *MemoryBackupStore {
	return &MemoryBackupStore{Objects: map[string][]byte{}}
}

func hashedName(sha string) string { return "originals/" + sha[:2] + "/" + sha }

func (s *MemoryBackupStore) Put(ctx context.Context, name string, r io.Reader, ct string) (string, error) {
	b, err := io.ReadAll(r)
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Puts++
	s.Objects["plain/"+name] = b
	return "plain/" + name, nil
}

func (s *MemoryBackupStore) Lookup(_ context.Context, sha string) (string, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.Objects[hashedName(sha)]
	return hashedName(sha), ok, nil
}

func (s *MemoryBackupStore) PutHashed(_ context.Context, sha, _ string, r io.Reader, _ string) (string, error) {
	b, err := io.ReadAll(r)
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.Objects[hashedName(sha)]; exists {
		return hashedName(sha), nil
	}
	s.Puts++
	s.Objects[hashedName(sha)] = b
	return hashedName(sha), nil
}

func (s *MemoryBackupStore) Open(_ context.Context, name string) (io.ReadCloser, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.Objects[name]
	if !ok {
		return nil, newErr(CodeObjectNotFound, "name", name)
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}

// MemoryJobStore es un MediaJobStore en memoria.
type MemoryJobStore struct {
	mu   sync.Mutex
	jobs map[string]MediaJob
	ord  []string
}

func NewMemoryJobStore() *MemoryJobStore { return &MemoryJobStore{jobs: map[string]MediaJob{}} }

func (s *MemoryJobStore) Enqueue(_ context.Context, jobs []MediaJob) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, j := range jobs {
		if _, ok := s.jobs[j.ID]; ok {
			continue
		}
		s.jobs[j.ID] = j
		s.ord = append(s.ord, j.ID)
		n++
	}
	return n, nil
}

func (s *MemoryJobStore) Claim(_ context.Context, now time.Time, limit int, lease time.Duration) ([]MediaJob, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []MediaJob
	for _, id := range s.ord {
		if len(out) >= limit {
			break
		}
		j := s.jobs[id]
		due := (j.State == MediaPending || j.State == MediaRunning) && !j.NextAttempt.After(now)
		if !due {
			continue
		}
		j.State, j.NextAttempt = MediaRunning, now.Add(lease)
		s.jobs[id] = j
		out = append(out, j)
	}
	return out, nil
}

func (s *MemoryJobStore) Update(_ context.Context, j MediaJob) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.jobs[j.ID]; !ok {
		return newErr(CodeJobNotFound, "id", j.ID)
	}
	s.jobs[j.ID] = j
	return nil
}

func (s *MemoryJobStore) List(_ context.Context, org string, f MediaJobFilter) ([]MediaJob, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []MediaJob
	for _, id := range s.ord {
		j := s.jobs[id]
		if j.Org != org || (f.RunID != "" && j.RunID != f.RunID) || (f.State != "" && j.State != f.State) ||
			(f.Entity != "" && j.Entity != f.Entity) || (f.Key != "" && j.Key != f.Key) || (f.Field != "" && j.Field != f.Field) {
			continue
		}
		out = append(out, j)
	}
	return out, nil
}

func (s *MemoryJobStore) RequeueDead(_ context.Context, org, runID string, now time.Time) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for id, j := range s.jobs {
		if j.Org == org && j.State == MediaDead && (runID == "" || j.RunID == runID) {
			j.State, j.Attempts, j.NextAttempt, j.LastError = MediaPending, 0, now, ""
			s.jobs[id] = j
			n++
		}
	}
	return n, nil
}

// MemorySink es un MediaSink en memoria que recuerda lo último ligado a cada campo.
type MemorySink struct {
	mu      sync.Mutex
	Refs    map[string][]MediaRef
	Attachs int
}

func NewMemorySink() *MemorySink { return &MemorySink{Refs: map[string][]MediaRef{}} }

func (s *MemorySink) Attach(_ context.Context, org, entity, key, field string, refs []MediaRef) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Attachs++
	s.Refs[mk(org, entity, key, field)] = refs
	return nil
}

func (s *MemorySink) Get(org, entity, key, field string) []MediaRef {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Refs[mk(org, entity, key, field)]
}

// Batches devuelve cuántos lotes se han recibido.
func (t *MemoryTarget) Batches() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.applies
}
