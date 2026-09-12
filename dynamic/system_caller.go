package dynamic

import (
	"github.com/google/uuid"

	"github.com/asteby/metacore-kernel/modelbase"
)

// SystemActorID is the fixed actor id stamped on writes made through a
// system-caller principal (created_by_id, canonical event ActorID, approval
// RequestedBy). Using one well-known id — instead of a random one per call —
// lets audit trails and event subscribers recognize system-driven writes by
// construction, without inspecting the role.
var SystemActorID = uuid.MustParse("00000000-0000-0000-0000-000000000001")

// SystemRole is the role reported by a system-caller principal's GetRole().
// It is informational only: checkPerm never looks at it to decide whether to
// bypass authorization (see systemPrincipal below) — a normal AuthUser that
// happens to set Role to "system" gets NO special treatment.
const SystemRole = "system"

// systemPrincipal is the concrete modelbase.AuthUser implementation returned
// by NewSystemCaller. It is deliberately unexported: the ONLY way any code —
// inside or outside this module — can obtain a value of this type is to call
// NewSystemCaller explicitly. checkPerm below bypasses the permission gate by
// asserting on THIS CONCRETE TYPE, not on a role string or an exported
// marker interface, so nothing an HTTP request controls (role claims, JWT
// content, query params) can ever produce one by accident: a normal request
// handler builds its AuthUser via AuthUserExtractor / UserResolver, which
// never returns this type unless the host explicitly wires NewSystemCaller
// into it — and doing that would defeat the whole point, so don't.
type systemPrincipal struct {
	orgID uuid.UUID
}

func (systemPrincipal) GetID() uuid.UUID               { return SystemActorID }
func (p systemPrincipal) GetOrganizationID() uuid.UUID { return p.orgID }
func (systemPrincipal) GetEmail() string               { return "" }
func (systemPrincipal) GetRole() string                { return SystemRole }
func (systemPrincipal) GetPasswordHash() string        { return "" }
func (systemPrincipal) SetEmail(string)                {}
func (systemPrincipal) SetName(string)                 {}
func (systemPrincipal) SetPasswordHash(string)         {}
func (systemPrincipal) SetRole(string)                 {}
func (systemPrincipal) SetOrganizationID(uuid.UUID)    {}

// NewSystemCaller builds an AuthUser principal for SYSTEM PROCESSES ONLY —
// background workers, scheduled jobs, one-off migrations — that need to call
// Service.Create / Update / Delete / Get / List / Aggregate without an HTTP
// request to authorize, because there is no logged-in user behind the call.
//
// Passing the result to Service methods SKIPS the permission gate entirely
// (Config.Permissions.Check is never invoked for it) but changes NOTHING
// else: tenant scoping (organization_id = orgID), field validation, hooks,
// declarative guards, canonical events, folio sequences and relation sync
// all still run exactly as they do for a real user — scoped to orgID. A
// system caller for org A can never read or write org B's rows.
//
// # SECURITY — read before using
//
//   - NEVER construct this from data an HTTP request controls (a header, a
//     JWT claim, a query param, a request body field). orgID must come from
//     the WORKER'S OWN configuration or from a row it already loaded through
//     an authorized path — never from the request the worker is processing.
//   - NEVER wire this into an AuthUserExtractor, UserResolver, or any other
//     path that serves a live HTTP request. Doing so would let every request
//     skip authorization, which is the exact failure mode this type exists
//     to make hard, not easy.
//   - Use it only where a real AuthUser cannot exist: sync workers, cron
//     jobs, backfills, and similar system-initiated processes.
func NewSystemCaller(orgID uuid.UUID) modelbase.AuthUser {
	return systemPrincipal{orgID: orgID}
}
