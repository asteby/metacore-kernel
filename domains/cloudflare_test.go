package domains

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

const cfToken = "cf-secret-token-123"

// cfFake is a tiny Cloudflare v4 emulation: zones + dns_records.
type cfFake struct {
	mu       sync.Mutex
	srv      *httptest.Server
	zones    map[string]string // name -> id
	recs     map[string][]cfRecord
	next     int
	requests []string
	bodies   []cfRecord
	status   int // forced status for dns_records calls (0 = normal)
	perPage  int
}

func newCF(t *testing.T) *cfFake {
	f := &cfFake{zones: map[string]string{"example.com": "zone-1"}, recs: map[string][]cfRecord{}, perPage: 100}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *cfFake) provider() *Cloudflare {
	return NewCloudflare(cfToken, withCloudflareBaseURL(f.srv.URL))
}

func (f *cfFake) write(w http.ResponseWriter, code int, result any, info map[string]any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	b, _ := json.Marshal(result)
	out := map[string]any{"success": code < 300, "errors": []any{}, "result": json.RawMessage(b)}
	if info != nil {
		out["result_info"] = info
	}
	if code >= 300 {
		out["errors"] = []map[string]any{{"code": 10000, "message": "Authentication error"}}
	}
	_ = json.NewEncoder(w).Encode(out)
}

func (f *cfFake) handle(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, r.Method+" "+r.URL.Path)
	if r.Header.Get("Authorization") != "Bearer "+cfToken {
		f.write(w, 403, nil, nil)
		return
	}
	switch {
	case r.URL.Path == "/user/tokens/verify":
		f.write(w, 200, map[string]string{"status": "active"}, nil)
	case r.URL.Path == "/zones":
		name := r.URL.Query().Get("name")
		var out []map[string]string
		if id, ok := f.zones[name]; ok {
			out = append(out, map[string]string{"id": id, "name": name})
		}
		if out == nil {
			out = []map[string]string{}
		}
		f.write(w, 200, out, nil)
	case strings.HasPrefix(r.URL.Path, "/zones/") && strings.Contains(r.URL.Path, "/dns_records"):
		if f.status != 0 {
			w.Header().Set("Retry-After", "7")
			f.write(w, f.status, nil, nil)
			return
		}
		zone := strings.Split(r.URL.Path, "/")[2]
		rest := strings.TrimPrefix(r.URL.Path, "/zones/"+zone+"/dns_records")
		switch {
		case r.Method == http.MethodGet && rest == "":
			name := r.URL.Query().Get("name")
			var all []cfRecord
			for _, x := range f.recs[zone] {
				if x.Name == name {
					all = append(all, x)
				}
			}
			page := 1
			fmt.Sscanf(r.URL.Query().Get("page"), "%d", &page)
			pages := (len(all) + f.perPage - 1) / f.perPage
			if pages == 0 {
				pages = 1
			}
			lo, hi := (page-1)*f.perPage, page*f.perPage
			if lo > len(all) {
				lo = len(all)
			}
			if hi > len(all) {
				hi = len(all)
			}
			part := all[lo:hi]
			if part == nil {
				part = []cfRecord{}
			}
			f.write(w, 200, part, map[string]any{"total_pages": pages})
		case r.Method == http.MethodPost && rest == "":
			var in cfRecord
			b, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(b, &in)
			f.bodies = append(f.bodies, in)
			f.next++
			in.ID = fmt.Sprintf("rec-%d", f.next)
			if in.Type == "TXT" {
				in.Content = `"` + in.Content + `"` // Cloudflare echoes TXT quoted
			}
			f.recs[zone] = append(f.recs[zone], in)
			f.write(w, 200, in, nil)
		case r.Method == http.MethodPut:
			// The applier must never PUT (it would reset comment/tags/proxied).
			f.write(w, 405, nil, nil)
		case r.Method == http.MethodPatch:
			id := strings.TrimPrefix(rest, "/")
			var in cfRecord
			b, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(b, &in)
			f.bodies = append(f.bodies, in)
			for i := range f.recs[zone] {
				if f.recs[zone][i].ID == id {
					old := f.recs[zone][i]
					in.ID = id
					if in.Type == "TXT" {
						in.Content = `"` + in.Content + `"`
					}
					// PATCH semantics: omitted fields keep their stored value.
					if in.Proxied == nil {
						in.Proxied = old.Proxied
					}
					if in.Comment == "" {
						in.Comment = old.Comment
					}
					if in.Tags == nil {
						in.Tags = old.Tags
					}
					f.recs[zone][i] = in
					f.write(w, 200, in, nil)
					return
				}
			}
			f.write(w, 404, nil, nil)
		default:
			f.write(w, 405, nil, nil)
		}
	default:
		f.write(w, 404, nil, nil)
	}
}

