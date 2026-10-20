package legacybridge

import (
	"context"
	"fmt"
	"math/rand/v2"
	"strings"
	"sync"
	"testing"
)

// orderedSink falla la prueba si una versión más vieja de una fila se escribe
// después de una más nueva (lo que pasaría sin el candado por fila).
type orderedSink struct {
	*fakeSink
	mu    sync.Mutex
	last  map[string]int
	wrong []string
}

func (o *orderedSink) Upsert(ctx context.Context, w Write) error {
	v := w.Envelope.Aggregate.Root["v"].(int)
	o.mu.Lock()
	key := w.Envelope.ExternalID
	if prev, ok := o.last[key]; ok && v < prev {
		o.wrong = append(o.wrong, fmt.Sprintf("%s: v%d después de v%d", key, v, prev))
	}
	if v > o.last[key] {
		o.last[key] = v
	}
	o.mu.Unlock()
	return o.fakeSink.Upsert(ctx, w)
}

func customerAt(ext string, v int) Envelope {
	return Envelope{Source: "erp", Entity: "customers.Customer", ExternalID: ext, Op: OpUpsert,
		SourceVersion: ver(v, fmt.Sprintf("h%d", v)), Aggregate: Aggregate{Root: map[string]any{"v": v}}}
}

// Dos aplicadores (como dos procesos: candados de proceso distintos) aplican
// las mismas versiones de unas filas en desorden y a la vez. Al final cada fila
// queda en su versión más nueva y ninguna versión vieja se escribió encima de
// una nueva. En Postgres lo garantiza pg_advisory_xact_lock; en memoria, el
// candado del proceso (un solo aplicador).
func TestApply_ConcurrentRunsKeepPerRowOrder(t *testing.T) {
	run := func(t *testing.T, s Store, appliers int) {
		sink := &orderedSink{fakeSink: newFakeSink(), last: map[string]int{}}
		var aps []*Applier
		for i := 0; i < appliers; i++ {
			cfg := Config{Store: s, Sink: sink}
			if g, ok := s.(*GormStore); ok {
				cfg.Atomic = GormAtomic(g.db)
			}
			ap, err := NewApplier(cfg)
			if err != nil {
				t.Fatal(err)
			}
			aps = append(aps, ap)
		}
		const rows, versions, workers = 3, 30, 6
		ctx := context.Background()
		var wg sync.WaitGroup
		for w := 0; w < workers; w++ {
			wg.Add(1)
			go func(w int) {
				defer wg.Done()
				ap := aps[w%len(aps)]
				var envs []Envelope
				for r := 0; r < rows; r++ {
					for v := 1; v <= versions; v++ {
						envs = append(envs, customerAt(fmt.Sprintf("erp:clients:%d", r), v))
					}
				}
				rand.Shuffle(len(envs), func(i, j int) { envs[i], envs[j] = envs[j], envs[i] })
				for _, e := range envs {
					if res := ap.Apply(ctx, org, e); res.Outcome == OutcomeError {
						t.Errorf("apply: %s", res.Detail)
						return
					}
				}
			}(w)
		}
		wg.Wait()
		if len(sink.wrong) > 0 {
			t.Fatalf("versiones fuera de orden: %s", strings.Join(sink.wrong[:min(5, len(sink.wrong))], "; "))
		}
		for r := 0; r < rows; r++ {
			ref, ok, err := s.GetRef(ctx, org, "erp", "customers.Customer", fmt.Sprintf("erp:clients:%d", r))
			if err != nil || !ok {
				t.Fatalf("ref %d: %v %v", r, ok, err)
			}
			if ref.SourceVersion != ver(versions, fmt.Sprintf("h%d", versions)) {
				t.Fatalf("fila %d quedó en %s", r, ref.SourceVersion)
			}
		}
	}
	t.Run("memory", func(t *testing.T) { run(t, NewMemoryStore(), 1) })
	t.Run("postgres", func(t *testing.T) { run(t, gormStore(t, postgresDB(t)), 2) })
}

