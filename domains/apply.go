package domains

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
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
	// Comment, Tags and Proxied are provider-side metadata the applier never
	// writes on update: they are preserved, shown in the plan and covered by
	// its fingerprint. Proxied is nil when the provider has no such notion.
	Comment string   `json:"comment,omitempty"`
	Tags    []string `json:"tags,omitempty"`
	Proxied *bool    `json:"proxied,omitempty"`
}

// sameStored reports whether two stored records are identical in everything
// the applier reasons about (value, TTL and the preserved metadata).
func (p ProviderRecord) sameStored(o ProviderRecord) bool {
	if p.ID != o.ID || p.Type != o.Type || strings.ToLower(strings.TrimSuffix(p.Name, ".")) != strings.ToLower(strings.TrimSuffix(o.Name, ".")) || p.Canonical() != o.Canonical() || p.TTL != o.TTL || p.Comment != o.Comment ||
		strings.Join(p.Tags, "\x1f") != strings.Join(o.Tags, "\x1f") {
		return false
	}
	return (p.Proxied == nil) == (o.Proxied == nil) && (p.Proxied == nil || *p.Proxied == *o.Proxied)
}

func (p ProviderRecord) fingerprint() string {
	px := "-"
	if p.Proxied != nil {
		px = fmt.Sprint(*p.Proxied)
	}
	return strings.Join([]string{p.ID, p.Canonical(), fmt.Sprint(p.TTL), p.Comment, strings.Join(p.Tags, ","), px}, "\x1d")
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
	// ErrInvalidPlan: the plan handed to Apply is not one Plan could have
	// produced for this provider (forged or corrupted: names outside the
	// domain, an update whose existing record is at another name, ...).
	ErrInvalidPlan = errors.New("domains: invalid plan")
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
	// DiagMergesSPF: an existing SPF record is kept and the required
	// mechanism is merged into it (nothing the tenant had is dropped).
	DiagMergesSPF = "merges_spf"
	// DiagProxiedExisting: the record being updated is proxied by the
	// provider; the update preserves that, but mail/auth hosts must be
	// DNS-only.
	DiagProxiedExisting = "proxied_existing"
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
			ex = e.Existing.fingerprint()
		}
		r := e.Record
		// Everything that changes what gets written (or how it is matched)
		// is part of what the user confirms.
		meta := fmt.Sprintf("ttl=%d unique=%t match=%s needle=%s", r.TTL, r.Unique, r.Match, r.Needle)
		lines = append(lines, strings.Join([]string{string(e.Action), r.Key(), r.Canonical(), meta, ex}, "\x1f"))
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
//
// Hosts should call ApplyConfirmed, the only path that ties the write to what
// the user confirmed. ProviderApplier.Apply still defends itself: it rejects a
// plan that does not belong to this provider/domain and re-verifies every
// update against the live record before writing.
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

// txtFamily classifies a TXT payload by the protocol it belongs to, so a
// unique record only ever replaces its own kind: an SPF never overwrites a
// google-site-verification token living at the same name.
func txtFamily(name, value string) string {
	l := strings.ToLower(canonTXT(value))
	switch {
	case strings.HasPrefix(l, "v=spf1"):
		return "spf"
	case strings.HasPrefix(l, "v=dmarc1"):
		return "dmarc"
	case strings.HasPrefix(l, "v=dkim1"), strings.Contains(name, "._domainkey."):
		return "dkim"
	}
	return ""
}

var spfAllRe = regexp.MustCompile(`^[+\-~?]?all$`)

// mergeSPF inserts the mechanisms of needle into an existing SPF record,
// before its terminating "all"/"redirect=" and without dropping anything the
// tenant already authorises. Mechanisms already present are not duplicated.
func mergeSPF(existing, needle string) string {
	fields := strings.Fields(canonTXT(existing))
	have := map[string]struct{}{}
	for _, f := range fields {
		have[strings.ToLower(f)] = struct{}{}
	}
	var add []string
	for _, n := range strings.Fields(canonTXT(needle)) {
		// Only mechanisms are merged: the version tag, a terminal "all" and
		// "redirect=" belong to the tenant's record (adding them would yield
		// "... ~all -all").
		if ln := strings.ToLower(n); ln == "v=spf1" || spfAllRe.MatchString(ln) || strings.HasPrefix(ln, "redirect=") {
			continue
		}
		if _, ok := have[strings.ToLower(n)]; !ok {
			add = append(add, n)
		}
	}
	if len(add) == 0 {
		return strings.Join(fields, " ")
	}
	at := len(fields)
	for i, f := range fields {
		lf := strings.ToLower(f)
		if i > 0 && (spfAllRe.MatchString(lf) || strings.HasPrefix(lf, "redirect=")) {
			at = i
			break
		}
	}
	out := append([]string{}, fields[:at]...)
	out = append(out, add...)
	out = append(out, fields[at:]...)
	return strings.Join(out, " ")
}

