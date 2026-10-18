package dataload

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

// Regresiones de la revisión de feat/dataload-escala (cada prueba falla sin su
// arreglo). Los escenarios son los que reprodujeron los revisores.

func attachedURLs(r *rig, key string) []string {
	var out []string
	for _, ref := range r.sink.Get("org-a", "item", key, "images") {
		out = append(out, ref.SourceURL)
	}
	return out
}

func oneRowCSV(urls string) []byte {
	return []byte(fmt.Sprintf("ID,Nombre,Precio,Descripción,Publicado,Imágenes,Meta: _wcct_x\n1,WHEY PROTEIN,549.50,x,1,\"%s\",x\n", urls))
}

// Una imagen que se quita en una carga posterior deja de ligarse, y un cambio de
// orden se respeta aunque los trabajos de esas URLs ya existan.
func TestRegression_MediaFollowsTheCurrentSetNotTheQueueHistory(t *testing.T) {
	r := newRig(t)
	a, b, d := r.origin.URL("/img/a.jpg"), r.origin.URL("/img/b.jpg"), r.origin.URL("/img/d.jpg")
	startAndWait(t, r, req(r, csvRows(r.origin)))
	r.drain(t, "org-a")
	if got := attachedURLs(r, "1"); fmt.Sprint(got) != fmt.Sprint([]string{a, b}) {
		t.Fatalf("inicial = %v", got)
	}
	startAndWait(t, r, req(r, oneRowCSV(d)))
	r.drain(t, "org-a")
	if got := attachedURLs(r, "1"); fmt.Sprint(got) != fmt.Sprint([]string{d}) {
		t.Fatalf("tras quitar a y b: %v (se esperaba solo d)", got)
	}
	// Mismo conjunto en otro orden: los trabajos ya existen (mismo jobID).
	startAndWait(t, r, req(r, oneRowCSV(b+", "+a)))
	r.drain(t, "org-a")
	if got := attachedURLs(r, "1"); fmt.Sprint(got) != fmt.Sprint([]string{b, a}) {
		t.Fatalf("reordenado = %v", got)
	}
	refs := r.sink.Get("org-a", "item", "1", "images")
	if refs[0].Index != 0 || refs[1].Index != 1 {
		t.Fatalf("índices = %+v", refs)
	}
	// Quitar todas: la galería queda vacía.
	startAndWait(t, r, req(r, oneRowCSV("")))
	r.drain(t, "org-a")
	if got := attachedURLs(r, "1"); len(got) != 0 {
		t.Fatalf("sin imágenes = %v", got)
	}
}

// Force tras borrar en el destino: los registros se recrean y sus medios (ya
// respaldados, sin volver a bajarlos) se vuelven a ligar.
func TestRegression_ForceAfterDeletingTheTargetReattachesMedia(t *testing.T) {
	r := newRig(t)
	rq := req(r, csvRows(r.origin))
	startAndWait(t, r, rq)
	r.drain(t, "org-a")
	hits := r.origin.Hits("/img/a.jpg")
	r.sink.mu.Lock()
	r.sink.Refs = map[string][]MediaRef{}
	r.sink.mu.Unlock()
	r.target.mu.Lock()
	for k := range r.target.rows {
		delete(r.target.rows, k)
	}
	r.target.mu.Unlock()
	rq.Force = true
	startAndWait(t, r, rq)
	r.drain(t, "org-a")
	if len(attachedURLs(r, "1")) != 2 || len(attachedURLs(r, "2")) != 1 {
		t.Fatalf("recreados sin imágenes: %v / %v", attachedURLs(r, "1"), attachedURLs(r, "2"))
	}
	if r.origin.Hits("/img/a.jpg") != hits {
		t.Fatal("volver a ligar no debe volver a bajar el original")
	}
}

// deferringSink pospone la liga mientras defer devuelva true (p. ej. el dueño
// aún no existe en el destino) y falla para la identidad "1" mientras fail > 0.
type deferringSink struct {
	mu sync.Mutex
	*MemorySink
	defer_ func(key string) bool
	fail   int
}

func (s *deferringSink) Attach(ctx context.Context, org, entity, key, field string, refs []MediaRef) error {
	s.mu.Lock()
	failNow := s.fail > 0 && key == "1"
	if failNow {
		s.fail--
	}
	s.mu.Unlock()
	if failNow {
		return errors.New("base caída")
	}
	if s.defer_ != nil && s.defer_(key) {
		return ErrAttachDeferred
	}
	return s.MemorySink.Attach(ctx, org, entity, key, field, refs)
}

