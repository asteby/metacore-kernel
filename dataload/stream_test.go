package dataload

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// countingOpener abre siempre el mismo contenido y cuenta las pasadas.
type countingOpener struct {
	body  func(n int) []byte // n = número de apertura (1, 2, ...)
	opens int32
}

func (o *countingOpener) open() (io.ReadCloser, error) {
	n := atomic.AddInt32(&o.opens, 1)
	return io.NopCloser(bytes.NewReader(o.body(int(n)))), nil
}

func fixed(b []byte) *countingOpener { return &countingOpener{body: func(int) []byte { return b }} }

func collectStream(t *testing.T, src StreamSource) []map[string]any {
	t.Helper()
	var out []map[string]any
	if err := src.Each(context.Background(), func(r map[string]any) error { out = append(out, r); return nil }); err != nil {
		t.Fatal(err)
	}
	return out
}

// Punto 1: los adaptadores en streaming entregan exactamente las filas de sus
// equivalentes en memoria (BOM, filas irregulares, tipos JSON) y la misma huella.
func TestStream_CSVAndJSONLinesYieldTheSameRowsAndHashAsInMemory(t *testing.T) {
	csvBody := []byte("\xef\xbb\xbfID,Nombre,Precio\n1,\"UNO, DOS\",10\n2,DOS\n3,TRES,30,extra\n")
	mem, _ := NewCSVSource("a.csv", csvBody).Rows(context.Background())
	got := collectStream(t, NewCSVStreamSource("a.csv", fixed(csvBody).open))
	if !reflect.DeepEqual(mem, got) || inputHash(mem) != inputHash(got) {
		t.Fatalf("csv:\n mem=%v\nflujo=%v", mem, got)
	}

	jsonArr := []byte(`[{"ID":"1","Precio":10.5,"tags":["a"]},{"ID":"2","activo":true,"n":null}]`)
	jsonl := []byte("{\"ID\":\"1\",\"Precio\":10.5,\"tags\":[\"a\"]}\n\n  {\"ID\":\"2\",\"activo\":true,\"n\":null}  \n")
	memJ, _ := NewJSONSource("a.json", jsonArr).Rows(context.Background())
	gotJ := collectStream(t, NewJSONLinesSource("a.jsonl", fixed(jsonl).open))
	if !reflect.DeepEqual(memJ, gotJ) || inputHash(memJ) != inputHash(gotJ) {
		t.Fatalf("json:\n mem=%v\nflujo=%v", memJ, gotJ)
	}

	bad := NewJSONLinesSource("b.jsonl", fixed([]byte("{\"ID\":\"1\"}\n[1,2]\n")).open)
	err := bad.Each(context.Background(), func(map[string]any) error { return nil })
	if m, ok := ErrorMessage(err); !ok || m.Code != CodeSourceLine || m.Params["line"] != 2 {
		t.Fatalf("línea que no es objeto: %v", err)
	}
}

// mixedRows: inválidas, duplicadas, nuevas, iguales y cambiadas respecto del destino.
func mixedRows(n int) []byte {
	var b strings.Builder
	b.WriteString("ID,Nombre,Precio,Imágenes\n")
	for i := 1; i <= n; i++ {
		switch {
		case i%11 == 0:
			fmt.Fprintf(&b, ",SIN ID %d,1,\n", i) // inválida: falta la identidad
		case i%7 == 0:
			fmt.Fprintf(&b, "i%04d,REPETIDA %d,2,\n", i-1, i) // duplicada de la anterior
		case i%5 == 0:
			fmt.Fprintf(&b, "i%04d,ARTICULO %d,abc,\n", i, i) // inválida: precio no numérico
		default:
			fmt.Fprintf(&b, "i%04d,ARTICULO %d,%d,https://cdn.example.test/%d.jpg\n", i, i, i, i)
		}
	}
	return []byte(b.String())
}

func seedTarget(t *testing.T, tgt *MemoryTarget, prof Profile, body []byte, every int, mutate bool) {
	t.Helper()
	p := mustPlan(t, PlanRequest{Org: "org-a", Profile: prof, Source: NewCSVSource("seed", body), Target: NewMemoryTarget()})
	var seed []Change
	for i, c := range p.Changes() {
		if i%every != 0 {
			continue
		}
		if mutate && i%(2*every) == 0 {
			rec := map[string]any{}
			for k, v := range c.Record {
				rec[k] = v
			}
			rec["name"] = "VIEJO"
			c.Record, c.Hash = rec, "otro"
		}
		seed = append(seed, c)
	}
	if err := tgt.Apply(context.Background(), "org-a", p.Entity, seed); err != nil {
		t.Fatal(err)
	}
}

