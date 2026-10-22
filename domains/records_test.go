package domains

import (
	"strings"
	"testing"
)

func TestRecordCanonical(t *testing.T) {
	cases := []struct {
		r    Record
		want string
	}{
		{Record{Type: TypeMX, Name: "a.com", Value: "Mail.A.com.", Priority: 10}, "10 mail.a.com"},
		{Record{Type: TypeCNAME, Name: "a.com", Value: "Target.Example.com."}, "target.example.com"},
		{Record{Type: TypeTXT, Name: "a.com", Value: `  "v=spf1 -all" `}, "v=spf1 -all"},
		{Record{Type: TypeA, Name: "a.com", Value: " 203.0.113.7 "}, "203.0.113.7"},
		{Record{Type: TypeAAAA, Name: "a.com", Value: "2001:DB8::1"}, "2001:db8::1"},
	}
	for _, c := range cases {
		if got := c.r.Canonical(); got != c.want {
			t.Errorf("%v = %q want %q", c.r, got, c.want)
		}
	}
	if Abs("Example.com", "_DMARC") != "_dmarc.example.com" || Abs("example.com", "@") != "example.com" || Abs("example.com", "") != "example.com" {
		t.Error("Abs")
	}
}

func TestRecordValidate(t *testing.T) {
	good := []Record{
		{Type: TypeMX, Name: "example.com", Value: "mail.example.com", Priority: 10},
		{Type: TypeTXT, Name: "_dmarc.example.com", Value: "v=DMARC1; p=none", TTL: 300, Unique: true, Match: MatchPrefix},
		{Type: TypeCNAME, Name: "s1._domainkey.example.com", Value: "s1.dkim.example.net"},
		{Type: TypeA, Name: "mail.example.com", Value: "203.0.113.7"},
		{Type: TypeAAAA, Name: "mail.example.com", Value: "2001:db8::7"},
	}
	for _, r := range good {
		if err := r.Validate(); err != nil {
			t.Errorf("%v: %v", r, err)
		}
	}
	bad := map[string]Record{
		"type":             {Type: "SRV", Name: "a.com", Value: "x"},
		"uppercase name":   {Type: TypeTXT, Name: "A.com", Value: "x"},
		"trailing dot":     {Type: TypeTXT, Name: "a.com.", Value: "x"},
		"empty label":      {Type: TypeTXT, Name: "a..com", Value: "x"},
		"empty value":      {Type: TypeTXT, Name: "a.com", Value: "  "},
		"low ttl":          {Type: TypeTXT, Name: "a.com", Value: "x", TTL: 10},
		"priority on txt":  {Type: TypeTXT, Name: "a.com", Value: "x", Priority: 5},
		"match on mx":      {Type: TypeMX, Name: "a.com", Value: "m.a.com", Match: MatchContains},
		"bad match":        {Type: TypeTXT, Name: "a.com", Value: "x", Match: "regex"},
		"mx to ip":         {Type: TypeMX, Name: "a.com", Value: "203.0.113.1", Priority: 1},
		"mx priority":      {Type: TypeMX, Name: "a.com", Value: "m.a.com", Priority: 70000},
		"cname to ip":      {Type: TypeCNAME, Name: "a.com", Value: "1.2.3.4"},
		"a with v6":        {Type: TypeA, Name: "a.com", Value: "2001:db8::1"},
		"aaaa with v4":     {Type: TypeAAAA, Name: "a.com", Value: "1.2.3.4"},
		"txt newline":      {Type: TypeTXT, Name: "a.com", Value: "a\nb"},
		"needle w/o match": {Type: TypeTXT, Name: "a.com", Value: "x", Needle: "x"},
	}
	for name, r := range bad {
		if r.Validate() == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestRecordSetValidate(t *testing.T) {
	ok := RecordSet{Domain: "example.com", Records: []Record{
		{Type: TypeMX, Name: "example.com", Value: "mail.example.com", Priority: 10},
		{Type: TypeMX, Name: "example.com", Value: "mail2.example.com", Priority: 20},
		{Type: TypeTXT, Name: "example.com", Value: "v=spf1 mx -all"},
	}}
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	if len(ok.Slots()) != 2 || len(ok.Slots()["MX|example.com"]) != 2 {
		t.Fatalf("slots: %v", ok.Slots())
	}
	cases := map[string]RecordSet{
		"empty":        {Domain: "example.com"},
		"bad domain":   {Domain: "nodot", Records: ok.Records},
		"outside":      {Domain: "example.com", Records: []Record{{Type: TypeTXT, Name: "other.org", Value: "x"}}},
		"suffix trick": {Domain: "example.com", Records: []Record{{Type: TypeTXT, Name: "badexample.com", Value: "x"}}},
		"duplicate":    {Domain: "example.com", Records: []Record{{Type: TypeTXT, Name: "example.com", Value: "x"}, {Type: TypeTXT, Name: "example.com", Value: ` "x" `}}},
		"cname mix": {Domain: "example.com", Records: []Record{
			{Type: TypeCNAME, Name: "www.example.com", Value: "a.example.net"}, {Type: TypeTXT, Name: "www.example.com", Value: "x"}}},
		"two cnames": {Domain: "example.com", Records: []Record{
			{Type: TypeCNAME, Name: "www.example.com", Value: "a.example.net"}, {Type: TypeCNAME, Name: "www.example.com", Value: "b.example.net"}}},
		"invalid record": {Domain: "example.com", Records: []Record{{Type: TypeMX, Name: "example.com", Value: "m.example.com", Priority: -1}}},
	}
	for name, s := range cases {
		if err := s.Validate(); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
	if err := (RecordSet{Domain: "example.com", Records: []Record{{Type: TypeTXT, Name: "x.example.com", Value: "a"}, {Type: TypeCNAME, Name: "x.example.com", Value: "b.net"}}}).Validate(); err == nil || !strings.Contains(err.Error(), "CNAME") {
		t.Errorf("cname mix message: %v", err)
	}
}
