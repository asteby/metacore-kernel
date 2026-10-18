package dataload

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// crashAfterWrite simula un proceso que muere ENTRE escribir un lote (con su
// marca) y guardar el checkpoint: el lote número atBatch se escribe y la llamada
// se queda colgada sin volver.
type crashAfterWrite struct {
	*MemoryMarkingTarget
	atBatch int32
	n       int32
	reached chan struct{}
	release chan struct{}
	once    sync.Once
}

func (c *crashAfterWrite) ApplyMarked(ctx context.Context, org, entity string, m ApplyMark, ch []Change) error {
	err := c.MemoryMarkingTarget.ApplyMarked(ctx, org, entity, m, ch)
	if atomic.AddInt32(&c.n, 1) == c.atBatch {
		close(c.reached)
		<-c.release
	}
	return err
}

// Punto 4: con marcas de aplicación, la reanudación recupera el conteo exacto
// de Created aunque el dueño anterior muriera después de escribir y antes del
// checkpoint. Sin marcas ese lote se contaría como unchanged.
func TestMarks_RecoverExactCreatedWhenTheProcessDiesBetweenApplyAndCheckpoint(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	tgt := NewMemoryMarkingTarget()
	crash := &crashAfterWrite{MemoryMarkingTarget: tgt, atBatch: 3, reached: make(chan struct{}), release: make(chan struct{})}
	a := leased(t, r, "A", crash, -1)
	t.Cleanup(func() { crash.once.Do(func() { close(crash.release) }) })
	req := RunRequest{Org: "org-a", Profile: catalogProfile(), Source: NewRowsSource("filas", syntheticRows(10))}
	run, err := a.Start(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	<-crash.reached // lote 3 escrito (6 filas en el destino), checkpoint aún en el lote 2
	if tgt.Len("org-a", "item") != 6 {
		t.Fatalf("filas escritas = %d", tgt.Len("org-a", "item"))
	}
	mid, _ := r.ledger.Get(ctx, "org-a", run.ID)
	if mid.Checkpoint.Batches != 2 || mid.Progress.Done != 4 {
		t.Fatalf("checkpoint = %+v progreso = %+v", mid.Checkpoint, mid.Progress)
	}

	r.clk.Advance(2 * time.Minute)
	b := leased(t, r, "B", tgt, -1)
	if _, err := b.Recover(ctx, "org-a", sameRequest(req)); err != nil {
		t.Fatal(err)
	}
	done, _ := b.Wait(ctx, "org-a", run.ID)
	p := done.Progress
	if done.State != RunSucceeded || p.Created != 10 || p.Done != 10 || p.Total != 10 || p.Unchanged != 0 {
		t.Fatalf("conteo exacto: %+v", p)
	}
	if done.Checkpoint.Recovered != 2 || done.Checkpoint.Batches != 5 || done.Checkpoint.Rows != 10 {
		t.Fatalf("checkpoint = %+v", done.Checkpoint)
	}
	if tgt.Len("org-a", "item") != 10 {
		t.Fatal("sin duplicados")
	}
}

// plainCrash es lo mismo sobre un Target sin marcas.
type plainCrash struct {
	*MemoryTarget
	n       int32
	reached chan struct{}
	release chan struct{}
}

func (c *plainCrash) Apply(ctx context.Context, org, entity string, ch []Change) error {
	err := c.MemoryTarget.Apply(ctx, org, entity, ch)
	if atomic.AddInt32(&c.n, 1) == 3 {
		close(c.reached)
		<-c.release
	}
	return err
}

// Punto 4 (referencia): sin marcas, los datos quedan bien pero el lote perdido
// se cuenta como unchanged. Es el hueco que cierra MarkingTarget.
func TestMarks_WithoutMarksTheLostBatchCountsAsUnchanged(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	crash := &plainCrash{MemoryTarget: r.target, reached: make(chan struct{}), release: make(chan struct{})}
	a := leased(t, r, "A", crash, -1)
	t.Cleanup(func() { close(crash.release) })
	req := RunRequest{Org: "org-a", Profile: catalogProfile(), Source: NewRowsSource("filas", syntheticRows(10))}
	run, _ := a.Start(ctx, req)
	<-crash.reached
	r.clk.Advance(2 * time.Minute)
	b := leased(t, r, "B", r.target, -1)
	_, _ = b.Recover(ctx, "org-a", sameRequest(req))
	done, _ := b.Wait(ctx, "org-a", run.ID)
	if done.State != RunSucceeded || done.Progress.Created != 8 || done.Progress.Unchanged != 2 || r.target.Len("org-a", "item") != 10 {
		t.Fatalf("sin marcas: %+v filas=%d", done.Progress, r.target.Len("org-a", "item"))
	}
}

// Punto 4: si el dueño anterior despierta y escribe tarde un lote con el mismo
// número, cuenta la marca de la generación más nueva.
func TestMarks_LatestGenerationWinsPerBatch(t *testing.T) {
	got := latestMarks([]ApplyMark{
		{Batch: 3, Generation: 0, Created: 2},
		{Batch: 3, Generation: 1, Created: 1, Updated: 1},
		{Batch: 4, Generation: 1, Created: 2},
	})
	if len(got) != 2 || got[0].Batch != 3 || got[0].Generation != 1 || got[0].Updated != 1 || got[1].Batch != 4 {
		t.Fatalf("got = %+v", got)
	}
}

// hangBeforeApply simula un proceso que muere ANTES de escribir su primer lote:
// la primera llamada a ApplyMarked se queda colgada sin escribir nada.
type hangBeforeApply struct {
	*MemoryMarkingTarget
	n       int32
	reached chan struct{}
	release chan struct{}
}

func (h *hangBeforeApply) ApplyMarked(ctx context.Context, org, entity string, m ApplyMark, ch []Change) error {
	if atomic.AddInt32(&h.n, 1) == 1 {
		close(h.reached)
		<-h.release
		return context.Canceled
	}
	return h.MemoryMarkingTarget.ApplyMarked(ctx, org, entity, m, ch)
}

// Las marcas que una reanudación ya sumó al avance no se vuelven a sumar si esa
// reanudación también muere antes de su primer checkpoint.
func TestMarks_RecoveredMarksAreNotCountedTwiceAcrossTwoResumes(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	tgt := NewMemoryMarkingTarget()
	crash := &crashAfterWrite{MemoryMarkingTarget: tgt, atBatch: 3, reached: make(chan struct{}), release: make(chan struct{})}
	a := leased(t, r, "A", crash, -1)
	t.Cleanup(func() { crash.once.Do(func() { close(crash.release) }) })
	req := RunRequest{Org: "org-a", Profile: catalogProfile(), Source: NewRowsSource("filas", syntheticRows(10))}
	run, err := a.Start(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	<-crash.reached // lote 3 escrito con marca; checkpoint en el lote 2

	r.clk.Advance(2 * time.Minute)
	hang := &hangBeforeApply{MemoryMarkingTarget: tgt, reached: make(chan struct{}), release: make(chan struct{})}
	b := leased(t, r, "B", hang, -1)
	t.Cleanup(func() { close(hang.release) })
	if _, err := b.Recover(ctx, "org-a", sameRequest(req)); err != nil {
		t.Fatal(err)
	}
	<-hang.reached // B sumó la marca del lote 3 y murió antes de escribir

	r.clk.Advance(2 * time.Minute)
	c := leased(t, r, "C", tgt, -1)
	if _, err := c.Recover(ctx, "org-a", sameRequest(req)); err != nil {
		t.Fatal(err)
	}
	done, _ := c.Wait(ctx, "org-a", run.ID)
	if p := done.Progress; done.State != RunSucceeded || p.Created != 10 || p.Done != 10 || p.Unchanged != 0 {
		t.Fatalf("conteo exacto tras dos reanudaciones: %+v", p)
	}
	if tgt.Len("org-a", "item") != 10 {
		t.Fatal("sin duplicados")
	}
}
