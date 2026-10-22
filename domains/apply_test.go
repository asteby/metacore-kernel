package domains

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
)

// memProvider is an in-memory ZoneProvider recording every write.
type memProvider struct {
	mu      sync.Mutex
	zone    Zone
	recs    []ProviderRecord
	next    int
	creates []Record
	updates []Record
	failOn  string // "Type|name" whose write fails
	zoneErr error
	lists   int
}

func newMem(zoneName string) *memProvider { return &memProvider{zone: Zone{ID: "z1", Name: zoneName}} }

func (m *memProvider) Name() string { return "mem" }
func (m *memProvider) add(t RecordType, name, value string, prio int) {
	m.next++
	m.recs = append(m.recs, ProviderRecord{ID: fmt.Sprintf("r%d", m.next), Type: t, Name: name, Value: value, Priority: prio})
}
func (m *memProvider) FindZone(_ context.Context, host string) (Zone, error) {
	if m.zoneErr != nil {
		return Zone{}, m.zoneErr
	}
	if host == m.zone.Name || strings.HasSuffix(host, "."+m.zone.Name) {
		return m.zone, nil
	}
	return Zone{}, ErrZoneNotFound
}
func (m *memProvider) ListRecords(_ context.Context, _ Zone, name string) ([]ProviderRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lists++
	var out []ProviderRecord
	for _, r := range m.recs {
		if r.Name == name {
			out = append(out, r)
		}
	}
	return out, nil
}
func (m *memProvider) CreateRecord(_ context.Context, _ Zone, r Record) (ProviderRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failOn == r.Key() {
		return ProviderRecord{}, errors.New("provider says no")
	}
	m.creates = append(m.creates, r)
	m.next++
	p := ProviderRecord{ID: fmt.Sprintf("r%d", m.next), Type: r.Type, Name: r.Name, Value: r.Value, Priority: r.Priority}
	m.recs = append(m.recs, p)
	return p, nil
}
func (m *memProvider) UpdateRecord(_ context.Context, _ Zone, id string, r Record) (ProviderRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.recs {
		if m.recs[i].ID == id {
			m.updates = append(m.updates, r)
			m.recs[i].Value, m.recs[i].Priority = r.Value, r.Priority
			return m.recs[i], nil
		}
	}
	return ProviderRecord{}, errors.New("not found")
}

func TestPlan_FreshZoneCreatesEverything(t *testing.T) {
	m := newMem("example.com")
	a := NewProviderApplier(m)
	plan, err := a.Plan(context.Background(), mailSet)
	if err != nil {
		t.Fatal(err)
	}
	if c := plan.Counts(); c[ActionCreate] != 5 || len(m.creates) != 0 {
		t.Fatalf("plan must be read-only and create 5: %v writes=%d", c, len(m.creates))
	}
	if plan.Zone.ID != "z1" || plan.Provider != "mem" || !plan.Changes() || plan.HasBlocked() {
		t.Fatalf("%+v", plan)
	}
}

func TestApply_IsIdempotent(t *testing.T) {
	m := newMem("example.com")
	a := NewProviderApplier(m)
	plan, _ := a.Plan(context.Background(), mailSet)
	res, err := a.Apply(context.Background(), plan)
	if err != nil || res.Created != 5 || res.Failed != 0 || res.AppliedAt.IsZero() {
		t.Fatalf("first apply: %+v %v", res, err)
	}
	// Re-applying the SAME (stale) plan must not duplicate anything.
	res, err = a.Apply(context.Background(), plan)
	if err != nil || res.Created != 0 || res.Unchanged != 5 {
		t.Fatalf("second apply of stale plan: %+v %v", res, err)
	}
	if len(m.recs) != 5 || len(m.creates) != 5 {
		t.Fatalf("records=%d creates=%d", len(m.recs), len(m.creates))
	}
	// A fresh plan is a pure noop.
	plan2, _ := a.Plan(context.Background(), mailSet)
	if plan2.Changes() || plan2.Counts()[ActionNoop] != 5 {
		t.Fatalf("converged zone must plan noop: %v", plan2.Counts())
	}
}

