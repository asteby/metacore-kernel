package dataload

import (
	"context"
	"errors"
	"sync"
	"testing"
)

// recordingAuthorizer guarda lo que se le pregunta y niega según deny.
type recordingAuthorizer struct {
	mu   sync.Mutex
	reqs []AuthRequest
	deny func(AuthRequest) error
}

func (a *recordingAuthorizer) Authorize(_ context.Context, req AuthRequest) error {
	a.mu.Lock()
	a.reqs = append(a.reqs, req)
	a.mu.Unlock()
	if a.deny != nil {
		return a.deny(req)
	}
	return nil
}

// Punto 5: Decide y ApplyApproved preguntan al Authorizer con acciones nombradas;
// una negativa no registra ni escribe nada.
func TestAuth_DecideAndApplyApprovedAskTheAuthorizerWithNamedActions(t *testing.T) {
	r := newRig(t)
	auth := &recordingAuthorizer{deny: func(req AuthRequest) error {
		if req.Actor == "lectora" && req.Action == ActionReviewApply {
			return ErrUnauthorized
		}
		return nil
	}}
	r.runner.Reviews, r.runner.Authorizer = NewMemoryReviewStore(), auth
	ctx := context.Background()
	startAndWait(t, r, listaRequest(t))

	p5, _, _ := r.runner.Reviews.Get(ctx, "org-a", "product", "P5")
	if _, err := r.runner.Decide(ctx, "org-a", "lectora", []ReviewDecision{{Entity: "product", Key: "P5", Hash: p5.Hash, Approve: true}}); err != nil {
		t.Fatal(err)
	}
	if got := auth.reqs[0]; got.Action != ActionReviewDecide || got.Org != "org-a" || got.Actor != "lectora" || got.Entity != "product" || len(got.Keys) != 1 || got.Keys[0] != "P5" {
		t.Fatalf("solicitud de Decide: %+v", got)
	}

	_, err := r.runner.ApplyApproved(ctx, "org-a", "lectora", "product")
	if !errors.Is(err, ErrUnauthorized) || ErrorCode(err) != CodeUnauthorized {
		t.Fatalf("ApplyApproved negado: %v", err)
	}
	if err.Error() != `dataload: "lectora" no puede aplicar filas aprobadas en la organización "org-a"` {
		t.Fatalf("texto: %q", err.Error())
	}
	if got := auth.reqs[1]; got.Action != ActionReviewApply || got.Keys[0] != "P5" {
		t.Fatalf("solicitud de ApplyApproved: %+v", got)
	}
	if _, ok := r.target.Get("org-a", "product", "P5"); ok {
		t.Fatal("una negativa no escribe en el destino")
	}
	if e, _, _ := r.runner.Reviews.Get(ctx, "org-a", "product", "P5"); e.State != ReviewApproved {
		t.Fatalf("la fila sigue aprobada sin aplicar: %s", e.State)
	}
	if run, err := r.runner.ApplyApproved(ctx, "org-a", "ana", "product"); err != nil || run.Progress.Created != 1 {
		t.Fatalf("con permiso: %v %+v", err, run)
	}
}

// Punto 5: sin Authorizer se niega (seguro por omisión); AllowAnyActor lo abre
// explícitamente, pero nunca sin org ni actor.
func TestAuth_WithoutAuthorizerDeniesUnlessAllowAnyActor(t *testing.T) {
	r := newRig(t)
	r.runner.Reviews, r.runner.Authorizer = NewMemoryReviewStore(), nil
	ctx := context.Background()
	if _, err := r.runner.Decide(ctx, "org-a", "ana", nil); !errors.Is(err, ErrNoAuthorizer) {
		t.Fatalf("sin Authorizer: %v", err)
	}
	if _, err := r.runner.ApplyApproved(ctx, "org-a", "ana", "product"); !errors.Is(err, ErrNoAuthorizer) {
		t.Fatalf("sin Authorizer (apply): %v", err)
	}
	r.runner.AllowAnyActor = true
	if _, err := r.runner.Decide(ctx, "org-a", "ana", nil); err != nil {
		t.Fatalf("AllowAnyActor: %v", err)
	}
	if _, err := r.runner.Decide(ctx, "org-a", "", nil); ErrorCode(err) != CodeActorRequired {
		t.Fatalf("sin actor: %v", err)
	}
	if _, err := r.runner.ApplyApproved(ctx, "", "ana", "product"); ErrorCode(err) != CodeActorRequired {
		t.Fatalf("sin org: %v", err)
	}
}

// Punto 5: si el sistema de permisos falla, se niega y la causa se conserva.
func TestAuth_AuthorizerFailureDeniesAndKeepsTheCause(t *testing.T) {
	r := newRig(t)
	down := errors.New("permisos: sin conexión")
	r.runner.Reviews = NewMemoryReviewStore()
	r.runner.Authorizer = AuthorizerFunc(func(context.Context, AuthRequest) error { return down })
	_, err := r.runner.Decide(context.Background(), "org-a", "ana", nil)
	if !errors.Is(err, ErrUnauthorized) || !errors.Is(err, down) {
		t.Fatalf("fallo del Authorizer: %v", err)
	}
	if len(AuthActions()) != 2 {
		t.Fatal("acciones nombradas")
	}
}