func TestCloudflare_PlanApplyEndToEnd(t *testing.T) {
	f := newCF(t)
	f.recs["zone-1"] = []cfRecord{
		{ID: "old-spf", Type: "TXT", Name: "example.com", Content: `"v=spf1 include:old.net -all"`, TTL: 1},
	}
	a := NewProviderApplier(f.provider())
	set := RecordSet{Domain: "mail.example.com", Records: []Record{
		{Type: TypeMX, Name: "mail.example.com", Value: "mx.mail.test.", Priority: 10},
		{Type: TypeTXT, Name: "mail.example.com", Value: "v=spf1 include:_spf.mail.test ~all", Unique: true, TTL: 300},
		{Type: TypeCNAME, Name: "s1._domainkey.mail.example.com", Value: "s1.dkim.mail.test"},
		{Type: TypeA, Name: "host.mail.example.com", Value: "203.0.113.7"},
	}}
	plan, err := a.Plan(context.Background(), set)
	if err != nil {
		t.Fatal(err)
	}
	// Zone found by walking up: mail.example.com -> example.com
	if plan.Zone.ID != "zone-1" || plan.Zone.Name != "example.com" {
		t.Fatalf("zone: %+v", plan.Zone)
	}
	if plan.Counts()[ActionCreate] != 4 {
		t.Fatalf("plan: %v", plan.Counts())
	}
	for _, r := range f.requests {
		if strings.HasPrefix(r, "POST") || strings.HasPrefix(r, "PUT") || strings.HasPrefix(r, "DELETE") {
			t.Fatalf("Plan must be read-only, saw %s", r)
		}
	}
	res, err := a.Apply(context.Background(), plan)
	if err != nil || res.Created != 4 {
		t.Fatalf("%+v %v", res, err)
	}
	byType := map[string]cfRecord{}
	for _, b := range f.bodies {
		byType[b.Type] = b
	}
	if mx := byType["MX"]; mx.Content != "mx.mail.test" || mx.Priority == nil || *mx.Priority != 10 || mx.TTL != 1 || mx.Proxied != nil {
		t.Fatalf("MX body: %+v", mx)
	}
	if tx := byType["TXT"]; tx.Content != "v=spf1 include:_spf.mail.test ~all" || tx.TTL != 300 {
		t.Fatalf("TXT body: %+v", tx)
	}
	for _, typ := range []string{"CNAME", "A"} {
		if p := byType[typ].Proxied; p == nil || *p {
			t.Fatalf("%s must be DNS-only (proxied=false): %+v", typ, byType[typ])
		}
	}
	// Re-plan: converged (TXT quotes returned by Cloudflare normalize away).
	plan2, err := a.Plan(context.Background(), set)
	if err != nil {
		t.Fatal(err)
	}
	if plan2.Changes() {
		t.Fatalf("applied zone must plan noop, got %v", plan2.Counts())
	}
}

func TestCloudflare_UpdateExistingUniqueTXT(t *testing.T) {
	f := newCF(t)
	f.recs["zone-1"] = []cfRecord{{ID: "old-spf", Type: "TXT", Name: "example.com", Content: `"v=spf1 include:old.net -all"`, TTL: 1}}
	a := NewProviderApplier(f.provider())
	set := RecordSet{Domain: "example.com", Records: []Record{{Type: TypeTXT, Name: "example.com", Value: "v=spf1 include:_spf.mail.test ~all", Unique: true}}}
	plan, err := a.Plan(context.Background(), set)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Entries[0].Action != ActionUpdate || plan.Entries[0].Existing.ID != "old-spf" || plan.Entries[0].Existing.Value != "v=spf1 include:old.net -all" {
		t.Fatalf("%+v", plan.Entries[0])
	}
	if res, err := a.Apply(context.Background(), plan); err != nil || res.Updated != 1 {
		t.Fatalf("%+v %v", res, err)
	}
	if got := f.recs["zone-1"][0].Content; got != `"v=spf1 include:_spf.mail.test ~all"` {
		t.Fatalf("stored: %s", got)
	}
}

