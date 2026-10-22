package domains

import (
	"context"
	"errors"
	"testing"
	"time"
)

var mailSet = RecordSet{Domain: "example.com", Records: []Record{
	{Type: TypeMX, Name: "example.com", Value: "mx.example.com", Priority: 10, Purpose: "mx"},
	{Type: TypeTXT, Name: "example.com", Value: "v=spf1 include:_spf.mail.test ~all", Purpose: "spf", Unique: true, Match: MatchContains, Needle: "include:_spf.mail.test"},
	{Type: TypeTXT, Name: "s1._domainkey.example.com", Value: "v=DKIM1; k=rsa; p=AAAA", Purpose: "dkim", Unique: true},
	{Type: TypeTXT, Name: "_dmarc.example.com", Value: "v=DMARC1; p=none", Purpose: "dmarc", Unique: true, Match: MatchPrefix, Needle: "v=DMARC1"},
	{Type: TypeTXT, Name: "_mta-sts.example.com", Value: "v=STSv1; id=1", Purpose: "mta_sts", Optional: true},
}}

func goodResolver(name string) *fakeResolver {
	return newFake(name).
		set(TypeMX, "example.com", "10 mx.example.com").
		set(TypeTXT, "example.com", "v=spf1 include:_spf.mail.test include:other.net ~all").
		set(TypeTXT, "s1._domainkey.example.com", `"v=DKIM1; k=rsa; p=AAAA"`).
		set(TypeTXT, "_dmarc.example.com", "v=DMARC1; p=quarantine; rua=mailto:d@example.com").
		set(TypeTXT, "_mta-sts.example.com", "v=STSv1; id=1")
}

func statusOf(c RecordSetCheck, purpose string) RecordCheck {
	for _, x := range c.Checks {
		if x.Record.Purpose == purpose {
			return x
		}
	}
	panic("no check " + purpose)
}

func hasDiag(c RecordCheck, code string) bool {
	for _, d := range c.Diagnostics {
		if d.Code == code {
			return true
		}
	}
	return false
}

