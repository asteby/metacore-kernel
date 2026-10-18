package dataload

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func syntheticRows(n int) []map[string]any {
	rows := make([]map[string]any, n)
	for i := range rows {
		rows[i] = map[string]any{"ID": fmt.Sprintf("i%04d", i+1), "Nombre": fmt.Sprintf("ARTICULO %d", i+1), "Precio": "10"}
	}
	return rows
}

// gateTarget simula un proceso colgado: la llamada número atBatch a Apply se
// queda esperando (sin mirar el contexto) hasta que el test la libera.
type gateTarget struct {
	*MemoryTarget
	atBatch int32
	reached chan struct{}
	release chan struct{}
	n       int32
	once    sync.Once
}

func newGate(inner *MemoryTarget, atBatch int) *gateTarget {
	return &gateTarget{MemoryTarget: inner, atBatch: int32(atBatch), reached: make(chan struct{}), release: make(chan struct{})}
}

func (g *gateTarget) Apply(ctx context.Context, org, entity string, ch []Change) error {
	if atomic.AddInt32(&g.n, 1) == g.atBatch {
		close(g.reached)
		<-g.release
	}
	return g.MemoryTarget.Apply(ctx, org, entity, ch)
}

func (g *gateTarget) Release() { g.once.Do(func() { close(g.release) }) }

// leased arma un Runner con arrendamientos sobre la persistencia del rig.
func leased(t *testing.T, r *rig, owner string, target Target, hb time.Duration) *Runner {
	t.Helper()
	rn := &Runner{Ledger: r.ledger, Target: target, Now: r.clk.Now, BatchSize: 2, Owner: owner, LeaseTTL: time.Minute, HeartbeatEvery: hb}
	t.Cleanup(rn.Close)
	return rn
}

func sameRequest(req RunRequest) RequestResolver {
	return func(context.Context, Run) (RunRequest, error) { return req, nil }
}

