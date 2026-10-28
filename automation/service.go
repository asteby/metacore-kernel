// Package automation is the kernel runtime for declarative automations and
// dynamic tools: "remind me tomorrow at 9", "every morning at 7 send me the
// security news of the northern zone", "tell me when a topic spikes in a branch".
//
// An automation is a JSON spec (automation/v1, validated by an embedded JSON
// Schema and semantically against the host's tool allowlist) with a trigger
// (cron with timezone, interval, one-shot, domain event) and a short list of
// whitelisted steps (tool, filter, condition, template, summarize, deliver).
// Steps are compiled to a linear runtime/flow DAG and executed by a flow
// Engine whose registry contains only automation nodes — there is no HTTP,
// SQL, shell or code node. The kernel outbox is the execution queue (leases,
// retries with backoff, dedupe, restart tolerance); the scheduler advances
// next_run_at in the same transaction that enqueues the run, under
// FOR UPDATE SKIP LOCKED, so several replicas never double-fire a slot.
//
// Everything the agent creates starts as a proposal that a person confirms
// (authorship "person with the assistant"), except trivial personal reminders
// when Policy.AutoApprovePersonalReminders is on. Runs act with the creator's
// identity, are budgeted (tokens/cost per run, daily org budget), cache model
// summaries by input hash, de-duplicate delivered items, and auto-pause after
// repeated failures.
package automation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/asteby/metacore-kernel/outbox"
)

// Outbox job kinds owned by this package.
const (
	JobRun   = "automation.run"
	JobEvent = "automation.event"
)

// Config wires the service.
type Config struct {
	DB         *gorm.DB
	Outbox     *outbox.Service // execution queue (required to schedule and run)
	Catalog    Catalog         // tool allowlist (required for tool steps)
	Deliverer  Deliverer       // notification channels (required for deliver steps)
	Summarizer Summarizer      // optional language model
	Auditor    Auditor         // optional
	Names      UserNames       // optional, for "persona con <asistente>" labels
	// Policy: start from DefaultPolicy() and override.
	Policy Policy
	// CanManage lets hosts allow admins to manage other people's automations.
	// Default: only the creator.
	CanManage func(ctx context.Context, id Identity, a *Automation) bool
	// Now overrides the clock (tests).
	Now         func() time.Time
	Logger      *slog.Logger
	SkipMigrate bool
	// RunMaxAttempts is the outbox attempt budget for a run (default 3).
	RunMaxAttempts int
	// ManagePath is the app route of the automations screen, used by the
	// cards' links (default "/automations").
	ManagePath string
}

// Service is the automation runtime.
type Service struct {
	db         *gorm.DB
	ob         *outbox.Service
	catalog    Catalog
	deliverer  Deliverer
	summarizer Summarizer
	auditor    Auditor
	names      UserNames
	policy     Policy
	canManage  func(ctx context.Context, id Identity, a *Automation) bool
	now        func() time.Time
	log        *slog.Logger
	maxAtt     int
	engine     *runEngine
	managePath string
}

// New builds the service, migrates its tables (unless SkipMigrate) and
// registers its outbox handlers. The host starts the outbox workers and,
// optionally, the scheduler (StartScheduler).
func New(cfg Config) (*Service, error) {
	if cfg.DB == nil {
		return nil, errors.New("automation: DB is required")
	}
	s := &Service{
		db:         cfg.DB,
		ob:         cfg.Outbox,
		catalog:    cfg.Catalog,
		deliverer:  cfg.Deliverer,
		summarizer: cfg.Summarizer,
		auditor:    cfg.Auditor,
		names:      cfg.Names,
		policy:     cfg.Policy.withDefaults(),
		canManage:  cfg.CanManage,
		now:        cfg.Now,
		log:        cfg.Logger,
		maxAtt:     cfg.RunMaxAttempts,
		managePath: cfg.ManagePath,
	}
	if s.managePath == "" {
		s.managePath = "/automations"
	}
	if s.now == nil {
		s.now = time.Now
	}
	if s.log == nil {
		s.log = slog.Default()
	}
	if s.maxAtt <= 0 {
		s.maxAtt = 3
	}
	if s.canManage == nil {
		s.canManage = func(_ context.Context, id Identity, a *Automation) bool { return a.CreatedBy == id.UserID }
	}
	if !cfg.SkipMigrate {
		if err := Migrate(context.Background(), cfg.DB); err != nil {
			return nil, err
		}
	}
	s.engine = newRunEngine(s)
	if s.ob != nil {
		s.ob.Register(JobRun, s.handleRunJob)
		s.ob.Register(JobEvent, s.handleEventJob)
	}
	return s, nil
}