// Si el destino todavía no puede recibir los medios (huérfana) o Attach falla,
// el campo queda pendiente de ligar: la cola reintenta la liga sin bajar de
// nuevo y la siguiente corrida que vea la identidad, aunque venga unchanged, la
// liga.
func TestRegression_DeferredOrFailedAttachIsRetried(t *testing.T) {
	r := newRig(t)
	orphan := true
	ds := &deferringSink{MemorySink: r.sink, defer_: func(key string) bool { return key == "2" && orphan }, fail: 1}
	r.queue.Sink = ds
	rq := req(r, csvRows(r.origin))
	startAndWait(t, r, rq)
	for i := 0; i < 10; i++ { // el fallo de Attach sale de ProcessOnce; se sigue procesando
		_, _ = r.queue.ProcessOnce(context.Background(), 100)
		r.clk.Advance(15 * time.Minute)
	}
	if len(attachedURLs(r, "1")) != 2 {
		t.Fatalf("el fallo transitorio de Attach no se reintentó: %v", attachedURLs(r, "1"))
	}
	if len(attachedURLs(r, "2")) != 0 {
		t.Fatal("la huérfana no debía ligarse todavía")
	}
	orphan = false // llegó el padre: el destino ya puede recibirla
	rq.Force = true
	run := startAndWait(t, r, rq)
	if run.Progress.Created+run.Progress.Updated != 0 {
		t.Fatalf("la segunda corrida debía ser unchanged: %+v", run.Progress)
	}
	if len(attachedURLs(r, "2")) != 1 {
		t.Fatalf("la huérfana resuelta no se ligó: %v", attachedURLs(r, "2"))
	}
}

// Una fila aprobada y sin aplicar que una corrida posterior ya cargó corregida
// pasa a superseded; ApplyApproved no regresa el dato a la versión vieja.
func TestRegression_ApprovedButUnappliedRowIsSupersededByANewerCleanLoad(t *testing.T) {
	r := newRig(t)
	r.runner.Reviews = NewMemoryReviewStore()
	ctx := context.Background()
	startAndWait(t, r, listaRequest(t))
	p6, ok, _ := r.runner.Reviews.Get(ctx, "org-a", "product", "P6")
	if !ok {
		t.Fatal("P6 debía estar en revisión")
	}
	if _, err := r.runner.Decide(ctx, "org-a", "ana", []ReviewDecision{{Entity: "product", Key: "P6", Hash: p6.Hash, Approve: true}}); err != nil {
		t.Fatal(err)
	}
	rows := listaRows()
	for i := range rows {
		if rows[i].sku == "P6" {
			rows[i].desc, rows[i].pub = "BUJE TRASERO TOYOTA YARIS 1.5 L4 2007 2012", "999"
		}
	}
	startAndWait(t, r, listaRequest(t, rows...))
	fresh, _ := r.target.Get("org-a", "product", "P6")
	if e, _, _ := r.runner.Reviews.Get(ctx, "org-a", "product", "P6"); e.State != ReviewSuperseded {
		t.Fatalf("estado = %s, se esperaba superseded", e.State)
	}
	if _, err := r.runner.ApplyApproved(ctx, "org-a", "ana", "product"); err != nil {
		t.Fatal(err)
	}
	if got, _ := r.target.Get("org-a", "product", "P6"); got.Hash != fresh.Hash {
		t.Fatalf("ApplyApproved pisó la versión nueva: %v -> %v", fresh.Record["price"], got.Record["price"])
	}
}

// Si el destino cambió después de decidir (la entrada no se superó porque la
// corrida que lo cambió no la vio), ApplyApproved no escribe: la entrada pasa a
// superseded y se cuenta.
func TestRegression_ApplyApprovedSkipsRowsWhoseDestinationMovedAfterTheDecision(t *testing.T) {
	ctx := context.Background()
	for name, store := range map[string]ReviewStore{"cas": NewMemoryReviewStore(), "sin cas": plainReviews{NewMemoryReviewStore()}} {
		t.Run(name, func(t *testing.T) {
			tgt := NewMemoryTarget()
			_ = tgt.Apply(ctx, "org-a", "product", []Change{{Key: "K", Action: ActionCreate, Record: map[string]any{"price": 1}, Hash: "base"}})
			_ = store.Put(ctx, ReviewEntry{Org: "org-a", Entity: "product", Key: "K", Hash: "aprobada", BaseHash: "base", Outcome: ActionUpdate,
				Record: map[string]any{"price": 300}, State: ReviewApproved})
			// Otro escribe una versión más nueva.
			_ = tgt.Apply(ctx, "org-a", "product", []Change{{Key: "K", Action: ActionUpdate, Record: map[string]any{"price": 999}, Hash: "nueva"}})
			rn := casRunner(store, tgt, newClock())
			run, err := rn.ApplyApproved(ctx, "org-a", "ana", "product")
			if err != nil {
				t.Fatal(err)
			}
			got, _ := tgt.Get("org-a", "product", "K")
			e, _, _ := store.Get(ctx, "org-a", "product", "K")
			if got.Hash != "nueva" || e.State != ReviewSuperseded || run.Progress.Superseded != 1 || run.Progress.Done != 0 {
				t.Fatalf("destino=%s estado=%s progress=%+v", got.Hash, e.State, run.Progress)
			}
		})
	}
}

