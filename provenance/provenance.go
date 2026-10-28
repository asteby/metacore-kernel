// Package provenance is the kernel's first-class answer to "who made this
// record, and how": a person on their own, an agent (the app's assistant, an
// automation run), the system (seed, migration, scheduled job), an
// integration (an importer, a webhook) — or a person and an agent together,
// when a human authorized what the agent proposed.
//
// # Model
//
// A Provenance travels in the context.Context of a request or job (WithContext
// / FromContext). Hosts set it once at the edge — the HTTP middleware for a
// logged-in person, the chat handler when a proposal is approved, the
// automation runner, an importer — and every write below inherits it:
//
//   - Register installs GORM callbacks that stamp created_by_id/updated_by_id
//     (only on models that already have those columns, never adding columns)
//     and append one row per changed record to the lateral table
//     metacore_provenance. Writes without a Provenance in ctx are untouched, so
//     existing code keeps working exactly as before.
//   - dynamic.Service copies it into the CanonicalEvent, and the audit
//     Recorder stores it on the activity/audit entry.
//   - Store reads it back: the creation provenance of a page of records
//     (tables), and the change history of one record (detail view).
//
// # Modes
//
//	manual     a person did it directly                     (human)
//	assisted   a person authorized what an agent proposed   (agent + authorized_by)
//	automated  nobody was in the loop at that moment        (agent, system, integration)
//
// An agent output that still waits for a person (a tentative calendar event, a
// draft) is automated with Proposed=true; the confirmation is a new change
// with mode assisted.
package provenance

import (
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
)

// ActorKind is what kind of actor made the change.
type ActorKind string

const (
	ActorHuman       ActorKind = "human"
	ActorAgent       ActorKind = "agent"
	ActorSystem      ActorKind = "system"
	ActorIntegration ActorKind = "integration"
)

// Mode is how much a person was involved.
type Mode string

const (
	ModeManual    Mode = "manual"
	ModeAssisted  Mode = "assisted"
	ModeAutomated Mode = "automated"
)

// Via values: the channel the change came through. Free-form strings are
// accepted; these are the ones the kernel and the SDK know how to label.
const (
	ViaUI         = "ui"
	ViaAPI        = "api"
	ViaChat       = "chat"
	ViaAutomation = "automation"
	ViaImport     = "import"
	ViaWebhook    = "webhook"
	ViaSeed       = "seed"
	ViaMigration  = "migration"
	ViaJob        = "job"
	ViaEvent      = "event"
)

// Ref kinds: what RefID points to.
const (
	RefSession       = "session"
	RefToolCall      = "tool_call"
	RefProposal      = "proposal"
	RefAutomation    = "automation"
	RefAutomationRun = "automation_run"
	RefImportBatch   = "import_batch"
	RefRun           = "run"
	RefSuggestion    = "suggestion"
)

// Provenance describes who made a change and how. The zero value is "no
// provenance" (IsZero) and is ignored everywhere.
type Provenance struct {
	ActorKind ActorKind `json:"actor_kind"`
	// ActorID is the person (human) or the agent/integration id when it has
	// one. Agents and system processes usually leave it nil.
	ActorID *uuid.UUID `json:"actor_id,omitempty"`
	// ActorName is the agent or integration label ("Asistente", "data-sync").
	// Never a person's name: the host resolves people from ids at read time.
	ActorName string `json:"actor_name,omitempty"`
	// OnBehalfOf is the person the agent acted for (automation owner, the
	// person talking to the copilot).
	OnBehalfOf *uuid.UUID `json:"on_behalf_of,omitempty"`
	// AuthorizedBy / AuthorizedAt: the person who approved the agent's
	// proposal, and when. Set ⇒ mode assisted.
	AuthorizedBy *uuid.UUID `json:"authorized_by,omitempty"`
	AuthorizedAt *time.Time `json:"authorized_at,omitempty"`
	// Via is the channel (ViaChat, ViaAutomation, ViaImport...).
	Via string `json:"via,omitempty"`
	// Origin is a short human label of the concrete source ("Briefing
	// automático", "Borrador aprobado en el chat", automation name).
	Origin string `json:"origin,omitempty"`
	// RefKind/RefID point to the session, tool call, proposal, run or
	// automation behind the change.
	RefKind string `json:"ref_kind,omitempty"`
	RefID   string `json:"ref_id,omitempty"`
	// SessionID is the copilot conversation, when there is one.
	SessionID *uuid.UUID `json:"session_id,omitempty"`
	// Summary is what the agent proposed, in one line.
	Summary string `json:"summary,omitempty"`
	Mode    Mode   `json:"mode"`
	// Proposed marks an agent output that still waits for a person.
	Proposed bool `json:"proposed,omitempty"`
}