// Policy returns the effective policy.
func (s *Service) Policy() Policy { return s.policy }

// ---- create / update -------------------------------------------------------

// CreateInput is a request to create an automation or dynamic tool.
type CreateInput struct {
	Kind string          // automation (default) | tool
	Spec json.RawMessage // automation/v1 JSON
	// Origin: chat (the agent proposed it), ui or api (a person wrote it).
	Origin string
	// Assisted marks agent assistance; together with Origin=chat it forces a
	// proposal unless the spec is a trivial personal reminder.
	Assisted bool
	// Propose forces the proposal state even for a person (UI "save as draft").
	Propose   bool
	SessionID *uuid.UUID
}

// Create validates and stores a new automation.
func (s *Service) Create(ctx context.Context, id Identity, in CreateInput) (*Automation, error) {
	if !s.policy.Enabled {
		return nil, ErrDisabled
	}
	if id.OrganizationID == uuid.Nil || id.UserID == uuid.Nil {
		return nil, ErrForbidden
	}
	kind := in.Kind
	if kind == "" {
		kind = KindAutomation
	}
	spec, raw, err := s.parseAndValidate(ctx, kind, in.Spec, id)
	if err != nil {
		return nil, err
	}
	var live int64
	if err := s.db.WithContext(ctx).Model(&Automation{}).
		Where("organization_id = ? AND created_by = ? AND status <> ?", id.OrganizationID, id.UserID, StatusArchived).
		Count(&live).Error; err != nil {
		return nil, err
	}
	if int(live) >= s.policy.MaxPerUser {
		return nil, fmt.Errorf("%w (%d por persona)", ErrLimit, s.policy.MaxPerUser)
	}
	origin := in.Origin
	if origin == "" {
		origin = OriginUI
	}
	now := s.now().UTC()
	a := &Automation{
		ID:             uuid.New(),
		OrganizationID: id.OrganizationID,
		Kind:           kind,
		Name:           strings.TrimSpace(spec.Name),
		Description:    strings.TrimSpace(spec.Description),
		Spec:           string(raw),
		SpecVersion:    SpecVersion,
		SpecHash:       specHash(raw),
		CreatedBy:      id.UserID,
		Origin:         origin,
		SessionID:      in.SessionID,
		Version:        1,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	if spec.Trigger != nil {
		a.TriggerType = spec.Trigger.Type
		a.EventName = spec.Trigger.Event
	}
	if in.Assisted || origin == OriginChat {
		a.AssistedBy = s.policy.AssistantName
	}
	if kind == KindTool {
		name := ToolNameFor(spec.Name)
		a.ToolName = &name
	}
	proposal := in.Propose || origin == OriginChat || in.Assisted
	if proposal && origin == OriginChat && s.policy.AutoApprovePersonalReminders && kind == KindAutomation && spec.IsPersonalReminder() {
		// The person asked the assistant for their own reminder: confirming it
		// again would be noise. Authorship stays "person with the assistant".
		proposal = false
	}
	if proposal {
		a.Status = StatusProposed
	} else {
		a.Status = StatusActive
		a.AuthorizedBy = &id.UserID
		a.AuthorizedAt = &now
		if err := s.schedule(a, spec, now); err != nil {
			return nil, err
		}
	}
	if err := s.db.WithContext(ctx).Create(a).Error; err != nil {
		if isUnique(err) {
			return nil, invalid("ya existe una herramienta llamada %q", spec.Name)
		}
		return nil, err
	}
	action := "created"
	if proposal {
		action = "proposed"
	}
	s.audit(ctx, a, id.UserID, action, describeTrigger(spec.Trigger, s.policy.DefaultTimezone, s.policy.EventLabels))
	return a, nil
}

// UpdateInput changes an automation.
type UpdateInput struct {
	Spec            json.RawMessage
	ExpectedVersion int  // optimistic concurrency (0 = skip check)
	Assisted        bool // the agent edited it: an active automation goes back to proposal
}

// Update replaces the spec.
func (s *Service) Update(ctx context.Context, id Identity, autoID uuid.UUID, in UpdateInput) (*Automation, error) {
	if !s.policy.Enabled {
		return nil, ErrDisabled
	}
	a, err := s.load(ctx, id, autoID)
	if err != nil {
		return nil, err
	}
	if a.Status == StatusArchived {
		return nil, ErrBadState
	}
	if in.ExpectedVersion > 0 && in.ExpectedVersion != a.Version {
		return nil, ErrConflict
	}
	spec, raw, err := s.parseAndValidate(ctx, a.Kind, in.Spec, Identity{OrganizationID: a.OrganizationID, UserID: a.CreatedBy})
	if err != nil {
		return nil, err
	}
	now := s.now().UTC()
	updates := map[string]any{
		"name": strings.TrimSpace(spec.Name), "description": strings.TrimSpace(spec.Description),
		"spec": string(raw), "spec_hash": specHash(raw), "updated_at": now, "version": a.Version + 1,
		"trigger_type": "", "event_name": "",
	}
	if spec.Trigger != nil {
		updates["trigger_type"] = spec.Trigger.Type
		updates["event_name"] = spec.Trigger.Event
	}
	if a.Kind == KindTool {
		updates["tool_name"] = ToolNameFor(spec.Name)
	}
	status := a.Status
	if in.Assisted {
		updates["assisted_by"] = s.policy.AssistantName
		if a.Status != StatusProposed {
			status = StatusProposed
			updates["authorized_by"] = nil
			updates["authorized_at"] = nil
			updates["next_run_at"] = nil
		}
	}
	if status == StatusActive || status == StatusError || status == StatusCompleted {
		// A person edited it: keep it running with the new schedule.
		status = StatusActive
		tmp := *a
		if err := s.schedule(&tmp, spec, now); err != nil {
			return nil, err
		}
		updates["next_run_at"] = tmp.NextRunAt
		updates["consecutive_failures"] = 0
		updates["status_reason"] = ""
	}
	updates["status"] = status
	res := s.db.WithContext(ctx).Model(&Automation{}).Where("id = ? AND version = ?", a.ID, a.Version).Updates(updates)
	if res.Error != nil {
		if isUnique(res.Error) {
			return nil, invalid("ya existe una herramienta llamada %q", spec.Name)
		}
		return nil, res.Error
	}
	if res.RowsAffected == 0 {
		return nil, ErrConflict
	}
	s.audit(ctx, a, id.UserID, "updated", "")
	return s.load(ctx, id, autoID)
}

// Confirm activates a proposal. Only a person who can manage it (by default
// its creator, i.e. the person who asked the assistant) can confirm.
func (s *Service) Confirm(ctx context.Context, id Identity, autoID uuid.UUID) (*Automation, error) {
	if !s.policy.Enabled {
		return nil, ErrDisabled
	}
	a, err := s.load(ctx, id, autoID)
	if err != nil {
		return nil, err
	}
	if a.Status == StatusActive {
		return a, nil // double click
	}
	if a.Status != StatusProposed {
		return nil, ErrBadState
	}
	// Re-validate: the catalog, permissions or the clock may have changed.
	if _, _, err := s.parseAndValidate(ctx, a.Kind, json.RawMessage(a.Spec), Identity{OrganizationID: a.OrganizationID, UserID: a.CreatedBy}); err != nil {
		return nil, err
	}
	spec, _ := a.ParsedSpec()
	now := s.now().UTC()
	tmp := *a
	if err := s.schedule(&tmp, spec, now); err != nil {
		return nil, err
	}
	res := s.db.WithContext(ctx).Model(&Automation{}).Where("id = ? AND status = ?", a.ID, StatusProposed).Updates(map[string]any{
		"status": StatusActive, "authorized_by": id.UserID, "authorized_at": now, "next_run_at": tmp.NextRunAt,
		"updated_at": now, "version": a.Version + 1, "status_reason": "",
	})
	if res.Error != nil {
		return nil, res.Error
	}
	if res.RowsAffected == 0 {
		return s.load(ctx, id, autoID)
	}
	s.audit(ctx, a, id.UserID, "confirmed", "")
	return s.load(ctx, id, autoID)
}

// Pause stops an active automation.
func (s *Service) Pause(ctx context.Context, id Identity, autoID uuid.UUID) (*Automation, error) {
	return s.transition(ctx, id, autoID, "paused", []string{StatusActive, StatusError}, map[string]any{"status": StatusPaused, "next_run_at": nil})
}

// Resume re-activates a paused or errored automation (resetting failures).
func (s *Service) Resume(ctx context.Context, id Identity, autoID uuid.UUID) (*Automation, error) {
	a, err := s.load(ctx, id, autoID)
	if err != nil {
		return nil, err
	}
	if a.Status != StatusPaused && a.Status != StatusError {
		if a.Status == StatusActive {
			return a, nil
		}
		return nil, ErrBadState
	}
	spec, err := a.ParsedSpec()
	if err != nil {
		return nil, err
	}
	if err := s.validateSpec(ctx, a.Kind, spec, Identity{OrganizationID: a.OrganizationID, UserID: a.CreatedBy}, s.now().UTC()); err != nil {
		return nil, err
	}
	tmp := *a
	if err := s.schedule(&tmp, spec, s.now().UTC()); err != nil {
		return nil, err
	}
	return s.transition(ctx, id, autoID, "resumed", []string{StatusPaused, StatusError}, map[string]any{
		"status": StatusActive, "next_run_at": tmp.NextRunAt, "consecutive_failures": 0, "status_reason": "",
	})
}

// Archive deletes (soft) an automation; it stays for audit.
func (s *Service) Archive(ctx context.Context, id Identity, autoID uuid.UUID) (*Automation, error) {
	return s.transition(ctx, id, autoID, "archived",
		[]string{StatusProposed, StatusActive, StatusPaused, StatusError, StatusCompleted},
		map[string]any{"status": StatusArchived, "next_run_at": nil})
}

func (s *Service) transition(ctx context.Context, id Identity, autoID uuid.UUID, action string, from []string, updates map[string]any) (*Automation, error) {
	a, err := s.load(ctx, id, autoID)
	if err != nil {
		return nil, err
	}
	ok := false
	for _, f := range from {
		if a.Status == f {
			ok = true
		}
	}
	if !ok {
		if st, _ := updates["status"].(string); st == a.Status {
			return a, nil
		}
		return nil, ErrBadState
	}
	updates["updated_at"] = s.now().UTC()
	updates["version"] = a.Version + 1
	res := s.db.WithContext(ctx).Model(&Automation{}).Where("id = ? AND version = ?", a.ID, a.Version).Updates(updates)
	if res.Error != nil {
		return nil, res.Error
	}
	if res.RowsAffected == 0 {
		return nil, ErrConflict
	}
	s.audit(ctx, a, id.UserID, action, "")
	return s.load(ctx, id, autoID)
}

// ---- read ------------------------------------------------------------------

// ListFilter narrows List.
type ListFilter struct {
	Kind   string
	Status string
	// All lists every automation of the org the identity may manage (admins);
	// otherwise only the person's own.
	All bool
}

// List returns the identity's automations (newest first, archived excluded
// unless asked).
func (s *Service) List(ctx context.Context, id Identity, f ListFilter) ([]Automation, error) {
	q := s.db.WithContext(ctx).Where("organization_id = ?", id.OrganizationID)
	if !f.All {
		q = q.Where("created_by = ?", id.UserID)
	}
	if f.Kind != "" {
		q = q.Where("kind = ?", f.Kind)
	}
	if f.Status != "" {
		q = q.Where("status = ?", f.Status)
	} else {
		q = q.Where("status <> ?", StatusArchived)
	}
	var out []Automation
	if err := q.Order("created_at DESC").Limit(200).Find(&out).Error; err != nil {
		return nil, err
	}
	if f.All {
		kept := out[:0]
		for i := range out {
			if s.canManage(ctx, id, &out[i]) {
				kept = append(kept, out[i])
			}
		}
		out = kept
	}
	return out, nil
}

// Get returns one automation the identity may manage.
func (s *Service) Get(ctx context.Context, id Identity, autoID uuid.UUID) (*Automation, error) {
	return s.load(ctx, id, autoID)
}

// Runs lists the latest runs of an automation.
func (s *Service) Runs(ctx context.Context, id Identity, autoID uuid.UUID, limit int) ([]Run, error) {
	if _, err := s.load(ctx, id, autoID); err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 100 {
		limit = 30
	}
	var out []Run
	err := s.db.WithContext(ctx).Where("automation_id = ?", autoID).Order("started_at DESC").Limit(limit).Find(&out).Error
	return out, err
}

// CatalogFor returns the tools the identity may compose.
func (s *Service) CatalogFor(ctx context.Context, id Identity) []CatalogTool {
	if s.catalog == nil {
		return nil
	}
	return s.catalog.List(ctx, id)
}

func (s *Service) load(ctx context.Context, id Identity, autoID uuid.UUID) (*Automation, error) {
	var a Automation
	err := s.db.WithContext(ctx).Where("id = ? AND organization_id = ?", autoID, id.OrganizationID).First(&a).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if !s.canManage(ctx, id, &a) {
		return nil, ErrNotFound // do not leak existence
	}
	return &a, nil
}

// ---- helpers ---------------------------------------------------------------

func (s *Service) parseAndValidate(ctx context.Context, kind string, raw json.RawMessage, id Identity) (*Spec, []byte, error) {
	spec, err := ParseSpec(raw)
	if err != nil {
		return nil, nil, err
	}
	if err := s.validateSpec(ctx, kind, spec, id, s.now().UTC()); err != nil {
		return nil, nil, err
	}
	canon := spec.JSON()
	return spec, canon, nil
}

// schedule sets NextRunAt for an active automation.
func (s *Service) schedule(a *Automation, spec *Spec, now time.Time) error {
	a.NextRunAt = nil
	if a.Kind != KindAutomation || spec.Trigger == nil {
		return nil
	}
	next, ok, err := NextRun(spec.Trigger, now, s.policy.DefaultTimezone)
	if err != nil {
		return err
	}
	if spec.Trigger.Type == TriggerOnce && !ok {
		return invalid("la fecha del recordatorio ya pasó")
	}
	if ok {
		n := next.UTC()
		a.NextRunAt = &n
	}
	return nil
}

func (s *Service) audit(ctx context.Context, a *Automation, actor uuid.UUID, action, detail string) {
	s.log.Info("automation", "action", action, "automation", a.ID, "org", a.OrganizationID, "actor", actor, "assisted_by", a.AssistedBy)
	if s.auditor == nil {
		return
	}
	s.auditor.Record(ctx, AuditEvent{
		OrganizationID: a.OrganizationID, AutomationID: a.ID, ActorID: actor, AssistedBy: a.AssistedBy,
		Action: action, Detail: detail, At: s.now().UTC(),
	})
}

var toolSlugRe = regexp.MustCompile(`[^a-z0-9]+`)

// ToolNameFor derives the agent-facing name of a dynamic tool ("dyn_…").
func ToolNameFor(name string) string {
	n := strings.ToLower(strings.TrimSpace(name))
	n = strings.NewReplacer("á", "a", "é", "e", "í", "i", "ó", "o", "ú", "u", "ü", "u", "ñ", "n").Replace(n)
	n = strings.Trim(toolSlugRe.ReplaceAllString(n, "_"), "_")
	if n == "" {
		n = "herramienta"
	}
	if len(n) > 50 {
		n = n[:50]
	}
	return "dyn_" + n
}

func isUnique(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "unique") || strings.Contains(msg, "duplicate key") || strings.Contains(msg, "23505")
}

// onConflictNothing is shared by idempotent inserts.
var onConflictNothing = clause.OnConflict{DoNothing: true}