// plainReviews oculta PutIf: un ReviewStore sin escritura condicional.
type plainReviews struct{ *MemoryReviewStore }

func (p plainReviews) Put(ctx context.Context, e ReviewEntry) error {
	return p.MemoryReviewStore.Put(ctx, e)
}
func (p plainReviews) Get(ctx context.Context, org, entity, key string) (ReviewEntry, bool, error) {
	return p.MemoryReviewStore.Get(ctx, org, entity, key)
}
func (p plainReviews) List(ctx context.Context, org string, f ReviewFilter) ([]ReviewEntry, error) {
	return p.MemoryReviewStore.List(ctx, org, f)
}

type flakyEnqueue struct {
	MediaJobStore
	fail int
}

func (f *flakyEnqueue) Enqueue(ctx context.Context, jobs []MediaJob) (int, error) {
	if f.fail > 0 {
		f.fail--
		return 0, errors.New("base caída")
	}
	return f.MediaJobStore.Enqueue(ctx, jobs)
}

// Si encolar los medios falla, la fila aprobada NO queda applied sin medios: el
// siguiente ApplyApproved la ve ya escrita (unchanged) y solo encola sus medios.
func TestRegression_ApplyApprovedEnqueuesMediaBeforeMarkingApplied(t *testing.T) {
	ctx := context.Background()
	for _, cas := range []bool{true, false} {
		t.Run(fmt.Sprint("cas=", cas), func(t *testing.T) {
			r := newRig(t)
			var store ReviewStore = NewMemoryReviewStore()
			if !cas {
				store = plainReviews{NewMemoryReviewStore()}
			}
			r.runner.Reviews = store
			_ = store.Put(ctx, ReviewEntry{Org: "org-a", Entity: "item", Key: "9", Hash: "h9", Outcome: ActionCreate, Record: map[string]any{"external_id": "9"},
				Media: map[string][]string{"images": {r.origin.URL("/img/z.jpg")}}, State: ReviewApproved})
			r.queue.Jobs = &flakyEnqueue{MediaJobStore: r.jobs, fail: 1}
			if _, err := r.runner.ApplyApproved(ctx, "org-a", "ana", "item"); err == nil {
				t.Fatal("esperaba el fallo al encolar")
			}
			if e, _, _ := store.Get(ctx, "org-a", "item", "9"); e.State != ReviewApproved {
				t.Fatalf("tras el fallo: %s (debe seguir aprobada)", e.State)
			}
			run, err := r.runner.ApplyApproved(ctx, "org-a", "ana", "item")
			if err != nil {
				t.Fatal(err)
			}
			r.drain(t, "org-a")
			e, _, _ := store.Get(ctx, "org-a", "item", "9")
			if e.State != ReviewApplied || len(r.sink.Get("org-a", "item", "9", "images")) != 1 || run.Progress.Created != 0 {
				t.Fatalf("estado=%s ligadas=%d progress=%+v", e.State, len(r.sink.Get("org-a", "item", "9", "images")), run.Progress)
			}
		})
	}
}