func TestRecover_ResumesAnInterruptedRunWithoutDuplicatingAndTheOldOwnerBackedOff(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	gate := newGate(r.target, 3)
	a := leased(t, r, "A", gate, -1)
	t.Cleanup(gate.Release) // corre antes que a.Close
	req := RunRequest{Org: "org-a", Actor: "u1", Profile: catalogProfile(), Source: NewRowsSource("filas", syntheticRows(10))}

	runA, err := a.Start(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	<-gate.reached // los lotes 1 y 2 ya se escribieron; el proceso A "se cuelga" en el 3
	if r.target.Len("org-a", "item") != 4 {
		t.Fatalf("antes del corte debía haber 4 filas: %d", r.target.Len("org-a", "item"))
	}
	mid, _ := r.ledger.Get(ctx, "org-a", runA.ID)
	if mid.State != RunRunning || mid.Progress.Done != 4 || mid.Checkpoint == nil || mid.Checkpoint.Batches != 2 || mid.Lease == nil || mid.Lease.Owner != "A" {
		t.Fatalf("checkpoint por lote: %+v %+v", mid.Progress, mid.Checkpoint)
	}

	r.clk.Advance(2 * time.Minute) // el arrendamiento de A venció
	b := leased(t, r, "B", r.target, -1)
	rep, err := b.Recover(ctx, "org-a", sameRequest(req))
	if err != nil || len(rep.Resumed) != 1 || len(rep.Failed) != 0 || rep.Resumed[0].ID != runA.ID {
		t.Fatalf("%v %+v", err, rep)
	}
	done, err := b.Wait(ctx, "org-a", runA.ID)
	if err != nil {
		t.Fatal(err)
	}
	if done.State != RunSucceeded || done.Progress.Total != 10 || done.Progress.Done != 10 || done.Progress.Created != 10 || done.Progress.Unchanged != 0 {
		t.Fatalf("la reanudación suma lo hecho antes: %+v", done.Progress)
	}
	if done.Checkpoint.Resumes != 1 || done.Checkpoint.Rows != 10 || done.Checkpoint.Batches != 5 || done.Lease != nil {
		t.Fatalf("checkpoint = %+v lease=%v", done.Checkpoint, done.Lease)
	}
	if r.target.Len("org-a", "item") != 10 {
		t.Fatalf("filas = %d", r.target.Len("org-a", "item"))
	}

	// A despierta: termina su lote (idempotente) pero ya no es dueña; no pisa el historial.
	gate.Release()
	a.Close()
	final, _ := r.ledger.Get(ctx, "org-a", runA.ID)
	if final.State != RunSucceeded || final.Progress.Done != 10 || final.Error != "" || final.Lease != nil {
		t.Fatalf("el dueño anterior pisó el resultado: %+v", final)
	}
	if r.target.Len("org-a", "item") != 10 {
		t.Fatal("sin duplicados")
	}
	// Y repetir la entrada ya es un replay del resultado de B.
	again, err := b.Start(ctx, req)
	if err != nil || again.ReplayOf != runA.ID {
		t.Fatalf("%v %+v", err, again)
	}
}

func TestRecover_LeavesAliveRunsAloneAndTakesOnlyExpiredOnes(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	gate := newGate(r.target, 2)
	a := leased(t, r, "A", gate, -1)
	t.Cleanup(gate.Release)
	req := RunRequest{Org: "org-a", Profile: catalogProfile(), Source: NewRowsSource("filas", syntheticRows(6))}
	run, _ := a.Start(ctx, req)
	<-gate.reached

	r.clk.Advance(30 * time.Second) // dentro del arrendamiento
	b := leased(t, r, "B", r.target, -1)
	rep, err := b.Recover(ctx, "org-a", sameRequest(req))
	if err != nil || len(rep.Resumed) != 0 || len(rep.Failed) != 0 {
		t.Fatalf("no se toca una corrida con arrendamiento vigente: %v %+v", err, rep)
	}
	// Dos procesos que recuperan a la vez no se llevan la misma corrida.
	r.clk.Advance(time.Minute)
	c := leased(t, r, "C", r.target, -1)
	var wg sync.WaitGroup
	var taken int32
	for _, rn := range []*Runner{b, c} {
		wg.Add(1)
		go func(rn *Runner) {
			defer wg.Done()
			rep, _ := rn.Recover(ctx, "org-a", sameRequest(req))
			atomic.AddInt32(&taken, int32(len(rep.Resumed)))
		}(rn)
	}
	wg.Wait()
	if taken != 1 {
		t.Fatalf("la corrida debe reanudarla un solo proceso, la tomaron %d", taken)
	}
	gate.Release()
	for _, rn := range []*Runner{b, c} {
		_, _ = rn.Wait(ctx, "org-a", run.ID)
	}
	if r.target.Len("org-a", "item") != 6 {
		t.Fatal("filas")
	}
}

func TestRecover_RefusesToResumeWhenInputOrProfileChanged(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	gate := newGate(r.target, 2)
	a := leased(t, r, "A", gate, -1)
	t.Cleanup(gate.Release)
	req := RunRequest{Org: "org-a", Profile: catalogProfile(), Source: NewRowsSource("filas", syntheticRows(6))}
	run, _ := a.Start(ctx, req)
	<-gate.reached
	r.clk.Advance(2 * time.Minute)

	b := leased(t, r, "B", r.target, -1)
	changed := req
	changed.Source = NewRowsSource("filas", syntheticRows(7))
	rep, err := b.Recover(ctx, "org-a", sameRequest(changed))
	if err != nil || len(rep.Failed) != 1 || len(rep.Resumed) != 0 || !strings.Contains(rep.Failed[0].Error, "entrada") {
		t.Fatalf("%v %+v", err, rep)
	}
	got, _ := r.ledger.Get(ctx, "org-a", run.ID)
	if got.State != RunFailed || !strings.Contains(got.Error, "no se pudo reanudar") {
		t.Fatalf("no debe quedar huérfana en running: %+v", got)
	}
	if r.target.Len("org-a", "item") != 2 {
		t.Fatal("no se escribe nada con una entrada distinta")
	}
}

func TestRecover_FailedResolverAndChangedProfileMarkTheRunFailed(t *testing.T) {
	for name, resolve := range map[string]func(RunRequest) RequestResolver{
		"resolver con error": func(RunRequest) RequestResolver {
			return func(context.Context, Run) (RunRequest, error) { return RunRequest{}, errors.New("fuente borrada") }
		},
		"otro perfil": func(req RunRequest) RequestResolver {
			req.Profile.Defaults = map[string]any{"extra": "x"}
			return sameRequest(req)
		},
		"otra organización": func(req RunRequest) RequestResolver {
			req.Org = "org-b"
			return sameRequest(req)
		},
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t)
			ctx := context.Background()
			gate := newGate(r.target, 2)
			a := leased(t, r, "A", gate, -1)
			t.Cleanup(gate.Release)
			req := RunRequest{Org: "org-a", Profile: catalogProfile(), Source: NewRowsSource("filas", syntheticRows(6))}
			run, _ := a.Start(ctx, req)
			<-gate.reached
			r.clk.Advance(2 * time.Minute)
			b := leased(t, r, "B", r.target, -1)
			rep, err := b.Recover(ctx, "org-a", resolve(req))
			got, _ := r.ledger.Get(ctx, "org-a", run.ID)
			if err != nil || len(rep.Failed) != 1 || got.State != RunFailed {
				t.Fatalf("%v %+v %s", err, rep, got.State)
			}
		})
	}
}