// Punto 1: StreamPlan y BuildPlan coinciden fila por fila (acciones, identidad,
// huella, problemas, diffs, motivos), en conteos y en las huellas de entrada y plan.
func TestStream_PlanMatchesBuildPlanRowByRow(t *testing.T) {
	ctx := context.Background()
	check := func(t *testing.T, req PlanRequest, body []byte, csvName string) {
		t.Helper()
		req.Source = NewCSVSource(csvName, body)
		want := mustPlan(t, req)
		req.Source, req.LookupChunk = NewCSVStreamSource(csvName, fixed(body).open), 3
		var rows []RowPlan
		pages := 0
		got, err := StreamPlan(ctx, req, StreamOptions{PageSize: 4, OnPage: func(_ context.Context, page []RowPlan) error {
			pages++
			rows = append(rows, append([]RowPlan(nil), page...)...)
			return nil
		}})
		if err != nil {
			t.Fatal(err)
		}
		if pages < 2 {
			t.Fatalf("debía paginar: %d", pages)
		}
		if got.InputHash != want.InputHash || got.PlanHash != want.PlanHash || !reflect.DeepEqual(got.Counts, want.Counts) ||
			got.MediaURLs != want.MediaURLs || got.IgnoredColumns != want.IgnoredColumns || got.Rows != nil {
			t.Fatalf("resumen distinto:\n flujo=%+v %v\nmemoria=%+v %v", got.Counts, got.PlanHash, want.Counts, want.PlanHash)
		}
		if len(rows) != len(want.Rows) {
			t.Fatalf("filas %d vs %d", len(rows), len(want.Rows))
		}
		for i := range rows {
			if !reflect.DeepEqual(rows[i], want.Rows[i]) {
				t.Fatalf("fila %d distinta:\n flujo=%+v\nmemoria=%+v", i+1, rows[i], want.Rows[i])
			}
		}
	}

	t.Run("catálogo con duplicados, inválidas y diff", func(t *testing.T) {
		prof := catalogProfile()
		body := mixedRows(60)
		tgt := NewMemoryTarget()
		seedTarget(t, tgt, prof, body, 3, true)
		check(t, PlanRequest{Org: "org-a", Profile: prof, Target: tgt}, body, "mixto.csv")
	})
	t.Run("extract con comprobación unique y filas en revisión", func(t *testing.T) {
		prof, ex := loadLista(t)
		check(t, PlanRequest{Org: "org-a", Profile: prof, Extract: ex, Target: NewMemoryTarget()}, listaCSV(listaRows()...), "lista.csv")
	})
}