func planRecord(rec Record, atName []ProviderRecord) PlanEntry {
	var same []ProviderRecord
	cnameAt, otherAt := false, false
	family := ""
	if rec.Type == TypeTXT {
		family = txtFamily(rec.Name, rec.Value)
	}
	// A TXT without a recognised family has no safe notion of "the one value
	// to replace": it is only ever created (or found equal), never updated.
	unique := (rec.Unique && (rec.Type != TypeTXT || family != "")) || rec.Type == TypeCNAME
	for _, p := range atName {
		switch {
		case p.Type == rec.Type:
			if family != "" && txtFamily(p.Name, p.Value) != family {
				continue // someone else's TXT (site verification, ...): untouched
			}
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
		desired := rec
		e := PlanEntry{Action: ActionUpdate, Existing: &same[0]}
		if family == "spf" && rec.Match == MatchContains && rec.Needle != "" {
			// Keep the tenant's SPF and merge our mechanism into it.
			desired.Value = mergeSPF(same[0].Value, rec.Needle)
			e.Warnings = append(e.Warnings, diag(DiagMergesSPF, SeverityWarn,
				map[string]any{"name": rec.Name, "from": same[0].Canonical(), "to": desired.Canonical()}, ""))
		} else {
			e.Warnings = append(e.Warnings, diag(DiagReplacesExisting, SeverityWarn,
				map[string]any{"name": rec.Name, "type": string(rec.Type), "from": same[0].Canonical(), "to": want}, ""))
		}
		e.Record = desired
		if len(same) > 1 {
			e.Warnings = append(e.Warnings, diag(DiagExtraExisting, SeverityWarn, map[string]any{"name": rec.Name, "count": len(same) - 1}, ""))
		}
		if same[0].Proxied != nil && *same[0].Proxied {
			e.Warnings = append(e.Warnings, diag(DiagProxiedExisting, SeverityWarn, map[string]any{"name": rec.Name}, "the existing record is proxied and stays proxied"))
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
	if err := a.validatePlan(ctx, plan); err != nil {
		return res, err
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
				// Re-plan this slot against the live zone: if something
				// appeared since the plan (a second SPF for a Unique record, a
				// conflicting CNAME) creating would duplicate or break it.
				if again := planRecord(e.Record, cur); again.Action != ActionCreate {
					ae.Error = ErrPlanChanged.Error()
					res.Failed++
					errs = append(errs, fmt.Errorf("create %s: %w", e.Record, ErrPlanChanged))
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
			// Re-verify against the live zone: the record must still be the
			// one the user saw (value, TTL and preserved metadata), otherwise
			// the write could clobber a change made in between.
			cur, err := a.P.ListRecords(ctx, plan.Zone, e.Record.Name)
			if err != nil {
				ae.Error = err.Error()
				res.Failed++
				errs = append(errs, fmt.Errorf("update %s: %w", e.Record, err))
				break
			}
			var live *ProviderRecord
			for i := range cur {
				if cur[i].ID == e.Existing.ID {
					live = &cur[i]
					break
				}
			}
			if live == nil || !live.sameStored(*e.Existing) {
				ae.Error = ErrPlanChanged.Error()
				res.Failed++
				errs = append(errs, fmt.Errorf("update %s: %w", e.Record, ErrPlanChanged))
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

// validatePlan refuses a plan that Plan could not have produced for this
// provider: foreign provider/zone, names outside the domain or the zone, or
// updates whose existing record sits at another name or type.
func (a *ProviderApplier) validatePlan(ctx context.Context, plan Plan) error {
	bad := func(format string, args ...any) error {
		return fmt.Errorf("%w: %s", ErrInvalidPlan, fmt.Sprintf(format, args...))
	}
	if plan.Provider != a.P.Name() {
		return bad("provider %q is not %q", plan.Provider, a.P.Name())
	}
	if err := ValidateHost(plan.Domain); err != nil {
		return bad("domain: %v", err)
	}
	zn := strings.ToLower(plan.Zone.Name)
	if plan.Zone.ID == "" || zn == "" || (plan.Domain != zn && !strings.HasSuffix(plan.Domain, "."+zn)) {
		return bad("zone %q does not contain domain %q", plan.Zone.Name, plan.Domain)
	}
	live, err := a.P.FindZone(ctx, plan.Domain)
	if err != nil {
		return err
	}
	if live.ID != plan.Zone.ID {
		return bad("zone %q is not the zone of %q", plan.Zone.ID, plan.Domain)
	}
	for i, e := range plan.Entries {
		if err := e.Record.Validate(); err != nil {
			return bad("entries[%d]: %v", i, err)
		}
		if e.Record.Name != plan.Domain && !strings.HasSuffix(e.Record.Name, "."+plan.Domain) {
			return bad("entries[%d]: name %q is outside domain %q", i, e.Record.Name, plan.Domain)
		}
		switch e.Action {
		case ActionCreate, ActionNoop, ActionUpdate:
		default:
			return bad("entries[%d]: unknown action %q", i, e.Action)
		}
		if e.Action == ActionUpdate && (e.Existing == nil || e.Existing.ID == "") {
			return bad("entries[%d]: update without an existing record id", i)
		}
		if e.Existing != nil {
			ex := e.Existing.Record()
			if ex.Name != e.Record.Name || ex.Type != e.Record.Type {
				return bad("entries[%d]: existing record %s %s does not match %s %s", i, ex.Type, ex.Name, e.Record.Type, e.Record.Name)
			}
			if e.Action == ActionUpdate && e.Record.Type == TypeTXT {
				// Only a recognised protocol family is ever updated, and only
				// onto a record of the same family: a forged plan must not be
				// able to overwrite google-site-verification with an SPF.
				fam := txtFamily(e.Record.Name, e.Record.Value)
				if fam == "" || txtFamily(e.Existing.Name, e.Existing.Value) != fam {
					return bad("entries[%d]: TXT update requires the existing record and the new one to share a family (spf|dmarc|dkim)", i)
				}
			}
		}
	}
	return nil
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
