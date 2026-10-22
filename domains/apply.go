package domains

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Zone is the DNS zone a provider manages for a domain.
type Zone struct {
	ID string `json:"id"`
	// Name is the zone apex ("example.com"); may be a parent of the domain.
	Name string `json:"name"`
}

// ProviderRecord is a record as the provider stores it.
type ProviderRecord struct {
	ID       string     `json:"id"`
	Type     RecordType `json:"type"`
	Name     string     `json:"name"`
	Value    string     `json:"value"`
	Priority int        `json:"priority,omitempty"`
	TTL      int        `json:"ttl,omitempty"`
}

// Record converts to the package's Record (for canonical comparison).
func (p ProviderRecord) Record() Record {
	return Record{Type: p.Type, Name: strings.ToLower(strings.TrimSuffix(p.Name, ".")), Value: p.Value, Priority: p.Priority, TTL: p.TTL}
}

// Canonical is the comparable value of the stored record.
func (p ProviderRecord) Canonical() string { return p.Record().Canonical() }

// ZoneProvider is the minimal write/read surface a DNS provider must offer. It
// never deletes: the applier is upsert-only by construction.
type ZoneProvider interface {
	Name() string
	// FindZone locates the zone that contains host (the most specific one the
	// credential can see). Returns ErrZoneNotFound / ErrProviderAuth.
	FindZone(ctx context.Context, host string) (Zone, error)
	// ListRecords returns every record at the exact owner name.
	ListRecords(ctx context.Context, z Zone, name string) ([]ProviderRecord, error)
	CreateRecord(ctx context.Context, z Zone, r Record) (ProviderRecord, error)
	UpdateRecord(ctx context.Context, z Zone, id string, r Record) (ProviderRecord, error)
}

// Provider errors.
var (
	// ErrZoneNotFound: the credential sees no zone containing the host.
	ErrZoneNotFound = errors.New("domains: zone not found for this credential")
	// ErrProviderAuth: the credential is invalid or lacks permissions
	// (needs Zone:Read and DNS:Edit on Cloudflare).
	ErrProviderAuth = errors.New("domains: provider rejected the credential")
	// ErrRateLimited: the provider asked us to slow down.
	ErrRateLimited = errors.New("domains: provider rate limit")
	// ErrPlanBlocked: the plan contains entries that cannot be applied.
	ErrPlanBlocked = errors.New("domains: plan has blocked entries")
	// ErrPlanChanged: the live plan differs from the one the user confirmed.
	ErrPlanChanged = errors.New("domains: plan changed since it was confirmed")
)

// Action of a plan entry.
type Action string

const (
	ActionCreate  Action = "create"
	ActionUpdate  Action = "update"
	ActionNoop    Action = "noop"
	ActionBlocked Action = "blocked"
)

// Plan diagnostics codes.
const (
	DiagReplacesExisting = "replaces_existing"
	DiagExtraExisting    = "extra_existing_values"
	DiagMXOtherTarget    = "mx_other_target"
	DiagCNAMEConflict    = "cname_conflict"
)

// PlanEntry is one record's diff.
type PlanEntry struct {
	Action Action `json:"action"`
	// Record is the desired state.
	Record Record `json:"record"`
	// Existing is the record an update replaces, or the equal record a noop
	// found.
	Existing *ProviderRecord `json:"existing,omitempty"`
	// Warnings the user must see before confirming (replaces_existing,
	// mx_other_target...). A blocked entry carries its reason here.
	Warnings []Diagnostic `json:"warnings,omitempty"`
}

// Plan is the read-only diff between a RecordSet and a provider zone.
type Plan struct {
	Provider string      `json:"provider"`
	Zone     Zone        `json:"zone"`
	Domain   string      `json:"domain"`
	Entries  []PlanEntry `json:"entries"`
}

// Counts summarizes the plan by action.
func (p Plan) Counts() map[Action]int {
	out := map[Action]int{}
	for _, e := range p.Entries {
		out[e.Action]++
	}
	return out
}

// HasBlocked reports whether any entry cannot be applied.
func (p Plan) HasBlocked() bool { return p.Counts()[ActionBlocked] > 0 }

// Changes reports whether applying would write anything.
func (p Plan) Changes() bool {
	c := p.Counts()
	return c[ActionCreate]+c[ActionUpdate] > 0
}