// MaxSummary / MaxOrigin bound the free-text fields stored per change.
const (
	MaxSummary = 300
	MaxOrigin  = 160
)

// IsZero reports whether p carries no provenance.
func (p Provenance) IsZero() bool { return p.ActorKind == "" }

// Normalize fills Mode when empty and trims/bounds the text fields:
// human → manual; agent with AuthorizedBy → assisted; anything else →
// automated. A Proposed output can never be assisted (nobody authorized it
// yet). Returns the normalized copy.
func (p Provenance) Normalize() Provenance {
	p.ActorName = strings.TrimSpace(p.ActorName)
	p.Origin = clip(strings.TrimSpace(p.Origin), MaxOrigin)
	p.Summary = clip(strings.TrimSpace(p.Summary), MaxSummary)
	p.Via = strings.TrimSpace(p.Via)
	if p.AuthorizedBy != nil && *p.AuthorizedBy == uuid.Nil {
		p.AuthorizedBy = nil
	}
	if p.OnBehalfOf != nil && *p.OnBehalfOf == uuid.Nil {
		p.OnBehalfOf = nil
	}
	if p.ActorID != nil && *p.ActorID == uuid.Nil {
		p.ActorID = nil
	}
	if p.Mode == "" {
		switch {
		case p.ActorKind == ActorHuman:
			p.Mode = ModeManual
		case p.AuthorizedBy != nil && !p.Proposed:
			p.Mode = ModeAssisted
		default:
			p.Mode = ModeAutomated
		}
	}
	if p.Proposed && p.Mode == ModeAssisted {
		p.Mode = ModeAutomated
	}
	if p.AuthorizedBy != nil && p.AuthorizedAt == nil && !p.Proposed {
		now := time.Now().UTC()
		p.AuthorizedAt = &now
	}
	return p
}

// Errors returned by Validate.
var (
	ErrNoActorKind    = errors.New("provenance: actor_kind is required")
	ErrBadActorKind   = errors.New("provenance: unknown actor_kind")
	ErrBadMode        = errors.New("provenance: unknown mode")
	ErrHumanNoActor   = errors.New("provenance: a human actor needs actor_id")
	ErrAssistedNoAuth = errors.New("provenance: assisted mode needs authorized_by")
)

// Validate checks a (normalized) provenance is coherent.
func (p Provenance) Validate() error {
	switch p.ActorKind {
	case "":
		return ErrNoActorKind
	case ActorHuman, ActorAgent, ActorSystem, ActorIntegration:
	default:
		return ErrBadActorKind
	}
	switch p.Mode {
	case ModeManual, ModeAssisted, ModeAutomated:
	default:
		return ErrBadMode
	}
	if p.ActorKind == ActorHuman && p.ActorID == nil {
		return ErrHumanNoActor
	}
	if p.Mode == ModeAssisted && p.AuthorizedBy == nil && p.OnBehalfOf == nil {
		return ErrAssistedNoAuth
	}
	return nil
}