type plainLedger struct{ Ledger }

func TestRecover_RequiresALeaseLedger(t *testing.T) {
	rn := &Runner{Ledger: plainLedger{NewMemoryLedger()}, Target: NewMemoryTarget()}
	if _, err := rn.Recover(context.Background(), "o", sameRequest(RunRequest{})); !errors.Is(err, ErrNoLeaseLedger) {
		t.Fatalf("err = %v", err)
	}
	// Sin arrendamientos todo sigue funcionando como antes.
	run, err := rn.Start(context.Background(), RunRequest{Org: "o", Profile: catalogProfile(), Source: NewRowsSource("f", syntheticRows(3))})
	if err != nil {
		t.Fatal(err)
	}
	done, _ := rn.Wait(context.Background(), "o", run.ID)
	rn.Close()
	if done.State != RunSucceeded || done.Lease != nil {
		t.Fatalf("%+v", done)
	}
}

func TestRunner_HeartbeatKeepsTheLeaseAliveWhileABatchIsSlow(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	gate := newGate(r.target, 2)
	a := leased(t, r, "A", gate, 5*time.Millisecond)
	t.Cleanup(gate.Release)
	req := RunRequest{Org: "org-a", Profile: catalogProfile(), Source: NewRowsSource("filas", syntheticRows(6))}
	run, _ := a.Start(ctx, req)
	<-gate.reached

	r.clk.Advance(40 * time.Second)
	waitFor(t, func() bool {
		got, _ := r.ledger.Get(ctx, "org-a", run.ID)
		return got.Lease != nil && !got.Lease.Heartbeat.Before(r.clk.Now())
	})
	r.clk.Advance(40 * time.Second) // 80 s desde que arrancó: sin latidos ya estaría vencida
	b := leased(t, r, "B", r.target, -1)
	rep, _ := b.Recover(ctx, "org-a", sameRequest(req))
	if len(rep.Resumed) != 0 {
		t.Fatal("una corrida que late no se recupera")
	}
	gate.Release()
	done, _ := a.Wait(ctx, "org-a", run.ID)
	if done.State != RunSucceeded {
		t.Fatalf("%+v", done)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("la condición no se cumplió a tiempo")
}

func TestStart_DoesNotDuplicateARunThatIsActiveInAnotherProcess(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	gate := newGate(r.target, 2)
	a := leased(t, r, "A", gate, -1)
	t.Cleanup(gate.Release)
	req := RunRequest{Org: "org-a", Profile: catalogProfile(), Source: NewRowsSource("filas", syntheticRows(6))}
	runA, _ := a.Start(ctx, req)
	<-gate.reached

	b := leased(t, r, "B", r.target, -1)
	got, err := b.Start(ctx, req)
	if err != nil || got.ID != runA.ID || got.State != RunRunning {
		t.Fatalf("la misma entrada en curso en otro proceso devuelve esa corrida: %v %+v", err, got)
	}
	if r.target.Len("org-a", "item") != 2 {
		t.Fatal("B no debe escribir nada")
	}
	list, _ := r.ledger.List(ctx, "org-a", 10)
	if len(list) != 1 {
		t.Fatalf("no se crea una corrida nueva: %d", len(list))
	}
	gate.Release()
	_, _ = a.Wait(ctx, "org-a", runA.ID)
}

func TestRunner_LargeBatchCheckpointsEveryBatchAndQueriesTheTargetInChunks(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	ct := &countingTarget{MemoryTarget: r.target}
	rn := &Runner{Ledger: r.ledger, Target: ct, Now: r.clk.Now, BatchSize: 100}
	t.Cleanup(rn.Close)
	req := RunRequest{Org: "org-a", Profile: catalogProfile(), Source: NewRowsSource("grande", syntheticRows(1200))}
	run, err := rn.Start(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	done, _ := rn.Wait(ctx, "org-a", run.ID)
	if done.State != RunSucceeded || done.Progress.Created != 1200 {
		t.Fatalf("%+v", done.Progress)
	}
	if cp := done.Checkpoint; cp == nil || cp.Batches != 12 || cp.Rows != 1200 || cp.LastKey != "i1200" || cp.Resumes != 0 {
		t.Fatalf("checkpoint = %+v", cp)
	}
	if len(ct.sizes) != 3 || ct.sizes[0] != DefaultLookupChunk || ct.sizes[2] != 200 {
		t.Fatalf("la consulta al destino va por tandas: %v", ct.sizes)
	}
}