// Fingerprint is a stable digest of what applying this plan would do. The UI
// shows the diff, the user confirms the fingerprint, and ApplyConfirmed refuses
// to write when the live plan no longer hashes to it (the zone changed while
// the dialog was open).
func (p Plan) Fingerprint() string {
	lines := make([]string, 0, len(p.Entries))
	for _, e := range p.Entries {
		ex := ""
		if e.Existing != nil {
			ex = e.Existing.ID + "=" + e.Existing.Canonical()
		}
		lines = append(lines, strings.Join([]string{string(e.Action), e.Record.Key(), e.Record.Canonical(), ex}, "\x1f"))
	}
	sort.Strings(lines)
	h := sha256.New()
	fmt.Fprintf(h, "%s\x1e%s\x1e%s\x1e", p.Provider, p.Zone.ID, p.Domain)
	h.Write([]byte(strings.Join(lines, "\x1e")))
	return hex.EncodeToString(h.Sum(nil))
}

// AppliedEntry is the outcome of one write.
type AppliedEntry struct {
	Record     Record `json:"record"`
	Action     Action `json:"action"`
	ProviderID string `json:"provider_id,omitempty"`
	Error      string `json:"error,omitempty"`
}

// ApplyResult is what Apply did.
type ApplyResult struct {
	Created   int            `json:"created"`
	Updated   int            `json:"updated"`
	Unchanged int            `json:"unchanged"`
	Failed    int            `json:"failed"`
	Entries   []AppliedEntry `json:"entries"`
	AppliedAt time.Time      `json:"applied_at"`
}

// DNSApplier writes a RecordSet through a DNS provider in two phases so the
// tenant approves exactly what will change.
//
//	Plan  read-only: finds the zone, lists what exists, returns the diff
//	Apply executes create/update entries; idempotent (re-running a plan
//	      that already took effect writes nothing); never deletes
type DNSApplier interface {
	Name() string
	Plan(ctx context.Context, set RecordSet) (Plan, error)
	Apply(ctx context.Context, plan Plan) (ApplyResult, error)
}

// ApplyConfirmed re-plans and applies only when the live plan still hashes to
// the fingerprint the user confirmed. On mismatch it returns the fresh plan
// with ErrPlanChanged so the UI can show the new diff.
func ApplyConfirmed(ctx context.Context, a DNSApplier, set RecordSet, confirmed string) (ApplyResult, Plan, error) {
	plan, err := a.Plan(ctx, set)
	if err != nil {
		return ApplyResult{}, plan, err
	}
	if confirmed == "" || plan.Fingerprint() != confirmed {
		return ApplyResult{}, plan, ErrPlanChanged
	}
	res, err := a.Apply(ctx, plan)
	return res, plan, err
}

// ProviderApplier is the DNSApplier over any ZoneProvider.
type ProviderApplier struct{ P ZoneProvider }

// NewProviderApplier wraps a provider.
func NewProviderApplier(p ZoneProvider) *ProviderApplier { return &ProviderApplier{P: p} }

func (a *ProviderApplier) Name() string { return a.P.Name() }

// Plan implements DNSApplier.
func (a *ProviderApplier) Plan(ctx context.Context, set RecordSet) (Plan, error) {
	if err := set.Validate(); err != nil {
		return Plan{}, err
	}
	zone, err := a.P.FindZone(ctx, set.Domain)
	if err != nil {
		return Plan{}, err
	}
	plan := Plan{Provider: a.P.Name(), Zone: zone, Domain: set.Domain}
	existingAt := map[string][]ProviderRecord{}
	list := func(name string) ([]ProviderRecord, error) {
		if v, ok := existingAt[name]; ok {
			return v, nil
		}
		v, err := a.P.ListRecords(ctx, zone, name)
		if err != nil {
			return nil, err
		}
		existingAt[name] = v
		return v, nil
	}
	for _, rec := range set.Records {
		all, err := list(rec.Name)
		if err != nil {
			return Plan{}, err
		}
		plan.Entries = append(plan.Entries, planRecord(rec, all))
	}
	return plan, nil
}