// Accountable is the person a change is attributed to in created_by_id /
// updated_by_id: the human actor, else whoever authorized it, else the person
// the agent acted for. Nil for unattended system/integration/agent work —
// those columns stay as they were (never a fake user id).
func (p Provenance) Accountable() *uuid.UUID {
	switch {
	case p.ActorKind == ActorHuman && p.ActorID != nil:
		return p.ActorID
	case p.AuthorizedBy != nil:
		return p.AuthorizedBy
	case p.OnBehalfOf != nil:
		return p.OnBehalfOf
	}
	return nil
}

// JSON encodes p (normalized). Empty string for a zero provenance.
func (p Provenance) JSON() string {
	if p.IsZero() {
		return ""
	}
	b, err := json.Marshal(p.Normalize())
	if err != nil {
		return ""
	}
	return string(b)
}

// Parse decodes a JSON provenance (as written by JSON). Zero on bad input.
func Parse(s string) Provenance {
	var p Provenance
	if strings.TrimSpace(s) == "" {
		return p
	}
	if err := json.Unmarshal([]byte(s), &p); err != nil {
		return Provenance{}
	}
	return p
}

// ---- constructors ------------------------------------------------------------

// Human is a person acting directly (manual).
func Human(userID uuid.UUID, via string) Provenance {
	id := userID
	return Provenance{ActorKind: ActorHuman, ActorID: &id, Via: via, Mode: ModeManual}
}

// Agent is the app's assistant acting without a person in the loop. Chain
// OnBehalf / Authorize / Ref to complete it.
func Agent(name, via string) Provenance {
	return Provenance{ActorKind: ActorAgent, ActorName: name, Via: via}
}

// System is an unattended platform process (seed, migration, job).
func System(via, origin string) Provenance {
	return Provenance{ActorKind: ActorSystem, Via: via, Origin: origin, Mode: ModeAutomated}
}

// Integration is an external source writing through an importer or webhook.
func Integration(name, via, origin string) Provenance {
	return Provenance{ActorKind: ActorIntegration, ActorName: name, Via: via, Origin: origin, Mode: ModeAutomated}
}

// Assisted is "person with agent": userID approved what the agent proposed.
func Assisted(agentName string, userID uuid.UUID, via string) Provenance {
	return Agent(agentName, via).OnBehalf(userID).Authorize(userID, time.Now().UTC())
}

// OnBehalf sets the person the agent acted for.
func (p Provenance) OnBehalf(userID uuid.UUID) Provenance {
	if userID == uuid.Nil {
		return p
	}
	id := userID
	p.OnBehalfOf = &id
	return p
}

// Authorize records the person who approved the proposal and when; the mode
// becomes assisted.
func (p Provenance) Authorize(userID uuid.UUID, at time.Time) Provenance {
	if userID == uuid.Nil {
		return p
	}
	id := userID
	t := at.UTC()
	p.AuthorizedBy, p.AuthorizedAt = &id, &t
	p.Proposed = false
	p.Mode = ModeAssisted
	return p
}

// Propose marks the output as waiting for a person.
func (p Provenance) Propose() Provenance {
	p.Proposed = true
	p.Mode = ModeAutomated
	p.AuthorizedBy, p.AuthorizedAt = nil, nil
	return p
}

// WithRef points the change to a session, tool call, proposal, run...
func (p Provenance) WithRef(kind, id string) Provenance {
	p.RefKind, p.RefID = kind, id
	return p
}

// WithSession sets the copilot conversation.
func (p Provenance) WithSession(id uuid.UUID) Provenance {
	if id == uuid.Nil {
		return p
	}
	s := id
	p.SessionID = &s
	return p
}

// WithOrigin sets the human-readable origin label.
func (p Provenance) WithOrigin(origin string) Provenance {
	p.Origin = origin
	return p
}

// WithSummary sets what the agent proposed.
func (p Provenance) WithSummary(s string) Provenance {
	p.Summary = s
	return p
}

func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