// ApplyApproved queda en el historial desde que empieza (running, con
// arrendamiento) y no al final: se ve en curso y Recover la cierra si el
// proceso muere.
func TestRegression_ApplyApprovedIsInTheLedgerWhileRunning(t *testing.T) {
	ctx := context.Background()
	clk := newClock()
	store := &hookReviewStore{MemoryReviewStore: NewMemoryReviewStore()}
	seedReviews(t, store, ReviewApproved, 1, clk.Now())
	ledger := NewMemoryLedger()
	rn := &Runner{Ledger: ledger, Target: NewMemoryTarget(), Reviews: store, Now: clk.Now, BatchSize: 2, Authorizer: allowAll, Owner: "A"}
	var during []Run
	store.before = func(e ReviewEntry, _ ReviewExpect) (bool, error) {
		if e.State == ReviewApplying {
			during, _ = ledger.List(ctx, "org-a", 10)
		}
		return true, nil
	}
	if _, err := rn.ApplyApproved(ctx, "org-a", "ana", "product"); err != nil {
		t.Fatal(err)
	}
	if len(during) != 1 || during[0].Mode != ModeReview || during[0].State != RunRunning || during[0].Lease == nil || during[0].Lease.Owner != "A" {
		t.Fatalf("en curso = %+v", during)
	}
	// Un proceso que muere a la mitad: Recover la cierra como fallida (sin
	// pedirle la solicitud al resolver) y sus reservas quedan para RecoverApplying.
	run := during[0]
	run.ID, run.State, run.Lease = "huerfana", RunRunning, &RunLease{Owner: "A", Until: clk.Now()}
	_ = ledger.Save(ctx, run)
	clk.Advance(time.Hour)
	b := &Runner{Ledger: ledger, Target: NewMemoryTarget(), Reviews: store, Now: clk.Now, Authorizer: allowAll, Owner: "B"}
	called := false
	rep, err := b.Recover(ctx, "org-a", func(context.Context, Run) (RunRequest, error) { called = true; return RunRequest{}, nil })
	got, _ := ledger.Get(ctx, "org-a", "huerfana")
	if err != nil || called || len(rep.Failed) != 1 || got.State != RunFailed || got.ErrorCode != CodeRecoverAbandoned {
		t.Fatalf("err=%v called=%v rep=%+v state=%s code=%s", err, called, rep, got.State, got.ErrorCode)
	}
}

// Un error transitorio al reconstruir la solicitud (base caída) no deja la
// corrida fallida para siempre: se suelta y la siguiente pasada la reanuda.
func TestRegression_RecoverDefersTransientResolverErrors(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	gate := newGate(r.target, 2)
	a := leased(t, r, "A", gate, -1)
	t.Cleanup(gate.Release)
	rq := RunRequest{Org: "org-a", Profile: catalogProfile(), Source: NewRowsSource("filas", syntheticRows(6))}
	run, _ := a.Start(ctx, rq)
	<-gate.reached
	r.clk.Advance(2 * time.Minute)
	b := leased(t, r, "B", r.target, -1)
	rep, err := b.Recover(ctx, "org-a", func(context.Context, Run) (RunRequest, error) { return RunRequest{}, errors.New("timeout") })
	got, _ := r.ledger.Get(ctx, "org-a", run.ID)
	if err != nil || len(rep.Deferred) != 1 || len(rep.Failed) != 0 || got.State != RunRunning || got.ErrorCode != CodeRecoverDeferred {
		t.Fatalf("err=%v rep=%+v state=%s code=%s", err, rep, got.State, got.ErrorCode)
	}
	rep, err = b.Recover(ctx, "org-a", sameRequest(rq))
	if err != nil || len(rep.Resumed) != 1 {
		t.Fatalf("la siguiente pasada debía reanudarla: %v %+v", err, rep)
	}
	done, _ := b.Wait(ctx, "org-a", run.ID)
	if done.State != RunSucceeded || r.target.Len("org-a", "item") != 6 {
		t.Fatalf("state=%s filas=%d", done.State, r.target.Len("org-a", "item"))
	}
}

// Un original dañado en el almacén se repara con los bytes correctos del origen
// (mismo sha256), en vez de quedar corrupto para siempre.
func TestRegression_CorruptOriginalIsRepairedFromTheOrigin(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	b := r.queue.Backup
	u := r.origin.URL("/img/a.jpg")
	e, err := b.Backup(ctx, "org-a", u)
	if err != nil {
		t.Fatal(err)
	}
	r.store.mu.Lock()
	r.store.Objects[e.StoredName] = []byte("bit rot")
	r.store.mu.Unlock()
	if rep, _ := b.VerifyAll(ctx, "org-a"); len(rep.Corrupt) != 1 {
		t.Fatalf("verify = %+v", rep)
	}
	fixed, err := b.Backup(ctx, "org-a", u)
	if err != nil || fixed.Status != EntryOK || fixed.SHA256 != e.SHA256 || fixed.StoredName != e.StoredName {
		t.Fatalf("Backup tras la corrupción: %+v %v", fixed, err)
	}
	if string(r.store.Objects[e.StoredName]) != string(jpegBytes("a")) {
		t.Fatal("el objeto no quedó restaurado")
	}
	if rep, _ := b.VerifyAll(ctx, "org-a"); !rep.Healthy() {
		t.Fatalf("tras reparar: %+v", rep)
	}
}
