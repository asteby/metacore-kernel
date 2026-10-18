package dataload

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"testing"
	"time"
)

// hookReviewStore es un MemoryReviewStore con un gancho antes de cada PutIf:
// si before devuelve (false, nil) o un error, PutIf responde eso sin escribir.
type hookReviewStore struct {
	*MemoryReviewStore
	before func(e ReviewEntry, expect ReviewExpect) (pass bool, err error)
}

func (h *hookReviewStore) PutIf(ctx context.Context, e ReviewEntry, expect ReviewExpect) (bool, error) {
	if h.before != nil {
		if pass, err := h.before(e, expect); !pass || err != nil {
			return false, err
		}
	}
	return h.MemoryReviewStore.PutIf(ctx, e, expect)
}

// casRunner arma un Runner liviano (sin medios) para las pruebas de revisión.
func casRunner(store ReviewStore, target Target, clk *clock) *Runner {
	return &Runner{Ledger: NewMemoryLedger(), Target: target, Reviews: store, Now: clk.Now, BatchSize: 2, Authorizer: allowAll}
}

// seedReviews guarda n filas en el estado dado (claves K0..Kn-1, huella h-Ki).
func seedReviews(t *testing.T, s ReviewStore, state ReviewState, n int, at time.Time) {
	t.Helper()
	for i := 0; i < n; i++ {
		k := fmt.Sprintf("K%d", i)
		if err := s.Put(context.Background(), ReviewEntry{
			Org: "org-a", Entity: "product", Key: k, Row: i + 1, Hash: "h-" + k, Record: map[string]any{"sku": k},
			State: state, DecidedBy: "ana", CreatedAt: at, UpdatedAt: at,
		}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestReviewCAS_MemoryStorePutIfComparesStateAndHash(t *testing.T) {
	ctx := context.Background()
	s := NewMemoryReviewStore()
	e := ReviewEntry{Org: "o", Entity: "p", Key: "k", Hash: "h1", State: ReviewPending}
	if ok, _ := s.PutIf(ctx, e, ReviewExpect{State: ReviewPending, Hash: "h1"}); ok {
		t.Fatal("no existe: una condición sobre estado no puede cumplirse")
	}
	if ok, _ := s.PutIf(ctx, e, ReviewExpect{Missing: true}); !ok {
		t.Fatal("inserción con Missing")
	}
	if ok, _ := s.PutIf(ctx, e, ReviewExpect{Missing: true}); ok {
		t.Fatal("Missing con la entrada ya guardada no debe escribir")
	}
	next := e
	next.State = ReviewApproved
	if ok, _ := s.PutIf(ctx, next, ReviewExpect{State: ReviewPending, Hash: "otra"}); ok {
		t.Fatal("huella distinta no debe escribir")
	}
	if ok, _ := s.PutIf(ctx, next, ReviewExpect{State: ReviewRejected, Hash: "h1"}); ok {
		t.Fatal("estado distinto no debe escribir")
	}
	if ok, _ := s.PutIf(ctx, next, ReviewExpect{State: ReviewPending, Hash: "h1"}); !ok {
		t.Fatal("la condición se cumple: debe escribir")
	}
	if got, _, _ := s.Get(ctx, "o", "p", "k"); got.State != ReviewApproved {
		t.Fatalf("estado = %s", got.State)
	}
}

// Con escritura condicional la fila se reserva antes de tocar el destino: un
// rechazo que llega en medio de la escritura se niega (ErrReviewBusy) y la fila
// termina applied, nunca escrita y 'rejected'.
func TestReviewCAS_DecideCannotRejectARowReservedForApply(t *testing.T) {
	r := newRig(t)
	r.runner.Reviews = NewMemoryReviewStore()
	ctx := context.Background()
	startAndWait(t, r, listaRequest(t))
	p5, _, _ := r.runner.Reviews.Get(ctx, "org-a", "product", "P5")
	if _, err := r.runner.Decide(ctx, "org-a", "ana", []ReviewDecision{{Entity: "product", Key: "P5", Hash: p5.Hash, Approve: true}}); err != nil {
		t.Fatal(err)
	}
	var rejectErr error
	r.runner.Target = &lookupHook{MemoryTarget: r.target, fn: func() {
		if e, _, _ := r.runner.Reviews.Get(ctx, "org-a", "product", "P5"); e.State != ReviewApplying {
			t.Errorf("antes de escribir la fila debe estar reservada: %s", e.State)
		}
		_, rejectErr = r.runner.Decide(ctx, "org-a", "luis", []ReviewDecision{{Entity: "product", Key: "P5", Hash: p5.Hash, Approve: false}})
	}}
	run, err := r.runner.ApplyApproved(ctx, "org-a", "ana", "product")
	if err != nil {
		t.Fatal(err)
	}
	if !errors.Is(rejectErr, ErrReviewBusy) {
		t.Fatalf("el rechazo de una fila reservada debe fallar con ErrReviewBusy: %v", rejectErr)
	}
	e, _, _ := r.runner.Reviews.Get(ctx, "org-a", "product", "P5")
	if _, ok := r.target.Get("org-a", "product", "P5"); !ok || e.State != ReviewApplied || e.DecidedBy != "ana" || e.AppliedRun != run.ID {
		t.Fatalf("P5 debe quedar escrita y applied: %s por %s", e.State, e.DecidedBy)
	}
	if run.Progress.Created != 1 || run.Progress.Done != 1 {
		t.Fatalf("avance: %+v", run.Progress)
	}
}

// Si la fila se rechaza justo antes de reservarla, el CAS falla y no se escribe.
func TestReviewCAS_ApplySkipsRowRejectedJustBeforeItsReservation(t *testing.T) {
	clk := newClock()
	store := &hookReviewStore{MemoryReviewStore: NewMemoryReviewStore()}
	seedReviews(t, store, ReviewApproved, 3, clk.Now())
	target := NewMemoryTarget()
	r := casRunner(store, target, clk)
	ctx := context.Background()
	var once sync.Once
	store.before = func(e ReviewEntry, _ ReviewExpect) (bool, error) {
		if e.Key == "K1" && e.State == ReviewApplying {
			once.Do(func() {
				if _, err := r.Decide(ctx, "org-a", "luis", []ReviewDecision{{Entity: "product", Key: "K1", Hash: "h-K1", Approve: false}}); err != nil {
					t.Error(err)
				}
			})
		}
		return true, nil
	}
	run, err := r.ApplyApproved(ctx, "org-a", "ana", "product")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := target.Get("org-a", "product", "K1"); ok {
		t.Fatal("K1 se rechazó antes de reservarla: no debe escribirse")
	}
	if e, _, _ := store.Get(ctx, "org-a", "product", "K1"); e.State != ReviewRejected || e.DecidedBy != "luis" {
		t.Fatalf("el rechazo se pisó: %s", e.State)
	}
	if run.Progress.Total != 2 || run.Progress.Done != 2 || target.Len("org-a", "product") != 2 {
		t.Fatalf("avance: %+v, destino %d", run.Progress, target.Len("org-a", "product"))
	}
}

// Una decisión múltiple que pierde la carrera en una fila deshace las que ya
// había escrito: sigue siendo todo o nada.
func TestReviewCAS_DecideConflictUndoesEarlierDecisions(t *testing.T) {
	clk := newClock()
	store := &hookReviewStore{MemoryReviewStore: NewMemoryReviewStore()}
	seedReviews(t, store, ReviewPending, 2, clk.Now())
	r := casRunner(store, NewMemoryTarget(), clk)
	ctx := context.Background()
	store.before = func(e ReviewEntry, _ ReviewExpect) (bool, error) {
		return !(e.Key == "K1" && e.State == ReviewApproved), nil // otra escritura le ganó a K1
	}
	_, err := r.Decide(ctx, "org-a", "ana", []ReviewDecision{
		{Entity: "product", Key: "K0", Hash: "h-K0", Approve: true},
		{Entity: "product", Key: "K1", Hash: "h-K1", Approve: true},
	})
	if !errors.Is(err, ErrReviewConflict) || ErrorCode(err) != CodeReviewConflict {
		t.Fatalf("se esperaba ErrReviewConflict: %v", err)
	}
	if e, _, _ := store.Get(ctx, "org-a", "product", "K0"); e.State != ReviewPending {
		t.Fatalf("K0 debe volver a pending: %s", e.State)
	}
}

// Si el destino falla al escribir, las reservas se resuelven leyendo el destino:
// lo que no se escribió vuelve a approved y nada queda en 'applying'.
func TestReviewCAS_TargetFailureReleasesReservations(t *testing.T) {
	clk := newClock()
	store := NewMemoryReviewStore()
	seedReviews(t, store, ReviewApproved, 2, clk.Now())
	target := NewMemoryTarget()
	target.FailApply = func(int) error { return errors.New("destino caído") }
	r := casRunner(store, target, clk)
	ctx := context.Background()
	if _, err := r.ApplyApproved(ctx, "org-a", "ana", "product"); ErrorCode(err) != CodeApplyApproved {
		t.Fatalf("se esperaba el fallo del destino: %v", err)
	}
	for _, k := range []string{"K0", "K1"} {
		if e, _, _ := store.Get(ctx, "org-a", "product", k); e.State != ReviewApproved || e.AppliedRun != "" {
			t.Fatalf("%s = %s (%s)", k, e.State, e.AppliedRun)
		}
	}
	target.FailApply = nil
	if run, err := r.ApplyApproved(ctx, "org-a", "ana", "product"); err != nil || run.Progress.Done != 2 {
		t.Fatalf("%v %+v", err, run.Progress)
	}
}

// El proceso muere con filas en 'applying': RecoverApplying deja en applied las
// que el destino ya tiene con su huella y en approved las demás, sin escribir
// al destino; el ApplyApproved siguiente escribe solo las devueltas, una vez.
func TestReviewCAS_RecoverApplyingSettlesOrphanReservationsWithoutRewriting(t *testing.T) {
	clk := newClock()
	store := NewMemoryReviewStore()
	ctx := context.Background()
	seedReviews(t, store, ReviewApproved, 3, clk.Now())
	target := NewMemoryTarget()
	// Crash simulado: K0 se reservó y se escribió; K1 se reservó y no se escribió;
	// K2 se reservó hace un momento (otro proceso vivo) y no debe tocarse.
	for i, k := range []string{"K0", "K1", "K2"} {
		e, _, _ := store.Get(ctx, "org-a", "product", k)
		e.State, e.AppliedRun = ReviewApplying, "run-muerto"
		if i == 2 {
			e.UpdatedAt = clk.Now().Add(10 * time.Minute)
		}
		if ok, _ := store.PutIf(ctx, e, ReviewExpect{State: ReviewApproved, Hash: e.Hash}); !ok {
			t.Fatal("reserva")
		}
	}
	if err := target.Apply(ctx, "org-a", "product", []Change{{Key: "K0", Action: ActionCreate, Record: map[string]any{"sku": "K0"}, Hash: "h-K0"}}); err != nil {
		t.Fatal(err)
	}
	clk.Advance(10 * time.Minute)
	r := casRunner(store, target, clk)
	if _, err := r.RecoverApplying(ctx, "org-a", "", "product", 5*time.Minute); err == nil {
		t.Fatal("sin responsable no vale")
	}
	applies := target.applies
	rep, err := r.RecoverApplying(ctx, "org-a", "ana", "product", 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(rep.Applied) != "[K0]" || fmt.Sprint(rep.Released) != "[K1]" {
		t.Fatalf("reporte: %+v", rep)
	}
	if target.applies != applies {
		t.Fatal("RecoverApplying no debe escribir en el destino")
	}
	want := map[string]ReviewState{"K0": ReviewApplied, "K1": ReviewApproved, "K2": ReviewApplying}
	for k, st := range want {
		if e, _, _ := store.Get(ctx, "org-a", "product", k); e.State != st {
			t.Errorf("%s = %s, quería %s", k, e.State, st)
		}
	}
	if e, _, _ := store.Get(ctx, "org-a", "product", "K0"); e.AppliedRun != "run-muerto" {
		t.Errorf("K0 conserva la corrida que la escribió: %q", e.AppliedRun)
	}
	run, err := r.ApplyApproved(ctx, "org-a", "ana", "product")
	if err != nil || run.Progress.Created != 1 || run.Progress.Done != 1 || target.Len("org-a", "product") != 2 {
		t.Fatalf("%v %+v destino=%d", err, run.Progress, target.Len("org-a", "product"))
	}
}

func TestReviewCAS_RecoverApplyingNeedsAConditionalStore(t *testing.T) {
	r := casRunner(plainReviewStore{NewMemoryReviewStore()}, NewMemoryTarget(), newClock())
	if _, err := r.RecoverApplying(context.Background(), "org-a", "ana", "product", time.Minute); ErrorCode(err) != CodeReviewNoConditional {
		t.Fatalf("%v", err)
	}
	r.Reviews = NewMemoryReviewStore()
	if _, err := r.RecoverApplying(context.Background(), "org-a", "ana", "", time.Minute); ErrorCode(err) != CodeEntityRequired {
		t.Fatalf("%v", err)
	}
}

// yieldTarget cede el procesador en cada operación para abrir las ventanas de carrera.
type yieldTarget struct{ *MemoryTarget }

func (y yieldTarget) Lookup(ctx context.Context, org, entity string, keys []string) (map[string]Existing, error) {
	runtime.Gosched()
	return y.MemoryTarget.Lookup(ctx, org, entity, keys)
}

func (y yieldTarget) Apply(ctx context.Context, org, entity string, changes []Change) error {
	runtime.Gosched()
	err := y.MemoryTarget.Apply(ctx, org, entity, changes)
	runtime.Gosched()
	return err
}

// Decide(reject) y ApplyApproved a la vez, muchas veces (pensada para -race):
// NUNCA queda una fila escrita en el destino con estado 'rejected', ni una
// reserva 'applying' abierta al terminar.
func TestReviewCAS_ConcurrentRejectAndApplyNeverLeaveARejectedRowWritten(t *testing.T) {
	const rounds, rows = 300, 6
	ctx := context.Background()
	var written, rejected int
	for round := 0; round < rounds; round++ {
		clk := newClock()
		store := NewMemoryReviewStore()
		seedReviews(t, store, ReviewApproved, rows, clk.Now())
		target := yieldTarget{NewMemoryTarget()}
		r := casRunner(store, target, clk)
		var wg sync.WaitGroup
		wg.Add(3)
		go func() {
			defer wg.Done()
			if _, err := r.ApplyApproved(ctx, "org-a", "ana", "product"); err != nil {
				t.Error(err)
			}
		}()
		for g := 0; g < 2; g++ {
			go func(g int) {
				defer wg.Done()
				for i := g; i < rows; i += 2 {
					k := fmt.Sprintf("K%d", i)
					_, err := r.Decide(ctx, "org-a", "luis", []ReviewDecision{{Entity: "product", Key: k, Hash: "h-" + k, Approve: false}})
					if err != nil && !errors.Is(err, ErrReviewBusy) && !errors.Is(err, ErrReviewClosed) && !errors.Is(err, ErrReviewConflict) {
						t.Error(err)
					}
					runtime.Gosched()
				}
			}(g)
		}
		wg.Wait()
		for i := 0; i < rows; i++ {
			k := fmt.Sprintf("K%d", i)
			e, _, _ := store.Get(ctx, "org-a", "product", k)
			_, isWritten := target.Get("org-a", "product", k)
			switch {
			case isWritten && e.State == ReviewRejected:
				t.Fatalf("ronda %d: %s quedó escrita con estado rejected", round, k)
			case e.State == ReviewApplying:
				t.Fatalf("ronda %d: %s quedó reservada", round, k)
			case isWritten != (e.State == ReviewApplied):
				t.Fatalf("ronda %d: %s escrita=%v estado=%s", round, k, isWritten, e.State)
			}
			if isWritten {
				written++
			} else {
				rejected++
			}
		}
	}
	// Las dos ramas de la carrera deben haberse dado (si no, la prueba no prueba nada).
	if written == 0 || rejected == 0 {
		t.Fatalf("la carrera no se dio: %d escritas, %d rechazadas", written, rejected)
	}
	t.Logf("%d rondas: %d filas escritas y applied, %d rechazadas sin escribir", rounds, written, rejected)
}

// afterApply corre fn después de cada escritura al destino.
type afterApply struct {
	*MemoryTarget
	fn func()
}

func (a afterApply) Apply(ctx context.Context, org, entity string, changes []Change) error {
	err := a.MemoryTarget.Apply(ctx, org, entity, changes)
	a.fn()
	return err
}

// Limitación documentada: sin ConditionalReviewStore, un rechazo que llega entre
// la escritura al destino y la marca 'applied' deja el dato escrito y la entrada
// 'rejected'. Con la extensión el mismo rechazo se niega (ErrReviewBusy).
func TestReview_WithoutConditionalStoreALateRejectStillLeavesTheRowWritten(t *testing.T) {
	ctx := context.Background()
	for _, conditional := range []bool{false, true} {
		clk := newClock()
		mem := NewMemoryReviewStore()
		var store ReviewStore = mem
		if !conditional {
			store = plainReviewStore{mem}
		}
		seedReviews(t, store, ReviewApproved, 1, clk.Now())
		var r *Runner
		var rejectErr error
		target := afterApply{MemoryTarget: NewMemoryTarget()}
		target.fn = func() {
			_, rejectErr = r.Decide(ctx, "org-a", "luis", []ReviewDecision{{Entity: "product", Key: "K0", Hash: "h-K0", Approve: false}})
		}
		r = casRunner(store, target, clk)
		if _, err := r.ApplyApproved(ctx, "org-a", "ana", "product"); err != nil {
			t.Fatal(err)
		}
		e, _, _ := mem.Get(ctx, "org-a", "product", "K0")
		_, written := target.Get("org-a", "product", "K0")
		if conditional {
			if !errors.Is(rejectErr, ErrReviewBusy) || !written || e.State != ReviewApplied {
				t.Fatalf("con PutIf: %v escrita=%v estado=%s", rejectErr, written, e.State)
			}
			continue
		}
		if rejectErr != nil || !written || e.State != ReviewRejected {
			t.Fatalf("sin PutIf se esperaba la carrera conocida: %v escrita=%v estado=%s", rejectErr, written, e.State)
		}
	}
}