func TestCloudflare_ZoneNotFoundAndAuth(t *testing.T) {
	f := newCF(t)
	p := f.provider()
	if _, err := p.FindZone(context.Background(), "nothere.org"); !errors.Is(err, ErrZoneNotFound) {
		t.Fatalf("zone: %v", err)
	}
	bad := NewCloudflare("wrong-token", withCloudflareBaseURL(f.srv.URL))
	_, err := bad.FindZone(context.Background(), "example.com")
	if !errors.Is(err, ErrProviderAuth) {
		t.Fatalf("auth: %v", err)
	}
	if strings.Contains(err.Error(), "wrong-token") {
		t.Fatal("errors must never contain the token")
	}
	if err := bad.Verify(context.Background()); !errors.Is(err, ErrProviderAuth) {
		t.Fatalf("verify: %v", err)
	}
	if err := p.Verify(context.Background()); err != nil {
		t.Fatalf("verify ok: %v", err)
	}
	if _, err := NewCloudflare("", withCloudflareBaseURL(f.srv.URL)).FindZone(context.Background(), "example.com"); !errors.Is(err, ErrProviderAuth) {
		t.Fatalf("empty token: %v", err)
	}
}

func TestCloudflare_RateLimitAndPagination(t *testing.T) {
	f := newCF(t)
	f.perPage = 2
	for i := 0; i < 5; i++ {
		f.recs["zone-1"] = append(f.recs["zone-1"], cfRecord{ID: fmt.Sprintf("r%d", i), Type: "TXT", Name: "example.com", Content: fmt.Sprintf(`"v%d"`, i), TTL: 1})
	}
	p := f.provider()
	got, err := p.ListRecords(context.Background(), Zone{ID: "zone-1", Name: "example.com"}, "example.com")
	if err != nil || len(got) != 5 || got[4].Value != "v4" {
		t.Fatalf("pagination: %d %v %v", len(got), err, got)
	}
	f.status = 429
	_, err = p.ListRecords(context.Background(), Zone{ID: "zone-1"}, "example.com")
	if !errors.Is(err, ErrRateLimited) || !strings.Contains(err.Error(), "7") {
		t.Fatalf("rate limit: %v", err)
	}
}

func TestCloudflare_NeverLeaksToken(t *testing.T) {
	c := NewCloudflare(cfToken)
	for _, s := range []string{c.String(), fmt.Sprintf("%v", c), fmt.Sprintf("%+v", c), fmt.Sprintf("%#v", c), fmt.Sprint(c)} {
		if strings.Contains(s, cfToken) {
			t.Fatalf("token leaked: %s", s)
		}
	}
}

func TestCloudflare_UpdateRequiresID(t *testing.T) {
	f := newCF(t)
	if _, err := f.provider().UpdateRecord(context.Background(), Zone{ID: "zone-1"}, "", Record{}); err == nil {
		t.Fatal("empty id")
	}
}

func TestCloudflare_UpdatePreservesCommentTagsAndProxied(t *testing.T) {
	f := newCF(t)
	yes := true
	f.recs["zone-1"] = []cfRecord{{ID: "h", Type: "CNAME", Name: "mail.example.com", Content: "old.target.net", TTL: 1, Proxied: &yes, Comment: "owned by ops", Tags: []string{"team:infra"}}}
	a := NewProviderApplier(f.provider())
	set := RecordSet{Domain: "example.com", Records: []Record{{Type: TypeCNAME, Name: "mail.example.com", Value: "new.target.net"}}}
	plan, err := a.Plan(context.Background(), set)
	if err != nil {
		t.Fatal(err)
	}
	e := plan.Entries[0]
	if e.Action != ActionUpdate || e.Existing.Comment != "owned by ops" || e.Existing.Proxied == nil || !*e.Existing.Proxied || !hasWarn(e, DiagProxiedExisting) {
		t.Fatalf("plan must show preserved metadata and warn about proxied: %+v", e)
	}
	if res, err := a.Apply(context.Background(), plan); err != nil || res.Updated != 1 {
		t.Fatalf("%+v %v", res, err)
	}
	got := f.recs["zone-1"][0]
	if got.Content != "new.target.net" || got.Comment != "owned by ops" || len(got.Tags) != 1 || got.Proxied == nil || !*got.Proxied {
		t.Fatalf("comment/tags/proxied must survive the update: %+v", got)
	}
	last := f.bodies[len(f.bodies)-1]
	if last.Proxied != nil || last.Comment != "" || last.Tags != nil {
		t.Fatalf("PATCH body must not carry preserved fields: %+v", last)
	}
	// The preserved metadata is part of the fingerprint.
	other := plan
	other.Entries = append([]PlanEntry(nil), plan.Entries...)
	ex := *other.Entries[0].Existing
	ex.Comment = "changed"
	other.Entries[0].Existing = &ex
	if other.Fingerprint() == plan.Fingerprint() {
		t.Fatal("fingerprint must cover comment")
	}
}
