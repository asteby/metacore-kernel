package dataload

import (
	"context"
)

// Autorización de las acciones que una persona dispara sobre los datos de una
// organización. El motor no sabe de roles ni de permisos: pregunta a un
// Authorizer (lo implementa ops con su sistema de permisos) con una acción
// nombrada y estable, y respeta la respuesta. Sin Authorizer el Runner niega,
// salvo que AllowAnyActor lo permita explícitamente (pruebas y desarrollo).

// Acciones nombradas. Son contrato (ops las mapea a permisos) y cada una tiene
// texto en el catálogo para los mensajes de "no autorizado".
const (
	// ActionReviewDecide: aprobar o rechazar filas en revisión (Runner.Decide).
	ActionReviewDecide Code = "dataload.action.review_decide"
	// ActionReviewApply: escribir en el destino las filas aprobadas (Runner.ApplyApproved).
	ActionReviewApply Code = "dataload.action.review_apply"
)

// AuthActions enumera las acciones nombradas que el motor pide autorizar.
func AuthActions() []Code { return []Code{ActionReviewDecide, ActionReviewApply} }

// AuthRequest es lo que se pide autorizar.
type AuthRequest struct {
	Org    string
	Actor  string
	Action Code
	// Entity es la entidad afectada ("*" si la llamada toca varias).
	Entity string
	// Keys son las identidades afectadas (para autorizar por registro si hace falta).
	Keys []string
}

// Authorizer decide si un actor puede hacer una acción. nil = permitido;
// cualquier error = denegado (ErrUnauthorized o el que explique el motivo; un
// fallo del propio sistema de permisos también niega).
type Authorizer interface {
	Authorize(ctx context.Context, req AuthRequest) error
}

// AuthorizerFunc adapta una función a Authorizer.
type AuthorizerFunc func(ctx context.Context, req AuthRequest) error

func (f AuthorizerFunc) Authorize(ctx context.Context, req AuthRequest) error { return f(ctx, req) }

// ErrUnauthorized: el actor no puede hacer la acción (errors.Is por código).
var ErrUnauthorized = newErr(CodeUnauthorized)

// ErrNoAuthorizer: el Runner no tiene Authorizer y no se le permitió operar sin él.
var ErrNoAuthorizer = newErr(CodeNoAuthorizer)

// authorize exige org y actor y consulta al Authorizer.
func (r *Runner) authorize(ctx context.Context, req AuthRequest) error {
	action := msg(req.Action)
	if req.Org == "" || req.Actor == "" {
		return newErr(CodeActorRequired, "action", action)
	}
	if r.Authorizer == nil {
		if r.AllowAnyActor {
			return nil
		}
		return newErr(CodeNoAuthorizer, "action", action)
	}
	err := r.Authorizer.Authorize(ctx, req)
	if err == nil {
		return nil
	}
	denied := newErr(CodeUnauthorized, "actor", req.Actor, "action", action, "org", req.Org)
	if e, ok := err.(*Error); ok && e.Code == CodeUnauthorized && len(e.Params) == 0 {
		return denied // el centinela pelón: no se repite como causa
	}
	denied.Cause = err // el motivo del Authorizer (o su fallo) queda como causa
	return denied
}
