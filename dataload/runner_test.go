package dataload

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func startAndWait(t *testing.T, r *rig, req RunRequest) Run {
	t.Helper()
	run, err := r.runner.Start(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	done, err := r.runner.Wait(context.Background(), req.Org, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	return done
}

func req(r *rig, body []byte) RunRequest {
	return RunRequest{Org: "org-a", Actor: "u1", Profile: catalogProfile(), Source: NewCSVSource("catalogo.csv", body)}
}

func TestRunner_DryRunIsRecordedInHistoryWithoutWriting(t *testing.T) {
	r := newRig(t)
	plan, run, err := r.runner.DryRun(context.Background(), req(r, csvRows(r.origin)))
	if err != nil || plan.Counts[ActionCreate] != 3 {
		t.Fatalf("%v %+v", err, plan)
	}
	if run.Mode != ModeDryRun || run.State != RunSucceeded || r.target.Len("org-a", "item") != 0 {
		t.Fatalf("run = %+v", run)
	}
	list, _ := r.ledger.List(context.Background(), "org-a", 10)
	if len(list) != 1 {
		t.Fatalf("historial = %d", len(list))
	}
}

func TestRunner_ApplyCreatesRecordsAndReportsProgress(t *testing.T) {
	r := newRig(t)
	run := startAndWait(t, r, req(r, csvRows(r.origin)))
	if run.State != RunSucceeded || run.Progress.Created != 3 || run.Progress.Done != 3 || run.Progress.MediaQueued != 3 {
		t.Fatalf("run = %+v", run)
	}
	if r.target.Len("org-a", "item") != 3 || r.target.Batches() != 2 { // lotes de 2
		t.Fatalf("filas=%d lotes=%d", r.target.Len("org-a", "item"), r.target.Batches())
	}
}

func TestRunner_SameInputTwiceIsReplayWithoutWrites(t *testing.T) {
	r := newRig(t)
	body := csvRows(r.origin)
	first := startAndWait(t, r, req(r, body))
	applies := r.target.Batches()
	second, err := r.runner.Start(context.Background(), req(r, body))
	if err != nil {
		t.Fatal(err)
	}
	if second.ReplayOf != first.ID || second.State != RunSucceeded || r.target.Batches() != applies {
		t.Fatalf("second = %+v", second)
	}
	if second.Progress != first.Progress {
		t.Fatal("el replay debe devolver el mismo resultado")
	}
	list, _ := r.ledger.List(context.Background(), "org-a", 10)
	if len(list) != 2 {
		t.Fatalf("el historial debe registrar ambos intentos, got %d", len(list))
	}
}

func TestRunner_ForceReapplyGivesAllUnchanged(t *testing.T) {
	r := newRig(t)
	body := csvRows(r.origin)
	startAndWait(t, r, req(r, body))
	forced := req(r, body)
	forced.Force = true
	run := startAndWait(t, r, forced)
	if run.ReplayOf != "" || run.Progress.Created != 0 || run.Progress.Updated != 0 || run.Progress.Unchanged != 3 {
		t.Fatalf("run = %+v", run)
	}
}

func TestRunner_ChangedInputOnlyTouchesChangedRows(t *testing.T) {
	r := newRig(t)
	body := csvRows(r.origin)
	startAndWait(t, r, req(r, body))
	run := startAndWait(t, r, req(r, []byte(replaceOnce(string(body), "299", "310"))))
	if run.Progress.Updated != 1 || run.Progress.Unchanged != 2 || run.Progress.Created != 0 {
		t.Fatalf("progress = %+v", run.Progress)
	}
}

func TestRunner_OverridesChangeTheRunKey(t *testing.T) {
	r := newRig(t)
	body := csvRows(r.origin)
	a := startAndWait(t, r, req(r, body))
	withOv := req(r, body)
	withOv.Overrides = &Overrides{IgnoreHeaders: []string{"Meta: *"}}
	b := startAndWait(t, r, withOv)
	if a.Key == b.Key || b.ReplayOf != "" || b.OverridesHash == "" {
		t.Fatalf("a=%s b=%+v", a.Key, b)
	}
}

func TestRunner_SameKeyDifferentOrgIsIndependent(t *testing.T) {
	r := newRig(t)
	body := csvRows(r.origin)
	startAndWait(t, r, req(r, body))
	other := req(r, body)
	other.Org = "org-b"
	run := startAndWait(t, r, other)
	if run.ReplayOf != "" || run.Progress.Created != 3 || r.target.Len("org-b", "item") != 3 {
		t.Fatalf("run = %+v", run)
	}
}

func TestRunner_ConcurrentStartsOfSameInputShareOneRun(t *testing.T) {
	r := newRig(t)
	release := make(chan struct{})
	r.target.FailApply = func(int) error { <-release; return nil }
	body := csvRows(r.origin)
	a, _ := r.runner.Start(context.Background(), req(r, body))
	b, _ := r.runner.Start(context.Background(), req(r, body))
	close(release)
	if a.ID != b.ID {
		t.Fatalf("debían ser la misma corrida: %s %s", a.ID, b.ID)
	}
	if _, err := r.runner.Wait(context.Background(), "org-a", a.ID); err != nil {
		t.Fatal(err)
	}
}

func TestRunner_StatusShowsRunningProgress(t *testing.T) {
	r := newRig(t)
	gate := make(chan struct{})
	var batches int32
	r.target.FailApply = func(int) error {
		if atomic.AddInt32(&batches, 1) == 2 {
			<-gate
		}
		return nil
	}
	run, _ := r.runner.Start(context.Background(), req(r, csvRows(r.origin)))
	var st Run
	for i := 0; i < 200; i++ {
		st, _ = r.runner.Status(context.Background(), "org-a", run.ID)
		if st.State == RunRunning && st.Progress.Done == 2 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if st.State != RunRunning || st.Progress.Done != 2 || st.Progress.Total != 3 {
		t.Fatalf("estado intermedio = %+v", st)
	}
	close(gate)
	final, _ := r.runner.Wait(context.Background(), "org-a", run.ID)
	if final.State != RunSucceeded || final.Progress.Done != 3 {
		t.Fatalf("final = %+v", final)
	}
}

func TestRunner_CancelKeepsAppliedBatchesAndRerunResumes(t *testing.T) {
	r := newRig(t)
	body := csvRows(r.origin)
	gate := make(chan struct{})
	var batches int32
	r.target.FailApply = func(int) error {
		if atomic.AddInt32(&batches, 1) == 2 {
			<-gate
		}
		return nil
	}
	run, _ := r.runner.Start(context.Background(), req(r, body))
	for i := 0; i < 200 && r.target.Batches() < 2; i++ {
		time.Sleep(5 * time.Millisecond)
	}
	r.runner.Cancel(run.ID)
	close(gate)
	final, _ := r.runner.Wait(context.Background(), "org-a", run.ID)
	if final.State != RunCanceled {
		t.Fatalf("estado = %s", final.State)
	}
	// Una corrida cancelada no cuenta como aplicada: repetir continúa y no duplica.
	again := startAndWait(t, r, req(r, body))
	if again.ReplayOf != "" || again.State != RunSucceeded || r.target.Len("org-a", "item") != 3 {
		t.Fatalf("again = %+v", again)
	}
	if again.Progress.Created+again.Progress.Unchanged != 3 {
		t.Fatalf("progress = %+v", again.Progress)
	}
}

func TestRunner_BatchErrorMarksRunFailedAndIsNotCountedAsApplied(t *testing.T) {
	r := newRig(t)
	boom := errors.New("base caída")
	r.target.FailApply = func(n int) error {
		if n == 2 {
			return boom
		}
		return nil
	}
	body := csvRows(r.origin)
	run := startAndWait(t, r, req(r, body))
	if run.State != RunFailed || !strings.Contains(run.Error, "base caída") || run.Progress.Done != 2 {
		t.Fatalf("run = %+v", run)
	}
	r.target.FailApply = nil
	retry := startAndWait(t, r, req(r, body))
	if retry.State != RunSucceeded || retry.ReplayOf != "" || retry.Progress.Created != 1 || retry.Progress.Unchanged != 2 {
		t.Fatalf("retry = %+v", retry)
	}
}

func TestRunner_FailOnInvalidAppliesNothing(t *testing.T) {
	r := newRig(t)
	rq := req(r, csvRows(r.origin, ",SIN ID,1,,1,,x"))
	rq.FailOnInvalid = true
	run := startAndWait(t, r, rq)
	if run.State != RunFailed || r.target.Len("org-a", "item") != 0 {
		t.Fatalf("run = %+v", run)
	}
}

func TestRunner_InvalidRowsAreSkippedAndReportedByDefault(t *testing.T) {
	r := newRig(t)
	run := startAndWait(t, r, req(r, csvRows(r.origin, ",SIN ID,1,,1,,x")))
	if run.State != RunSucceeded || run.Progress.Invalid != 1 || len(run.Issues) == 0 || run.Progress.Created != 3 {
		t.Fatalf("run = %+v", run)
	}
}

func TestRunner_RejectsInvalidProfileAndMissingOrg(t *testing.T) {
	r := newRig(t)
	bad := req(r, csvRows(r.origin))
	bad.Profile.Key = nil
	if _, err := r.runner.Start(context.Background(), bad); err == nil {
		t.Fatal("perfil inválido aceptado")
	}
	noOrg := req(r, csvRows(r.origin))
	noOrg.Org = ""
	if _, err := r.runner.Start(context.Background(), noOrg); err == nil {
		t.Fatal("org vacía aceptada")
	}
}

func TestLedger_GetIsScopedToOrg(t *testing.T) {
	r := newRig(t)
	run := startAndWait(t, r, req(r, csvRows(r.origin)))
	if _, err := r.runner.Status(context.Background(), "org-b", run.ID); !errors.Is(err, ErrRunNotFound) {
		t.Fatalf("otra org no debe ver la corrida: %v", err)
	}
}

func TestRunKey_DependsOnAllParts(t *testing.T) {
	base := RunKey("o", "p", "i", ModeApply)
	for _, k := range []string{RunKey("o2", "p", "i", ModeApply), RunKey("o", "p2", "i", ModeApply), RunKey("o", "p", "i2", ModeApply), RunKey("o", "p", "i", ModeDryRun)} {
		if k == base {
			t.Fatal("la llave debe depender de org, perfil, entrada y modo")
		}
	}
}