func writeFile(t *testing.T, body []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "carga.csv")
	if err := os.WriteFile(p, body, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// Punto 1: un archivo mayor que el tope en memoria se aplica entero en
// streaming, por lotes con checkpoint, y repetirlo es un replay.
func TestStream_RunnerAppliesAFileLargerThanTheInMemoryCap(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	prof := catalogProfile()
	prof.MaxRows = 50 // tope en memoria
	var b strings.Builder
	b.WriteString("ID,Nombre,Precio\n")
	for i := 1; i <= 500; i++ {
		fmt.Fprintf(&b, "s%04d,ARTICULO %d,%d\n", i, i, i)
	}
	path := writeFile(t, []byte(b.String()))

	if mem := startAndWait(t, r, RunRequest{Org: "org-a", Profile: prof, Source: NewCSVSource("carga.csv", []byte(b.String()))}); mem.State != RunFailed || r.target.Len("org-a", "item") != 0 {
		t.Fatalf("en memoria debe respetar MaxRows: %s %s", mem.State, mem.Error)
	}
	opens := int32(0)
	open := func() (io.ReadCloser, error) { atomic.AddInt32(&opens, 1); return FileOpener(path)() }
	r.runner.PageSize, r.runner.BatchSize = 64, 25
	req := RunRequest{Org: "org-a", Profile: prof, Source: NewCSVStreamSource("carga.csv", open)}
	done := startAndWait(t, r, req)
	if done.State != RunSucceeded || done.Progress.Created != 500 || done.Progress.Total != 500 || done.Progress.Done != 500 {
		t.Fatalf("%s %+v %s", done.State, done.Progress, done.Error)
	}
	if r.target.Len("org-a", "item") != 500 || done.Checkpoint.Batches != 20 || done.Checkpoint.Rows != 500 {
		t.Fatalf("filas=%d checkpoint=%+v", r.target.Len("org-a", "item"), done.Checkpoint)
	}
	if opens != 3 { // huella para la llave, primera pasada, plan+escritura
		t.Fatalf("pasadas = %d", opens)
	}
	again, err := r.runner.Start(ctx, req)
	if err != nil || again.ReplayOf != done.ID {
		t.Fatalf("misma entrada = replay: %v %+v", err, again)
	}
	// El tope en streaming también existe.
	prof.MaxStreamRows = 100
	_, err = r.runner.Start(ctx, RunRequest{Org: "org-a", Profile: prof, Source: NewCSVStreamSource("carga.csv", open), Force: true})
	if ErrorCode(err) != CodeSourceTooManyRows {
		t.Fatalf("MaxStreamRows: %v", err)
	}
}

// orderTarget registra el orden en que llegan las identidades.
type orderTarget struct {
	*MemoryTarget
	mu   sync.Mutex
	keys []string
}

func (o *orderTarget) Apply(ctx context.Context, org, entity string, ch []Change) error {
	o.mu.Lock()
	for _, c := range ch {
		o.keys = append(o.keys, c.Key)
	}
	o.mu.Unlock()
	return o.MemoryTarget.Apply(ctx, org, entity, ch)
}

// Punto 1: con ApplyOrder el streaming escribe padres antes que variantes en
// todo el archivo (una pasada por rango) y la huella del plan no cambia.
func TestStream_ApplyOrderHoldsAcrossTheWholeFile(t *testing.T) {
	ctx := context.Background()
	prof := Profile{Name: "orden", Version: "1", Entity: "e", Key: []string{"id"},
		Fields:     []Field{{Target: "id", From: []string{"id"}, Required: true}, {Target: "kind", From: []string{"kind"}}},
		ApplyOrder: []OrderRule{{Field: "kind", Rank: []string{"parent", "variant"}}}}
	var b strings.Builder
	b.WriteString("id,kind\n")
	for i := 1; i <= 30; i++ {
		kind := "variant"
		if i%3 == 1 {
			kind = "parent"
		}
		fmt.Fprintf(&b, "k%02d,%s\n", i, kind)
	}
	body := []byte(b.String())
	tgt := &orderTarget{MemoryTarget: NewMemoryTarget()}
	r := &Runner{Ledger: NewMemoryLedger(), Target: tgt, BatchSize: 4, PageSize: 5}
	defer r.Close()
	run, err := r.Start(ctx, RunRequest{Org: "o", Profile: prof, Source: NewCSVStreamSource("orden.csv", fixed(body).open)})
	if err != nil {
		t.Fatal(err)
	}
	done, _ := r.Wait(ctx, "o", run.ID)
	if done.State != RunSucceeded || done.Progress.Created != 30 || len(tgt.keys) != 30 {
		t.Fatalf("%+v %s keys=%d", done.Progress, done.Error, len(tgt.keys))
	}
	for i, k := range tgt.keys {
		parent := (atoi(k[1:])-1)%3 == 0
		if (i < 10) != parent {
			t.Fatalf("orden de escritura: %v", tgt.keys)
		}
	}
	want := mustPlan(t, PlanRequest{Org: "o", Profile: prof, Source: NewCSVSource("orden.csv", body), Target: NewMemoryTarget()})
	if done.PlanHash != want.PlanHash {
		t.Fatal("la huella del plan no depende del camino")
	}
}

func atoi(s string) int {
	n := 0
	for _, c := range s {
		n = n*10 + int(c-'0')
	}
	return n
}

// Punto 1: una corrida en streaming interrumpida se recupera sin duplicar.
func TestStream_RecoverResumesAStreamRunWithoutDuplicating(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	gate := newGate(r.target, 3)
	a := leased(t, r, "A", gate, -1)
	t.Cleanup(gate.Release)
	var b strings.Builder
	b.WriteString("ID,Nombre\n")
	for i := 1; i <= 10; i++ {
		fmt.Fprintf(&b, "r%02d,ARTICULO %d\n", i, i)
	}
	src := fixed([]byte(b.String()))
	req := RunRequest{Org: "org-a", Profile: catalogProfile(), Source: NewCSVStreamSource("r.csv", src.open)}
	run, err := a.Start(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	<-gate.reached
	r.clk.Advance(2 * time.Minute)
	bb := leased(t, r, "B", r.target, -1)
	if rep, err := bb.Recover(ctx, "org-a", sameRequest(req)); err != nil || len(rep.Resumed) != 1 {
		t.Fatalf("%v %+v", err, rep)
	}
	done, _ := bb.Wait(ctx, "org-a", run.ID)
	if done.State != RunSucceeded || done.Progress.Created != 10 || done.Progress.Done != 10 || done.Progress.Unchanged != 0 || done.Progress.Total != 10 {
		t.Fatalf("%+v", done.Progress)
	}
	if r.target.Len("org-a", "item") != 10 {
		t.Fatal("sin duplicados")
	}
}

// Punto 1: si la fuente cambia entre pasadas la corrida falla en vez de mezclar.
func TestStream_SourceChangingBetweenPassesFails(t *testing.T) {
	ctx := context.Background()
	o := &countingOpener{body: func(n int) []byte {
		if n <= 2 {
			return []byte("ID,Nombre\n1,UNO\n")
		}
		return []byte("ID,Nombre\n1,OTRO\n")
	}}
	r := &Runner{Ledger: NewMemoryLedger(), Target: NewMemoryTarget()}
	defer r.Close()
	run, err := r.Start(ctx, RunRequest{Org: "o", Profile: catalogProfile(), Source: NewCSVStreamSource("x.csv", o.open)})
	if err != nil {
		t.Fatal(err)
	}
	done, _ := r.Wait(ctx, "o", run.ID)
	if done.State != RunFailed || done.ErrorCode != CodeSourceChanged {
		t.Fatalf("%+v", done)
	}
}

// Punto 1: FailOnInvalid corta tras la primera pasada sin escribir nada, y el
// dry-run en streaming deja problemas en la corrida sin cargar filas en el plan.
func TestStream_FailOnInvalidAndDryRun(t *testing.T) {
	ctx := context.Background()
	body := mixedRows(30)
	tgt := NewMemoryTarget()
	r := &Runner{Ledger: NewMemoryLedger(), Target: tgt}
	defer r.Close()
	run, _ := r.Start(ctx, RunRequest{Org: "o", Profile: catalogProfile(), Source: NewCSVStreamSource("m.csv", fixed(body).open), FailOnInvalid: true})
	done, _ := r.Wait(ctx, "o", run.ID)
	if done.State != RunFailed || done.ErrorCode != CodeRunInvalidRows || tgt.Batches() != 0 {
		t.Fatalf("%+v batches=%d", done, tgt.Batches())
	}
	plan, dry, err := r.DryRun(ctx, RunRequest{Org: "o", Profile: catalogProfile(), Source: NewCSVStreamSource("m.csv", fixed(body).open)})
	if err != nil || plan.Rows != nil || len(dry.Issues) == 0 || dry.Progress.Invalid == 0 || dry.PlanHash == "" {
		t.Fatalf("%v rows=%d %+v", err, len(plan.Rows), dry.Progress)
	}
	mem := mustPlan(t, PlanRequest{Org: "o", Profile: catalogProfile(), Source: NewCSVSource("m.csv", body), Target: tgt})
	if dry.PlanHash != mem.PlanHash || len(dry.Issues) != len(mem.issues()) {
		t.Fatal("dry-run en streaming = dry-run en memoria")
	}
}

// Punto 1: el streaming también funciona con revisiones y medios por página.
func TestStream_ReviewsAndMediaArePagedLikeInMemory(t *testing.T) {
	ctx := context.Background()
	run := func(stream bool) (*rig, Run) {
		r := newRig(t)
		r.runner.Reviews = NewMemoryReviewStore()
		r.runner.PageSize = 3
		req := listaRequest(t)
		if stream {
			req.Source = NewCSVStreamSource("lista.csv", fixed(listaCSV(listaRows()...)).open)
		}
		return r, startAndWait(t, r, req)
	}
	rm, mem := run(false)
	rs, str := run(true)
	if str.State != RunSucceeded || str.PlanHash != mem.PlanHash || !reflect.DeepEqual(str.Progress, mem.Progress) {
		t.Fatalf("progreso:\n flujo=%+v\nmemoria=%+v", str.Progress, mem.Progress)
	}
	if !reflect.DeepEqual(str.Review, mem.Review) || !reflect.DeepEqual(str.Issues, mem.Issues) {
		t.Fatal("revisiones y problemas iguales")
	}
	pm, _ := rm.runner.Reviews.List(ctx, "org-a", ReviewFilter{})
	ps, _ := rs.runner.Reviews.List(ctx, "org-a", ReviewFilter{})
	if len(pm) != len(ps) || len(ps) == 0 {
		t.Fatalf("filas en revisión %d vs %d", len(ps), len(pm))
	}
	if rm.target.Len("org-a", "product") != rs.target.Len("org-a", "product") {
		t.Fatal("mismo destino")
	}
}
