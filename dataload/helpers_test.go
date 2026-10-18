package dataload

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/asteby/metacore-kernel/importer"
)

// jpeg mínimo con magic bytes válidos y un sufijo para distinguir contenidos.
func jpegBytes(tag string) []byte {
	return append([]byte{0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x10, 'J', 'F', 'I', 'F', 0x00}, []byte("img-"+tag)...)
}

// clock es un reloj controlable.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *clock { return &clock{t: time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)} }
func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}
func (c *clock) Advance(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }

// origin es un sitio de origen falso: sirve /img/<tag>.jpg y cuenta descargas.
type origin struct {
	*httptest.Server
	hits  sync.Map // path -> *int32
	flaky sync.Map // path -> cuántos 503 devolver antes de servir
}

func newOrigin(t *testing.T) *origin {
	o := &origin{}
	o.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, _ := o.hits.LoadOrStore(r.URL.Path, new(int32))
		atomic.AddInt32(c.(*int32), 1)
		if f, ok := o.flaky.Load(r.URL.Path); ok {
			if left := f.(*int32); atomic.AddInt32(left, -1) >= 0 {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
		}
		switch {
		case strings.HasPrefix(r.URL.Path, "/img/"):
			w.Header().Set("Content-Type", "image/jpeg")
			_, _ = w.Write(jpegBytes(strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/img/"), ".jpg")))
		case strings.HasPrefix(r.URL.Path, "/same/"): // contenido idéntico bajo URLs distintas
			_, _ = w.Write(jpegBytes("same"))
		case r.URL.Path == "/notimage":
			_, _ = w.Write([]byte("<html>no soy imagen</html>"))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(o.Close)
	return o
}

func (o *origin) Hits(path string) int {
	if c, ok := o.hits.Load(path); ok {
		return int(atomic.LoadInt32(c.(*int32)))
	}
	return 0
}

func (o *origin) Flaky(path string, failures int32) { o.flaky.Store(path, &failures) }
func (o *origin) URL(path string) string            { return o.Server.URL + path }

// rig arma todo el sistema en memoria.
type rig struct {
	clk      *clock
	origin   *origin
	target   *MemoryTarget
	ledger   *MemoryLedger
	store    *MemoryBackupStore
	manifest *MemoryManifest
	jobs     *MemoryJobStore
	sink     *MemorySink
	queue    *MediaQueue
	runner   *Runner
}

func newRig(t *testing.T) *rig {
	t.Helper()
	r := &rig{clk: newClock(), origin: newOrigin(t), target: NewMemoryTarget(), ledger: NewMemoryLedger(),
		store: NewMemoryBackupStore(), manifest: NewMemoryManifest(), jobs: NewMemoryJobStore(), sink: NewMemorySink()}
	r.queue = &MediaQueue{
		Jobs: r.jobs, Sink: r.sink, Now: r.clk.Now, Workers: 3,
		Backup: &Backuper{Store: r.store, Manifest: r.manifest, Now: r.clk.Now, Fetch: importer.TransformDeps{
			// El cliente del test llega a 127.0.0.1; en producción se usa el de netguard.
			HTTPClient: &http.Client{Timeout: 5 * time.Second},
			Sleep:      func(context.Context, time.Duration) error { return nil },
			Policy:     importer.MediaPolicy{MaxAttempts: 1},
		}},
	}
	r.runner = &Runner{Ledger: r.ledger, Target: r.target, Media: r.queue, Now: r.clk.Now, BatchSize: 2}
	t.Cleanup(r.runner.Close)
	return r
}

// drain procesa la cola avanzando el reloj hasta que no quede nada pendiente.
func (r *rig) drain(t *testing.T, org string) {
	t.Helper()
	for i := 0; i < 20; i++ {
		n, err := r.queue.ProcessOnce(context.Background(), 100)
		if err != nil {
			t.Fatalf("ProcessOnce: %v", err)
		}
		st, _ := r.queue.Stats(context.Background(), org, "")
		if st.Pending == 0 && st.Running == 0 {
			return
		}
		if n == 0 {
			r.clk.Advance(15 * time.Minute)
		}
	}
	t.Fatal("la cola no se vació")
}

// catalogProfile es un perfil de prueba genérico (no de un giro real).
func catalogProfile() Profile {
	return Profile{
		Name: "catalogo-csv", Version: "1", Entity: "item", Key: []string{"external_id"},
		Clean: []string{CleanTrim},
		Fields: []Field{
			{Target: "external_id", From: []string{"ID"}, Required: true},
			{Target: "name", From: []string{"Nombre", "Name"}, Required: true, Clean: []string{CleanTrimQuotes, CleanCollapseSpaces}},
			{Target: "price", From: []string{"Precio"}, Type: "number"},
			{Target: "description", From: []string{"Descripción"}, Clean: []string{CleanUnescapeNewline, CleanStripHTML}},
			{Target: "published", From: []string{"Publicado"}, Type: "boolean", Values: map[string]string{"-1": "0"}},
			{Target: "images", From: []string{"Imágenes"}, Media: true},
		},
	}
}

func csvRows(origin *origin, extra ...string) []byte {
	var b strings.Builder
	b.WriteString("ID,Nombre,Precio,Descripción,Publicado,Imágenes,Meta: _wcct_x\n")
	b.WriteString(fmt.Sprintf("1,\"  WHEY   PROTEIN \",549.50,\"<p>Línea uno\\nLínea dos</p>\",1,\"%s, %s\",x\n", origin.URL("/img/a.jpg"), origin.URL("/img/b.jpg")))
	b.WriteString(fmt.Sprintf("2,CREATINA,299,Sin html,-1,%s,x\n", origin.URL("/img/c.jpg")))
	b.WriteString("3,PREWORKOUT,399,,1,,x\n")
	for _, e := range extra {
		b.WriteString(e + "\n")
	}
	return []byte(b.String())
}

func mustPlan(t *testing.T, req PlanRequest) *Plan {
	t.Helper()
	p, err := BuildPlan(context.Background(), req)
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	return p
}