// Lotes simultáneos del mismo flujo con cursores en desorden: el cursor
// guardado termina en el mayor y nunca retrocede.
func TestIngest_ConcurrentBatchesNeverMoveCursorBack(t *testing.T) {
	run := func(t *testing.T, s Store, appliers int) {
		var aps []*Applier
		for i := 0; i < appliers; i++ {
			cfg := Config{Store: s, Sink: newFakeSink()}
			if g, ok := s.(*GormStore); ok {
				cfg.Atomic = GormAtomic(g.db)
			}
			ap, err := NewApplier(cfg)
			if err != nil {
				t.Fatal(err)
			}
			aps = append(aps, ap)
		}
		ctx := context.Background()
		const n = 40
		cursors := make([]string, n)
		for i := range cursors {
			// Mismo instante para la mitad: el desempate ordena.
			cursors[i] = ver(i/2, fmt.Sprintf("%020d", i))
		}
		rand.Shuffle(n, func(i, j int) { cursors[i], cursors[j] = cursors[j], cursors[i] })
		var wg sync.WaitGroup
		var mu sync.Mutex
		seen := ""
		for i, c := range cursors {
			wg.Add(1)
			go func(i int, c string) {
				defer wg.Done()
				res, err := aps[i%len(aps)].Ingest(ctx, org, Batch{Source: "erp", Stream: "clients", Cursor: c})
				if err != nil {
					t.Error(err)
					return
				}
				mu.Lock()
				defer mu.Unlock()
				if res.Advanced && CompareCursors(res.Cursor, seen) > 0 {
					seen = res.Cursor
				}
			}(i, c)
		}
		wg.Wait()
		got, _ := s.GetCursor(ctx, org, "erp", "clients")
		want := ver((n-1)/2, fmt.Sprintf("%020d", n-1))
		if got != want {
			t.Fatalf("cursor = %s, quería %s", got, want)
		}
	}
	t.Run("memory", func(t *testing.T) { run(t, NewMemoryStore(), 1) })
	t.Run("postgres", func(t *testing.T) { run(t, gormStore(t, postgresDB(t)), 2) })
}

func TestCompareCursors(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"", "", 0},
		{ver(1, ""), "", 1},
		{ver(1, "002"), ver(1, "010"), -1},
		{ver(2, "001"), ver(1, "999"), 1},
		{ver(1, "5"), ver(1, "5"), 0},
		{"2026-10-18T10:00:01.5Z", "2026-10-18T10:00:01Z", 1},
	}
	for _, c := range cases {
		if got := CompareCursors(c.a, c.b); got != c.want {
			t.Errorf("CompareCursors(%q, %q) = %d, quería %d", c.a, c.b, got, c.want)
		}
	}
}

func TestIngest_RejectsOversizedBatchAndFields(t *testing.T) {
	e := newApplyEnv(t, NewMemoryStore())
	ctx := context.Background()
	big := Batch{Source: "erp", Envelopes: make([]Envelope, MaxBatchEnvelopes+1)}
	if _, err := e.ap.Ingest(ctx, org, big); err == nil {
		t.Fatal("un lote de más de MaxBatchEnvelopes debe rechazarse")
	}
	env := customerAt("erp:clients:"+strings.Repeat("9", maxIDLen), 1)
	if res := e.ap.Apply(ctx, org, env); res.Outcome != OutcomeReview {
		t.Fatalf("external_id gigante = %s", res.Outcome)
	}
	env = customerAt("erp:clients:1", 1)
	env.Requires = make([]Dependency, MaxRequires+1)
	for i := range env.Requires {
		env.Requires[i] = Dependency{Entity: "x", ExternalID: fmt.Sprint(i)}
	}
	if res := e.ap.Apply(ctx, org, env); res.Outcome != OutcomeReview {
		t.Fatalf("demasiadas dependencias = %s", res.Outcome)
	}
}

func TestListRefs_PagesByExternalIDWithinOrg(t *testing.T) {
	eachStore(t, func(t *testing.T, s Store) {
		ctx := context.Background()
		for i := 0; i < 5; i++ {
			for _, o := range []string{org, "otra"} {
				if err := s.PutRef(ctx, Ref{Org: o, System: "erp", Entity: "products", ExternalID: fmt.Sprintf("erp:products:%d", i), LocalID: fmt.Sprintf("%s-%d", o, i)}); err != nil {
					t.Fatal(err)
				}
			}
		}
		var got []string
		after := ""
		for {
			page, err := s.ListRefs(ctx, org, "erp", "products", after, 2)
			if err != nil {
				t.Fatal(err)
			}
			if len(page) == 0 {
				break
			}
			for _, r := range page {
				if r.Org != org {
					t.Fatalf("ref de otra organización: %+v", r)
				}
				got = append(got, r.ExternalID)
			}
			after = page[len(page)-1].ExternalID
		}
		if len(got) != 5 || got[0] != "erp:products:0" || got[4] != "erp:products:4" {
			t.Fatalf("páginas = %v", got)
		}
	})
}
