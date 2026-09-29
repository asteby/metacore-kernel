package dynamic

import (
	"context"
	"errors"

	"github.com/google/uuid"

	"github.com/asteby/metacore-kernel/modelbase"
	"github.com/asteby/metacore-kernel/permission"
)

// AccessPolicyResolver returns the access policy of a model the host knows
// about outside the Go registry (addon models, manifest-declared policies).
// ok=false defers to modelbase.AccessPolicyFor (SetAccessPolicy, then the
// model's DefineAccess). See Config.AccessPolicyResolver.
type AccessPolicyResolver func(ctx context.Context, model string) (modelbase.AccessPolicy, bool)

// capActionFor maps a policy action to the capability verb the permission
// service has always been asked for (`<model>.read|create|update|delete`).
func capActionFor(action modelbase.AccessAction) string {
	switch action {
	case modelbase.AccessList, modelbase.AccessGet:
		return "read"
	default:
		return string(action)
	}
}

// AccessPolicy returns the effective policy of model, ok=false when it has
// none (unrestricted beyond the permission service).
func (s *Service) AccessPolicy(ctx context.Context, model string) (modelbase.AccessPolicy, bool) {
	inst, _ := s.lookupModel(ctx, model)
	return s.accessPolicyFor(ctx, model, inst)
}

func (s *Service) accessPolicyFor(ctx context.Context, model string, instance any) (modelbase.AccessPolicy, bool) {
	if s.accessPolicies != nil {
		if p, ok := s.accessPolicies(ctx, model); ok {
			return p, !p.IsZero()
		}
	}
	p, ok := modelbase.AccessPolicyFor(model, instance)
	return p, ok && !p.IsZero()
}

// principalRoles is every role the policy matcher considers for user: its
// JWT role, the roles of a modelbase.RolesProvider principal (host
// RoleResolver) and Config.ActorRolesResolver.
func (s *Service) principalRoles(ctx context.Context, user modelbase.AuthUser) []string {
	roles := modelbase.PrincipalRoles(user)
	if s.actorRolesResolver != nil {
		roles = append(roles, s.actorRolesResolver(ctx, user)...)
	}
	return roles
}

// checkAccess enforces the model's declarative AccessPolicy for one action.
// No policy, or no rule for the action, allows (historical behaviour). The
// system caller bypasses it, like checkPerm.
func (s *Service) checkAccess(ctx context.Context, user modelbase.AuthUser, model string, instance any, action modelbase.AccessAction) error {
	if _, ok := user.(systemPrincipal); ok {
		return nil
	}
	p, ok := s.accessPolicyFor(ctx, model, instance)
	if !ok {
		return nil
	}
	rule := p.Rule(action)
	if rule == nil {
		return nil
	}
	denied := &AccessDeniedError{Model: model, Action: string(action)}
	if user == nil {
		return denied
	}
	if rule.MatchesRoles(s.principalRoles(ctx, user)) {
		return nil
	}
	if len(rule.Capabilities) > 0 && s.perms != nil {
		caps := make([]permission.Capability, 0, len(rule.Capabilities))
		for _, c := range rule.Capabilities {
			caps = append(caps, permission.Capability(c))
		}
		if err := s.perms.CheckAny(ctx, user, caps...); err == nil {
			return nil
		}
	}
	return denied
}

// authorize is the single gate of every dynamic operation: the model's
// AccessPolicy (which can only narrow) AND the capability check.
func (s *Service) authorize(ctx context.Context, user modelbase.AuthUser, model string, instance any, action modelbase.AccessAction) error {
	if err := s.checkAccess(ctx, user, model, instance, action); err != nil {
		return err
	}
	return s.checkPerm(ctx, user, model, capActionFor(action))
}

// Can reports whether user may perform action on model, without side
// effects. Useful for UIs (show/hide the create button) and custom endpoints.
func (s *Service) Can(ctx context.Context, user modelbase.AuthUser, model string, action modelbase.AccessAction) bool {
	inst, ok := s.lookupModel(ctx, model)
	if !ok {
		return false
	}
	return s.authorize(ctx, user, model, inst, action) == nil
}

// --- singleton --------------------------------------------------------------

// isSingleton reports whether model holds one row per organization: marked
// with modelbase.MarkSingleton, implementing modelbase.Singleton, or served
// with TableMetadata.Singleton (e.g. set by a host transformer).
func (s *Service) isSingleton(model string, instance any, meta *modelbase.TableMetadata) bool {
	if modelbase.IsSingletonModel(model, instance) {
		return true
	}
	return meta != nil && meta.Singleton
}

// findSingletonID returns the id of the organization's row of a singleton
// model, "" when there is none. Soft-deleted rows do not count.
func (s *Service) findSingletonID(ctx context.Context, user modelbase.AuthUser, tableName string, instance any) (string, error) {
	q := s.scope.ScopeQuery(s.db.WithContext(ctx).Table(tableName), user)
	q = scopeSoftDelete(q, instance)
	var ids []string
	if err := q.Limit(1).Pluck("id", &ids).Error; err != nil {
		return "", err
	}
	if len(ids) == 0 {
		return "", nil
	}
	return ids[0], nil
}