func TestCheck_AllOK(t *testing.T) {
	got, err := Check(context.Background(), []DNSResolver{goodResolver("a"), goodResolver("b")}, mailSet, CheckOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !got.AllOK || got.Counts[RecordOK] != 5 {
		t.Fatalf("%+v", got.Counts)
	}
	if st := statusOf(got, "spf"); len(st.PerResolver) != 2 || st.PerResolver["a"] != RecordOK {
		t.Fatalf("per resolver: %+v", st.PerResolver)
	}
}

func TestCheck_MissingAndWrongValue(t *testing.T) {
	r := newFake("a").
		set(TypeMX, "example.com", "10 other.host.net").
		set(TypeTXT, "s1._domainkey.example.com", "v=DKIM1; k=rsa; p=ZZZZ")
	got, err := Check(context.Background(), []DNSResolver{r}, mailSet, CheckOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got.AllOK {
		t.Fatal("must not be all ok")
	}
	if c := statusOf(got, "mx"); c.Status != RecordWrongValue || !hasDiag(c, DiagValueMismatch) {
		t.Fatalf("mx: %+v", c)
	}
	if c := statusOf(got, "dkim"); c.Status != RecordWrongValue {
		t.Fatalf("dkim: %+v", c)
	}
	if c := statusOf(got, "spf"); c.Status != RecordMissing || !hasDiag(c, DiagNotFound) {
		t.Fatalf("spf: %+v", c)
	}
	if c := statusOf(got, "dmarc"); c.Status != RecordMissing {
		t.Fatalf("dmarc: %+v", c)
	}
	// optional record missing does not break AllOK on its own
	only := RecordSet{Domain: "example.com", Records: []Record{mailSet.Records[4]}}
	g2, _ := Check(context.Background(), []DNSResolver{newFake("a")}, only, CheckOptions{})
	if !g2.AllOK || g2.Checks[0].Status != RecordMissing {
		t.Fatalf("optional: %+v", g2)
	}
}

func TestEvaluate_PartialPropagation(t *testing.T) {
	a, b := goodResolver("a"), newFake("b") // b has not seen anything yet
	got, err := Check(context.Background(), []DNSResolver{a, b}, mailSet, CheckOptions{})
	if err != nil {
		t.Fatal(err)
	}
	c := statusOf(got, "mx")
	if c.Status != RecordPropagating || !hasDiag(c, DiagPartialPropag) || c.PerResolver["b"] != RecordMissing {
		t.Fatalf("%+v", c)
	}
	if got.AllOK {
		t.Fatal("propagating is not ok yet")
	}
}

func TestEvaluate_RecentlyAppliedIsPropagating(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	r := newFake("a")
	got, _ := Check(context.Background(), []DNSResolver{r}, mailSet, CheckOptions{EvalOptions: EvalOptions{AppliedAt: now.Add(-10 * time.Minute), Now: now}})
	if c := statusOf(got, "mx"); c.Status != RecordPropagating || !hasDiag(c, DiagRecentlyApplied) {
		t.Fatalf("recent: %+v", c)
	}
	// Outside the window it is plainly missing.
	got, _ = Check(context.Background(), []DNSResolver{r}, mailSet, CheckOptions{EvalOptions: EvalOptions{AppliedAt: now.Add(-3 * time.Hour), Now: now}})
	if c := statusOf(got, "mx"); c.Status != RecordMissing {
		t.Fatalf("old: %+v", c)
	}
	// Custom window.
	got, _ = Check(context.Background(), []DNSResolver{r}, mailSet, CheckOptions{EvalOptions: EvalOptions{AppliedAt: now.Add(-3 * time.Hour), Window: 4 * time.Hour, Now: now}})
	if c := statusOf(got, "mx"); c.Status != RecordPropagating {
		t.Fatalf("window: %+v", c)
	}
	// A record that IS visible stays ok even if recently applied.
	got, _ = Check(context.Background(), []DNSResolver{goodResolver("a")}, mailSet, CheckOptions{EvalOptions: EvalOptions{AppliedAt: now, Now: now}})
	if !got.AllOK {
		t.Fatalf("visible records stay ok: %+v", got.Counts)
	}
}

func TestEvaluate_LookupFailureIsUnknownNotMissing(t *testing.T) {
	r := newFake("a").fail(TypeMX, "example.com", errors.New("read udp: i/o timeout"))
	got, _ := Check(context.Background(), []DNSResolver{r}, mailSet, CheckOptions{})
	c := statusOf(got, "mx")
	if c.Status != RecordUnknown || !hasDiag(c, DiagLookupFailed) {
		t.Fatalf("%+v", c)
	}
	// one resolver ok, another failing: not contradicted -> ok
	good, down := goodResolver("a"), newFake("b").fail(TypeMX, "example.com", errors.New("servfail"))
	got, _ = Check(context.Background(), []DNSResolver{good, down}, RecordSet{Domain: "example.com", Records: mailSet.Records[:1]}, CheckOptions{})
	if got.Checks[0].Status != RecordOK {
		t.Fatalf("ok + unknown should be ok: %+v", got.Checks[0])
	}
	// one resolver missing, another failing: missing (a real answer beats an error)
	miss := newFake("a")
	got, _ = Check(context.Background(), []DNSResolver{miss, down}, RecordSet{Domain: "example.com", Records: mailSet.Records[:1]}, CheckOptions{})
	if got.Checks[0].Status != RecordMissing {
		t.Fatalf("missing + unknown: %+v", got.Checks[0])
	}
}

func TestEvaluate_SPFMatchModes(t *testing.T) {
	spfOnly := RecordSet{Domain: "example.com", Records: []Record{mailSet.Records[1]}}
	run := func(vals ...string) RecordCheck {
		r := newFake("a").set(TypeTXT, "example.com", vals...)
		g, _ := Check(context.Background(), []DNSResolver{r}, spfOnly, CheckOptions{})
		return g.Checks[0]
	}
	if c := run("v=spf1 include:other.net include:_SPF.mail.test -all"); c.Status != RecordOK {
		t.Fatalf("contains is case-insensitive: %+v", c)
	}
	if c := run("v=spf1 include:other.net -all"); c.Status != RecordWrongValue {
		t.Fatalf("missing include: %+v", c)
	}
	c := run("v=spf1 include:_spf.mail.test ~all", "v=spf1 include:other.net ~all")
	if c.Status != RecordWrongValue || !hasDiag(c, DiagMultipleSPF) {
		t.Fatalf("two SPF records are a permerror: %+v", c)
	}
	// non-SPF TXT next to ours is fine
	if c := run("google-site-verification=abc", "v=spf1 include:_spf.mail.test ~all"); c.Status != RecordOK {
		t.Fatalf("%+v", c)
	}
}

func TestEvaluate_MXShadowing(t *testing.T) {
	set := RecordSet{Domain: "example.com", Records: []Record{mailSet.Records[0]}}
	run := func(vals ...string) RecordCheck {
		g, _ := Check(context.Background(), []DNSResolver{newFake("a").set(TypeMX, "example.com", vals...)}, set, CheckOptions{})
		return g.Checks[0]
	}
	if c := run("10 mx.example.com"); c.Status != RecordOK {
		t.Fatalf("%+v", c)
	}
	if c := run("10 mx.example.com", "20 backup.other.net"); c.Status != RecordOK {
		t.Fatalf("higher-number foreign MX does not shadow: %+v", c)
	}
	c := run("10 mx.example.com", "5 old.mail.net")
	if c.Status != RecordWrongValue || !hasDiag(c, DiagMXShadowed) {
		t.Fatalf("lower-number foreign MX shadows ours: %+v", c)
	}
	if c := run("10 MX.Example.com."); c.Status != RecordOK {
		t.Fatalf("case/trailing dot: %+v", c)
	}
}

func TestEvaluate_CNAMEAndAddress(t *testing.T) {
	set := RecordSet{Domain: "example.com", Records: []Record{
		{Type: TypeCNAME, Name: "s2._domainkey.example.com", Value: "s2.dkim.mail.test", Purpose: "dkim2"},
		{Type: TypeA, Name: "mail.example.com", Value: "203.0.113.7", Purpose: "a"},
		{Type: TypeAAAA, Name: "mail.example.com", Value: "2001:db8::7", Purpose: "aaaa"},
	}}
	r := newFake("a").
		set(TypeCNAME, "s2._domainkey.example.com", "s2.dkim.mail.test").
		set(TypeA, "mail.example.com", "203.0.113.7", "198.51.100.1").
		set(TypeAAAA, "mail.example.com", "2001:db8:0:0:0:0:0:7")
	g, _ := Check(context.Background(), []DNSResolver{r}, set, CheckOptions{})
	if !g.AllOK {
		t.Fatalf("%+v", g.Counts)
	}
	r2 := newFake("a").set(TypeCNAME, "s2._domainkey.example.com", "other.net").set(TypeA, "mail.example.com", "198.51.100.1")
	g, _ = Check(context.Background(), []DNSResolver{r2}, set, CheckOptions{})
	if statusOf(g, "dkim2").Status != RecordWrongValue || statusOf(g, "a").Status != RecordWrongValue || statusOf(g, "aaaa").Status != RecordMissing {
		t.Fatalf("%+v", g.Counts)
	}
}

func TestEvaluate_UniqueWithExtraValuesWarns(t *testing.T) {
	set := RecordSet{Domain: "example.com", Records: []Record{mailSet.Records[2]}}
	r := newFake("a").set(TypeTXT, "s1._domainkey.example.com", "v=DKIM1; k=rsa; p=AAAA", "v=DKIM1; k=rsa; p=OLD")
	g, _ := Check(context.Background(), []DNSResolver{r}, set, CheckOptions{})
	if g.Checks[0].Status != RecordOK || !hasDiag(g.Checks[0], DiagUniqueHasExtra) {
		t.Fatalf("%+v", g.Checks[0])
	}
}

func TestCheck_Validation(t *testing.T) {
	if _, err := Check(context.Background(), nil, mailSet, CheckOptions{}); err == nil {
		t.Fatal("no resolvers")
	}
	if _, err := Check(context.Background(), []DNSResolver{newFake("a")}, RecordSet{}, CheckOptions{}); err == nil {
		t.Fatal("invalid set")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Check(ctx, []DNSResolver{newFake("a")}, mailSet, CheckOptions{}); err == nil {
		t.Fatal("cancelled ctx")
	}
}

func TestCheck_OneLookupPerSlotPerResolver(t *testing.T) {
	// Two MX records share one slot: a single lookup serves both.
	set := RecordSet{Domain: "example.com", Records: []Record{
		{Type: TypeMX, Name: "example.com", Value: "mx1.example.com", Priority: 10},
		{Type: TypeMX, Name: "example.com", Value: "mx2.example.com", Priority: 20},
	}}
	r := newFake("a").set(TypeMX, "example.com", "10 mx1.example.com", "20 mx2.example.com")
	g, _ := Check(context.Background(), []DNSResolver{r}, set, CheckOptions{})
	if !g.AllOK || r.hits != 1 {
		t.Fatalf("ok=%v hits=%d", g.AllOK, r.hits)
	}
}

func TestRecordStatusValid(t *testing.T) {
	for _, s := range []RecordStatus{RecordOK, RecordMissing, RecordWrongValue, RecordPropagating, RecordUnknown} {
		if !s.Valid() {
			t.Errorf("%s", s)
		}
	}
	if RecordStatus("pending").Valid() {
		t.Error("pending is not a record status")
	}
	if string(RecordMissing) != "missing" || string(RecordWrongValue) != "wrong_value" || string(RecordPropagating) != "propagating" || string(RecordOK) != "ok" {
		t.Error("wire values")
	}
}
