package domains

import (
	"context"
	"errors"
	"testing"
)

func TestContainsToken(t *testing.T) {
	cases := []struct {
		recs  []string
		token string
		want  bool
	}{
		{[]string{"asteby-verify-abc"}, "asteby-verify-abc", true},
		{[]string{`"asteby-verify-abc"`}, "asteby-verify-abc", true},
		{[]string{`  "asteby-verify-abc"  `}, "asteby-verify-abc", true},
		{[]string{"v=spf1 -all", "asteby-verify-abc"}, "asteby-verify-abc", true},
		{[]string{"asteby-verify-abcdef"}, "asteby-verify-abc", false}, // exact, not substring
		{[]string{"prefix asteby-verify-abc"}, "asteby-verify-abc", false},
		{[]string{"asteby-verify-other"}, "asteby-verify-abc", false},
		{nil, "asteby-verify-abc", false},
		{[]string{""}, "", false},
	}
	for i, c := range cases {
		if got := ContainsToken(c.recs, c.token); got != c.want {
			t.Errorf("case %d = %v", i, got)
		}
	}
}

func TestCheckOwnership(t *testing.T) {
	const host, token = "tienda.acme.mx", "asteby-verify-abc123"
	name := "_asteby-verify." + host

	ok := newFake("r").set(TypeTXT, name, `"`+token+`"`)
	if r := CheckOwnership(context.Background(), ok, host, token); !r.Verified || r.Status != RecordOK || r.Diagnostic != nil || r.Record.Name != name {
		t.Fatalf("ok: %+v", r)
	}

	if r := CheckOwnership(context.Background(), newFake("r"), host, token); r.Verified || r.Status != RecordMissing || r.Diagnostic.Code != DiagTokenNotPublished {
		t.Fatalf("missing: %+v", r)
	}

	other := newFake("r").set(TypeTXT, name, "asteby-verify-somebody-else")
	if r := CheckOwnership(context.Background(), other, host, token); r.Verified || r.Status != RecordWrongValue || r.Diagnostic.Code != DiagTokenMismatch || len(r.Observed) != 1 {
		t.Fatalf("wrong token: %+v", r)
	}

	// A resolver failure is NOT "the record is absent".
	down := newFake("r").fail(TypeTXT, name, errors.New("i/o timeout"))
	if r := CheckOwnership(context.Background(), down, host, token); r.Verified || r.Status != RecordUnknown || r.Diagnostic.Code != DiagLookupFailed {
		t.Fatalf("lookup failure: %+v", r)
	}

	if r := CheckOwnership(context.Background(), ok, host, ""); r.Verified || r.Status != RecordMissing {
		t.Fatalf("no token: %+v", r)
	}
}