// guardSingletonCreate refuses a create when the organization already has
// the singleton's row.
func (s *Service) guardSingletonCreate(ctx context.Context, model string, user modelbase.AuthUser, tableName string, instance any, meta *modelbase.TableMetadata) error {
	if !s.isSingleton(model, instance, meta) {
		return nil
	}
	id, err := s.findSingletonID(ctx, user, tableName, instance)
	if err != nil {
		return err
	}
	if id != "" {
		return &SingletonExistsError{Model: model, ID: id}
	}
	return nil
}

// SingletonResult is what GetSingleton answers: the row, and whether it is
// persisted (false = the defaults of a row the caller may not create).
type SingletonResult struct {
	Data      map[string]any
	Persisted bool
}

// GetSingleton returns the organization's row of a singleton model. When the
// row does not exist yet it is created from the model's SingletonDefaults
// (or the column defaults) if the caller may create it; otherwise the
// defaults are returned unsaved (Persisted=false).
func (s *Service) GetSingleton(ctx context.Context, model string, user modelbase.AuthUser) (SingletonResult, error) {
	instance, meta, err := s.resolveModel(ctx, model)
	if err != nil {
		return SingletonResult{}, err
	}
	if !s.isSingleton(model, instance, meta) {
		return SingletonResult{}, ErrNotSingleton
	}
	if err := s.authorize(ctx, user, model, instance, modelbase.AccessGet); err != nil {
		return SingletonResult{}, err
	}
	tableName, err := s.tableNameFor(ctx, model, instance)
	if err != nil {
		return SingletonResult{}, err
	}
	id, err := s.findSingletonID(ctx, user, tableName, instance)
	if err != nil {
		return SingletonResult{}, err
	}
	if id == "" {
		defaults := singletonDefaults(ctx, instance)
		if s.authorize(ctx, user, model, instance, modelbase.AccessCreate) != nil {
			return SingletonResult{Data: defaults, Persisted: false}, nil
		}
		row, err := s.Create(ctx, model, user, defaults)
		var se *SingletonExistsError
		switch {
		case errors.As(err, &se):
			id = se.ID // lost a race with a concurrent materialization
		case err != nil:
			return SingletonResult{}, err
		default:
			return SingletonResult{Data: row, Persisted: true}, nil
		}
	}
	uid, err := uuid.Parse(id)
	if err != nil {
		return SingletonResult{}, err
	}
	row, err := s.Get(ctx, model, user, uid)
	if err != nil {
		return SingletonResult{}, err
	}
	return SingletonResult{Data: row, Persisted: true}, nil
}

// SaveSingleton upserts the organization's row of a singleton model: an
// update of the existing row, or a create (over the SingletonDefaults) when
// there is none. Each path runs its own authorization.
func (s *Service) SaveSingleton(ctx context.Context, model string, user modelbase.AuthUser, input map[string]any) (map[string]any, error) {
	instance, meta, err := s.resolveModel(ctx, model)
	if err != nil {
		return nil, err
	}
	if !s.isSingleton(model, instance, meta) {
		return nil, ErrNotSingleton
	}
	tableName, err := s.tableNameFor(ctx, model, instance)
	if err != nil {
		return nil, err
	}
	if err := s.authorize(ctx, user, model, instance, modelbase.AccessGet); err != nil {
		return nil, err
	}
	id, err := s.findSingletonID(ctx, user, tableName, instance)
	if err != nil {
		return nil, err
	}
	if id == "" {
		merged := singletonDefaults(ctx, instance)
		for k, v := range input {
			merged[k] = v
		}
		return s.Create(ctx, model, user, merged)
	}
	uid, err := uuid.Parse(id)
	if err != nil {
		return nil, err
	}
	delete(input, "id")
	return s.Update(ctx, model, user, uid, input)
}

func singletonDefaults(ctx context.Context, instance any) map[string]any {
	out := map[string]any{}
	if d, ok := instance.(modelbase.SingletonDefaulter); ok {
		for k, v := range d.SingletonDefaults(ctx) {
			out[k] = v
		}
	}
	return out
}

// --- Go-model validation ------------------------------------------------------

// validateModel runs the model's own validation (modelbase.Validatable /
// modelbase.WriteValidator) on the instance the input was merged into, right
// before the write. op is "create" or "update".
func validateModel(ctx context.Context, instance any, op string) error {
	ve := NewValidationError()
	if v, ok := instance.(modelbase.Validatable); ok {
		for field, msg := range v.Validate() {
			ve.AddMessage(field, msg)
		}
	}
	if w, ok := instance.(modelbase.WriteValidator); ok {
		if err := w.ValidateWrite(ctx, op); err != nil {
			var fe modelbase.FieldErrors
			var other *ValidationError
			switch {
			case errors.As(err, &fe):
				for field, msg := range fe {
					ve.AddMessage(field, msg)
				}
			case errors.As(err, &other):
				for field, errs := range other.Fields {
					for _, e := range errs {
						ve.Fields = appendField(ve.Fields, field, e)
					}
				}
			default:
				return err
			}
		}
	}
	return ve.Err()
}

func appendField(m map[string][]FieldError, field string, e FieldError) map[string][]FieldError {
	if m == nil {
		m = make(map[string][]FieldError)
	}
	m[field] = append(m[field], e)
	return m
}

// validationFromFieldErrors converts a modelbase.FieldErrors (returned by a
// hook) to the wire ValidationError.
func validationFromFieldErrors(fe modelbase.FieldErrors) *ValidationError {
	ve := NewValidationError()
	for field, msg := range fe {
		ve.AddMessage(field, msg)
	}
	return ve
}