func TestPlan_UniqueUpdatesExistingWithWarnings(t *testing.T) {
	m := newMem("example.com")
	m.add(TypeTXT, "example.com", "v=spf1 include:old.net -all", 0)
	m.add(TypeTXT, "_dmarc.example.com", "v=DMARC1; p=reject", 0) // already satisfies needle v=DMARC1
	m.add(TypeTXT, "s1._domainkey.example.com", "v=DKIM1; k=rsa; p=OLD", 0)
	m.add(TypeTXT, "s1._domainkey.example.com", "v=DKIM1; k=rsa; p=OLDER", 0)
	m.add(TypeMX, "example.com", "mail.elsewhere.net", 5)
	a := NewProviderApplier(m)
	plan, err := a.Plan(context.Background(), mailSet)
	if err != nil {
		t.Fatal(err)
	}
	byPurpose := map[string]PlanEntry{}
	for _, e := range plan.Entries {
		byPurpose[e.Record.Purpose] = e
	}
	if e := byPurpose["spf"]; e.Action != ActionUpdate || e.Existing == nil || !hasWarn(e, DiagReplacesExisting) {
		t.Fatalf("spf must update with a replaces_existing warning: %+v", e)
	}
	if e := byPurpose["dmarc"]; e.Action != ActionNoop {
		t.Fatalf("dmarc already satisfied by needle: %+v", e)
	}
	if e := byPurpose["dkim"]; e.Action != ActionUpdate || !hasWarn(e, DiagExtraExisting) {
		t.Fatalf("dkim: %+v", e)
	}
	if e := byPurpose["mx"]; e.Action != ActionCreate || !hasWarn(e, DiagMXOtherTarget) {
		t.Fatalf("mx must be created next to the foreign one, with a warning: %+v", e)
	}
	res, err := a.Apply(context.Background(), plan)
	if err != nil || res.Updated != 2 || res.Created != 2 || res.Unchanged != 1 {
		t.Fatalf("%+v %v", res, err)
	}
	// Nothing was ever deleted.
	if len(m.recs) != 5+2 { // 5 existing + new MX + new mta_sts (updates are in place)
		t.Fatalf("records after apply = %d", len(m.recs))
	}
	for _, r := range m.recs {
		if r.Value == "mail.elsewhere.net" {
			return
		}
	}
	t.Fatal("the tenant's own MX must survive")
}

func hasWarn(e PlanEntry, code string) bool {
	for _, w := range e.Warnings {
		if w.Code == code {
			return true
		}
	}
	return false
}

func TestPlan_CNAMEConflictIsBlocked(t *testing.T) {
	m := newMem("example.com")
	m.add(TypeCNAME, "s1._domainkey.example.com", "somewhere.net", 0)
	set := RecordSet{Domain: "example.com", Records: []Record{
		{Type: TypeTXT, Name: "s1._domainkey.example.com", Value: "v=DKIM1; p=A", Unique: true},
	}}
	a := NewProviderApplier(m)
	plan, err := a.Plan(context.Background(), set)
	if err != nil {
		t.Fatal(err)
	}
	if !plan.HasBlocked() || plan.Entries[0].Action != ActionBlocked || !hasWarn(plan.Entries[0], DiagCNAMEConflict) {
		t.Fatalf("%+v", plan.Entries)
	}
	if _, err := a.Apply(context.Background(), plan); !errors.Is(err, ErrPlanBlocked) || len(m.creates) != 0 {
		t.Fatalf("blocked plans must not write: %v", err)
	}
	// and the reverse: a CNAME where other types already live
	m2 := newMem("example.com")
	m2.add(TypeTXT, "www.example.com", "hello", 0)
	plan, _ = NewProviderApplier(m2).Plan(context.Background(), RecordSet{Domain: "example.com", Records: []Record{{Type: TypeCNAME, Name: "www.example.com", Value: "t.net"}}})
	if !plan.HasBlocked() {
		t.Fatalf("cname over txt: %+v", plan.Entries)
	}
	// an existing equal CNAME is a noop, a different one is updated in place
	m3 := newMem("example.com")
	m3.add(TypeCNAME, "www.example.com", "T.net.", 0)
	cn := RecordSet{Domain: "example.com", Records: []Record{{Type: TypeCNAME, Name: "www.example.com", Value: "t.net"}}}
	plan, _ = NewProviderApplier(m3).Plan(context.Background(), cn)
	if plan.Entries[0].Action != ActionNoop {
		t.Fatalf("%+v", plan.Entries[0])
	}
	cn.Records[0].Value = "u.net"
	plan, _ = NewProviderApplier(m3).Plan(context.Background(), cn)
	if plan.Entries[0].Action != ActionUpdate {
		t.Fatalf("a name holds one CNAME: %+v", plan.Entries[0])
	}
}