func planRecord(rec Record, atName []ProviderRecord) PlanEntry {
	var same []ProviderRecord
	cnameAt, otherAt := false, false
	unique := rec.Unique || rec.Type == TypeCNAME // a name holds one CNAME
	for _, p := range atName {
		switch {
		case p.Type == rec.Type:
			same = append(same, p)
		case p.Type == TypeCNAME:
			cnameAt = true
		default:
			otherAt = true
		}
	}
	want := rec.Canonical()
	for i := range same {
		if valueSatisfies(rec, want, same[i].Canonical()) || same[i].Canonical() == want {
			e := PlanEntry{Action: ActionNoop, Record: rec, Existing: &same[i]}
			if unique && len(same) > 1 {
				e.Warnings = append(e.Warnings, diag(DiagExtraExisting, SeverityWarn, map[string]any{"name": rec.Name, "count": len(same) - 1}, ""))
			}
			return e
		}
	}
	if rec.Type == TypeCNAME && otherAt {
		return blocked(rec, "another record type already exists at this name; a CNAME must be alone")
	}
	if rec.Type != TypeCNAME && cnameAt {
		return blocked(rec, "a CNAME already exists at this name; it cannot coexist with "+string(rec.Type))
	}
	if unique && len(same) > 0 {
		e := PlanEntry{Action: ActionUpdate, Record: rec, Existing: &same[0]}
		e.Warnings = append(e.Warnings, diag(DiagReplacesExisting, SeverityWarn,
			map[string]any{"name": rec.Name, "type": string(rec.Type), "from": same[0].Canonical(), "to": want}, ""))
		if len(same) > 1 {
			e.Warnings = append(e.Warnings, diag(DiagExtraExisting, SeverityWarn, map[string]any{"name": rec.Name, "count": len(same) - 1}, ""))
		}
		return e
	}
	e := PlanEntry{Action: ActionCreate, Record: rec}
	if rec.Type == TypeMX && len(same) > 0 {
		e.Warnings = append(e.Warnings, diag(DiagMXOtherTarget, SeverityWarn,
			map[string]any{"name": rec.Name, "existing": same[0].Canonical()}, "existing MX records stay: mail keeps flowing to them until the tenant removes them"))
	}
	return e
}

func blocked(rec Record, why string) PlanEntry {
	return PlanEntry{Action: ActionBlocked, Record: rec, Warnings: []Diagnostic{diag(DiagCNAMEConflict, SeverityError, map[string]any{"name": rec.Name}, why)}}
}

// Apply implements DNSApplier. It refuses a plan with blocked entries, skips
// noops, and before each create re-checks the live slot so running the same
// plan twice (or two operators at once) never produces duplicates.
func (a *ProviderApplier) Apply(ctx context.Context, plan Plan) (ApplyResult, error) {
	res := ApplyResult{AppliedAt: time.Now().UTC()}
	if plan.HasBlocked() {
		return res, ErrPlanBlocked
	}
	var errs []error
	for _, e := range plan.Entries {
		ae := AppliedEntry{Record: e.Record, Action: e.Action}
		switch e.Action {
		case ActionNoop:
			res.Unchanged++
			if e.Existing != nil {
				ae.ProviderID = e.Existing.ID
			}
		case ActionCreate:
			cur, err := a.P.ListRecords(ctx, plan.Zone, e.Record.Name)
			if err == nil {
				if hit := findSame(e.Record, cur); hit != nil {
					ae.Action, ae.ProviderID = ActionNoop, hit.ID
					res.Unchanged++
					break
				}
			}
			if err != nil {
				ae.Error = err.Error()
				res.Failed++
				errs = append(errs, fmt.Errorf("%s: %w", e.Record, err))
				break
			}
			pr, err := a.P.CreateRecord(ctx, plan.Zone, e.Record)
			if err != nil {
				ae.Error = err.Error()
				res.Failed++
				errs = append(errs, fmt.Errorf("create %s: %w", e.Record, err))
				break
			}
			ae.ProviderID = pr.ID
			res.Created++
		case ActionUpdate:
			if e.Existing == nil {
				ae.Error = "update without existing record"
				res.Failed++
				errs = append(errs, fmt.Errorf("update %s: %s", e.Record, ae.Error))
				break
			}
			pr, err := a.P.UpdateRecord(ctx, plan.Zone, e.Existing.ID, e.Record)
			if err != nil {
				ae.Error = err.Error()
				res.Failed++
				errs = append(errs, fmt.Errorf("update %s: %w", e.Record, err))
				break
			}
			ae.ProviderID = pr.ID
			res.Updated++
		default:
			ae.Error = "unknown action"
			res.Failed++
		}
		res.Entries = append(res.Entries, ae)
		if ctx.Err() != nil {
			errs = append(errs, ctx.Err())
			break
		}
	}
	return res, errors.Join(errs...)
}

func findSame(rec Record, cur []ProviderRecord) *ProviderRecord {
	want := rec.Canonical()
	for i := range cur {
		if cur[i].Type == rec.Type && (cur[i].Canonical() == want || valueSatisfies(rec, want, cur[i].Canonical())) {
			return &cur[i]
		}
	}
	return nil
}
