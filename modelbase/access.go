package modelbase

import (
	"context"
	"strings"
	"sync"
)

// AccessAction is one of the five dynamic-CRUD operations an AccessPolicy
// gates. The string values are part of the wire contract (they appear in the
// 403 body the dynamic handler answers).
type AccessAction string

const (
	// AccessList gates GET /dynamic/:model and every read that enumerates rows
	// of the model (aggregate, facets, export, options, search).
	AccessList AccessAction = "list"
	// AccessGet gates GET /dynamic/:model/:id and GET /dynamic/:model/current.
	AccessGet AccessAction = "get"
	// AccessCreate gates POST /dynamic/:model and the spreadsheet import.
	AccessCreate AccessAction = "create"
	// AccessUpdate gates PUT /dynamic/:model/:id and row actions.
	AccessUpdate AccessAction = "update"
	// AccessDelete gates DELETE /dynamic/:model/:id.
	AccessDelete AccessAction = "delete"
)

// AccessRule says who may perform one action. A caller passes the rule when
// ANY of its clauses matches:
//
//   - Public: any authenticated principal (the historical dynamic behaviour);
//   - Roles: the principal holds one of these roles. Roles are compared
//     case-insensitively against the JWT role, the roles a RolesProvider
//     principal carries (see WithRoles) and the host's role resolver. There is
//     NO super-role bypass here: "owner" only matches a rule that names it;
//   - Capabilities: the permission service grants one of these capabilities
//     (e.g. "promotions.manage"). It follows permission.Service semantics,
//     including its super roles (by default "owner" holds every capability),
//     and never matches when the host wired no permission service.
//
// A rule with no clause set matches nobody (see AccessNobody).
type AccessRule struct {
	Public       bool     `json:"public,omitempty"`
	Roles        []string `json:"roles,omitempty"`
	Capabilities []string `json:"capabilities,omitempty"`
}

// AccessPolicy is the declarative per-model access policy the dynamic CRUD
// enforces on top of the permission service. It can only NARROW access: a
// request must satisfy the policy AND the capability check the dynamic
// runtime already performs (`<model>.<read|create|update|delete>`).
//
// A nil rule falls back to Default; a nil Default means "no restriction" for
// that action, i.e. today's behaviour. A model with no policy at all is
// exactly as before.
type AccessPolicy struct {
	List    *AccessRule `json:"list,omitempty"`
	Get     *AccessRule `json:"get,omitempty"`
	Create  *AccessRule `json:"create,omitempty"`
	Update  *AccessRule `json:"update,omitempty"`
	Delete  *AccessRule `json:"delete,omitempty"`
	Default *AccessRule `json:"default,omitempty"`
}

// HasAccessPolicy is the optional interface a model implements to declare its
// access policy next to its metadata:
//
//	func (Promotion) DefineAccess() modelbase.AccessPolicy {
//	    return modelbase.AccessPublicReadStaffWrite("store.admin")
//	}
type HasAccessPolicy interface {
	DefineAccess() AccessPolicy
}

// Rule returns the rule that governs action: the action's own rule, else
// Default, else nil (unrestricted).
func (p AccessPolicy) Rule(action AccessAction) *AccessRule {
	var r *AccessRule
	switch action {
	case AccessList:
		r = p.List
	case AccessGet:
		r = p.Get
	case AccessCreate:
		r = p.Create
	case AccessUpdate:
		r = p.Update
	case AccessDelete:
		r = p.Delete
	}
	if r == nil {
		r = p.Default
	}
	return r
}

// IsZero reports whether the policy restricts nothing.
func (p AccessPolicy) IsZero() bool {
	return p.List == nil && p.Get == nil && p.Create == nil && p.Update == nil && p.Delete == nil && p.Default == nil
}

// MatchesRoles reports whether one of roles satisfies the rule's Public or
// Roles clause (case-insensitive). Capabilities are checked by the caller,
// which owns the permission service.
func (r *AccessRule) MatchesRoles(roles []string) bool {
	if r == nil {
		return true
	}
	if r.Public {
		return true
	}
	for _, want := range r.Roles {
		w := strings.ToLower(strings.TrimSpace(want))
		if w == "" {
			continue
		}
		for _, have := range roles {
			if strings.ToLower(strings.TrimSpace(have)) == w {
				return true
			}
		}
	}
	return false
}

// AccessPublic lets every authenticated principal through.
func AccessPublic() *AccessRule { return &AccessRule{Public: true} }

// AccessRoles lets through principals holding any of roles.
func AccessRoles(roles ...string) *AccessRule { return &AccessRule{Roles: roles} }

// AccessCapabilities lets through principals granted any of caps by the
// permission service.
func AccessCapabilities(caps ...string) *AccessRule { return &AccessRule{Capabilities: caps} }

// AccessNobody blocks the action for every HTTP principal. Server code keeps
// its own path (dynamic.NewSystemCaller, hooks, custom endpoints).
func AccessNobody() *AccessRule { return &AccessRule{} }

// OrRoles returns a copy of r that also admits roles.
func (r AccessRule) OrRoles(roles ...string) *AccessRule {
	r.Roles = append(append([]string(nil), r.Roles...), roles...)
	return &r
}

// OrCapabilities returns a copy of r that also admits caps.
func (r AccessRule) OrCapabilities(caps ...string) *AccessRule {
	r.Capabilities = append(append([]string(nil), r.Capabilities...), caps...)
	return &r
}