func TestApplyConfirmed_FingerprintGuardsStalePlans(t *testing.T) {
	m := newMem("example.com")
	a := NewProviderApplier(m)
	plan, _ := a.Plan(context.Background(), mailSet)
	fp := plan.Fingerprint()
	if fp != plan.Fingerprint() || len(fp) != 64 {
		t.Fatal("fingerprint must be stable")
	}

	// The tenant edits the zone while the confirmation dialog is open.
	m.add(TypeTXT, "example.com", "v=spf1 include:surprise.net -all", 0)
	res, fresh, err := ApplyConfirmed(context.Background(), a, mailSet, fp)
	if !errors.Is(err, ErrPlanChanged) || res.Created != 0 || len(m.creates) != 0 {
		t.Fatalf("stale confirmation must not write: %v %+v", err, res)
	}
	if fresh.Fingerprint() == fp {
		t.Fatal("the fresh plan must differ")
	}
	if _, _, err := ApplyConfirmed(context.Background(), a, mailSet, ""); !errors.Is(err, ErrPlanChanged) {
		t.Fatalf("empty confirmation: %v", err)
	}
	res, _, err = ApplyConfirmed(context.Background(), a, mailSet, fresh.Fingerprint())
	if err != nil || res.Created+res.Updated != 5 {
		t.Fatalf("confirmed apply: %+v %v", res, err)
	}
}

func TestFingerprintIgnoresOrderButNotContent(t *testing.T) {
	m := newMem("example.com")
	a := NewProviderApplier(m)
	p1, _ := a.Plan(context.Background(), mailSet)
	rev := RecordSet{Domain: mailSet.Domain}
	for i := len(mailSet.Records) - 1; i >= 0; i-- {
		rev.Records = append(rev.Records, mailSet.Records[i])
	}
	p2, _ := a.Plan(context.Background(), rev)
	if p1.Fingerprint() != p2.Fingerprint() {
		t.Fatal("entry order must not change the fingerprint")
	}
	changed := mailSet
	changed.Records = append([]Record(nil), mailSet.Records...)
	changed.Records[0].Value = "mx9.example.com"
	p3, _ := a.Plan(context.Background(), changed)
	if p3.Fingerprint() == p1.Fingerprint() {
		t.Fatal("content change must change the fingerprint")
	}
}

func TestApply_PartialFailureReported(t *testing.T) {
	m := newMem("example.com")
	m.failOn = "TXT|_dmarc.example.com"
	a := NewProviderApplier(m)
	plan, _ := a.Plan(context.Background(), mailSet)
	res, err := a.Apply(context.Background(), plan)
	if err == nil || res.Failed != 1 || res.Created != 4 {
		t.Fatalf("%+v %v", res, err)
	}
	var failed *AppliedEntry
	for i := range res.Entries {
		if res.Entries[i].Error != "" {
			failed = &res.Entries[i]
		}
	}
	if failed == nil || failed.Record.Purpose != "dmarc" || strings.Contains(err.Error(), "token") {
		t.Fatalf("%+v", failed)
	}
	// Retry converges: only the failed record is attempted again.
	m.failOn = ""
	plan, _ = a.Plan(context.Background(), mailSet)
	if plan.Counts()[ActionCreate] != 1 {
		t.Fatalf("retry plan: %v", plan.Counts())
	}
}

func TestPlan_ErrorsPropagate(t *testing.T) {
	m := newMem("example.com")
	a := NewProviderApplier(m)
	if _, err := a.Plan(context.Background(), RecordSet{Domain: "other.org", Records: []Record{{Type: TypeTXT, Name: "other.org", Value: "x"}}}); !errors.Is(err, ErrZoneNotFound) {
		t.Fatalf("zone: %v", err)
	}
	m.zoneErr = ErrProviderAuth
	if _, err := a.Plan(context.Background(), mailSet); !errors.Is(err, ErrProviderAuth) {
		t.Fatalf("auth: %v", err)
	}
	if _, err := a.Plan(context.Background(), RecordSet{}); err == nil {
		t.Fatal("invalid set")
	}
	if a.Name() != "mem" {
		t.Fatal("name")
	}
}