// AccessPublicReadStaffWrite is the catalogue preset: every authenticated
// principal may list/get; only staffRoles may create, update or delete.
func AccessPublicReadStaffWrite(staffRoles ...string) AccessPolicy {
	return AccessPolicy{
		List:    AccessPublic(),
		Get:     AccessPublic(),
		Default: AccessRoles(staffRoles...),
	}
}

// AccessStaffOnly restricts every action to staffRoles.
func AccessStaffOnly(staffRoles ...string) AccessPolicy {
	return AccessPolicy{Default: AccessRoles(staffRoles...)}
}

// AccessReadOnly lets every authenticated principal read and nobody write
// through the dynamic CRUD (writes go through the app's own endpoints).
func AccessReadOnly() AccessPolicy {
	return AccessPolicy{List: AccessPublic(), Get: AccessPublic(), Default: AccessNobody()}
}

// RolesProvider is the optional interface a principal implements to carry
// roles beyond its single GetRole() — platform roles such as "store.admin"
// that are not stored in the JWT. See WithRoles.
type RolesProvider interface {
	GetRoles() []string
}

// WithRoles wraps user so that it also reports extra roles via RolesProvider.
// The extra roles are computed lazily, once, on the first GetRoles call, so
// a resolver that hits the database costs nothing on routes that never check
// a policy. Every AuthUser method keeps delegating to user.
func WithRoles(user AuthUser, resolve func() []string) AuthUser {
	if user == nil || resolve == nil {
		return user
	}
	return &rolesPrincipal{AuthUser: user, resolve: resolve}
}

type rolesPrincipal struct {
	AuthUser
	resolve func() []string
	once    sync.Once
	roles   []string
}

func (p *rolesPrincipal) GetRoles() []string {
	p.once.Do(func() { p.roles = p.resolve() })
	out := make([]string, 0, len(p.roles)+1)
	if r := p.AuthUser.GetRole(); r != "" {
		out = append(out, r)
	}
	return append(out, p.roles...)
}

// Unwrap returns the wrapped principal, for code that type-asserts on the
// app's concrete user type.
func (p *rolesPrincipal) Unwrap() AuthUser { return p.AuthUser }

// PrincipalRoles returns every role user holds: GetRole() plus the roles of a
// RolesProvider. Duplicates are harmless for matching.
func PrincipalRoles(user AuthUser) []string {
	if user == nil {
		return nil
	}
	if rp, ok := user.(RolesProvider); ok {
		return rp.GetRoles()
	}
	if r := user.GetRole(); r != "" {
		return []string{r}
	}
	return nil
}

// --- registry -------------------------------------------------------------

var (
	accessMu        sync.RWMutex
	accessPolicies  = map[string]AccessPolicy{}
	singletonModels = map[string]bool{}
)

// SetAccessPolicy registers the policy for the model registered under key. It
// overrides the model's own DefineAccess. host.WithAccess calls it.
func SetAccessPolicy(key string, p AccessPolicy) {
	if key == "" {
		return
	}
	accessMu.Lock()
	defer accessMu.Unlock()
	accessPolicies[key] = p
}

// AccessPolicyFor returns the effective policy of the model registered under
// key: the registered one (SetAccessPolicy), else instance.DefineAccess(), else
// ok=false (no policy — historical behaviour).
func AccessPolicyFor(key string, instance any) (AccessPolicy, bool) {
	accessMu.RLock()
	p, ok := accessPolicies[key]
	accessMu.RUnlock()
	if ok {
		return p, true
	}
	if hp, ok := instance.(HasAccessPolicy); ok {
		return hp.DefineAccess(), true
	}
	return AccessPolicy{}, false
}

// Singleton is the optional interface of a model that holds at most one row
// per organization (settings, profile, configuration). Embed SingletonModel
// to implement it. The dynamic CRUD then refuses a second create for the org
// (409), serves GET/PUT /dynamic/:model/current, and the served
// TableMetadata carries `singleton: true` so the UI renders a settings form
// instead of a table.
type Singleton interface {
	IsSingleton() bool
}

// SingletonModel is an embeddable zero-size marker implementing Singleton.
// It has no fields, so GORM and JSON ignore it.
type SingletonModel struct{}

// IsSingleton implements Singleton.
func (SingletonModel) IsSingleton() bool { return true }

// SingletonDefaulter is optionally implemented by a Singleton model to supply
// the input used when GET /dynamic/:model/current materializes the org's row
// for the first time. Without it the row is created from an empty input
// (column defaults).
type SingletonDefaulter interface {
	SingletonDefaults(ctx context.Context) map[string]any
}

// MarkSingleton flags the model registered under key as a singleton without
// touching its type. host.AsSingleton calls it.
func MarkSingleton(key string) {
	if key == "" {
		return
	}
	accessMu.Lock()
	defer accessMu.Unlock()
	singletonModels[key] = true
}

// IsSingletonModel reports whether the model registered under key is a
// singleton: marked with MarkSingleton, or instance implements Singleton
// returning true.
func IsSingletonModel(key string, instance any) bool {
	accessMu.RLock()
	marked := singletonModels[key]
	accessMu.RUnlock()
	if marked {
		return true
	}
	if s, ok := instance.(Singleton); ok {
		return s.IsSingleton()
	}
	return false
}
