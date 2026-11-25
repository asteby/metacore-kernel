package apitoken

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

var pepperA = []byte("pepper-A-deployment-secret-0123456789")
var pepperB = []byte("pepper-B-rotated-secret-9876543210")

func fixedPepper(p []byte) func() []byte { return func() []byte { return p } }

// seedLegacy stores a token the way kernel versions before schemes did: a bare
// hex SHA-256 and the un-namespaced prefix.
func seedLegacy(t *testing.T, store Store, kind Kind) (string, *APIToken) {
	t.Helper()
	plain, err := generate("", kind, bytes.NewReader(bytes.Repeat([]byte{3}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 16, 11, 0, 0, 0, time.UTC)
	tok := &APIToken{ID: uuid.New(), OrganizationID: &org, Name: "legacy", Kind: kind, Prefix: displayPrefix("", plain),
		TokenHash: HashToken(plain), Scopes: []string{"a:read"}, Status: StatusActive, CreatedAt: now, UpdatedAt: now}
	if kind == KindPlatform {
		tok.OrganizationID = nil
	}
	if err := store.Create(context.Background(), tok); err != nil {
		t.Fatal(err)
	}
	return plain, tok
}

func TestLegacyHashStillAuthenticates(t *testing.T) {
	cases := []struct {
		name string
		opts []Option
	}{
		{"default service", nil},
		{"schemed", []Option{WithSchemedHash()}},
		{"pepper", []Option{WithPepper(fixedPepper(pepperA))}},
		{"pepper unavailable", []Option{WithPepper(func() []byte { return nil })}},
	}
	for _, c := range cases {
		for _, kind := range []Kind{KindPublishable, KindSecret, KindPlatform} {
			t.Run(c.name+"/"+string(kind), func(t *testing.T) {
				eachStore(t, func(t *testing.T, store Store) {
					plain, seeded := seedLegacy(t, store, kind)
					svc := newSvc(store, newClock(), c.opts...)
					got, err := svc.Authenticate(context.Background(), plain)
					if err != nil || got.ID != seeded.ID {
						t.Fatalf("legacy token must authenticate: %v", err)
					}
				})
			})
		}
	}
}

func TestSchemedHashColumn(t *testing.T) {
	eachStore(t, func(t *testing.T, store Store) {
		ctx := context.Background()
		for _, c := range []struct {
			name   string
			opts   []Option
			prefix string
			want   func(plain string) string
		}{
			{"default is bare legacy", nil, "", HashToken},
			{"schemed", []Option{WithSchemedHash()}, SchemeSHA256 + "$", HashTokenSchemed},
			{"pepper", []Option{WithPepper(fixedPepper(pepperA))}, SchemeHMACSHA256 + "$", func(p string) string { return HashTokenHMAC(p, pepperA) }},
			{"pepper wins over schemed", []Option{WithSchemedHash(), WithPepper(fixedPepper(pepperA))}, SchemeHMACSHA256 + "$", func(p string) string { return HashTokenHMAC(p, pepperA) }},
		} {
			svc := newSvc(store, newClock(), c.opts...)
			plain, tok, err := svc.Issue(ctx, org, c.name, KindSecret, []string{"a:read"}, nil)
			if err != nil {
				t.Fatal(c.name, err)
			}
			if tok.TokenHash != c.want(plain) {
				t.Fatalf("%s: stored hash %q", c.name, tok.TokenHash)
			}
			if !strings.HasPrefix(tok.TokenHash, c.prefix) || (c.prefix == "" && strings.Contains(tok.TokenHash, "$")) {
				t.Fatalf("%s: scheme of %q", c.name, tok.TokenHash)
			}
			if len(tok.TokenHash) > 96 {
				t.Fatalf("%s: hash exceeds column size", c.name)
			}
			if _, err := svc.Authenticate(ctx, plain); err != nil {
				t.Fatalf("%s: authenticate: %v", c.name, err)
			}
		}
	})
}

func TestSchemedLegacyReadsBothForms(t *testing.T) {
	// A token stored as "sha256$..." authenticates on a service that does not
	// write schemes, and one stored bare on a service that does.
	eachStore(t, func(t *testing.T, store Store) {
		ctx := context.Background()
		schemed := newSvc(store, newClock(), WithSchemedHash())
		p, _, err := schemed.Issue(ctx, org, "s", KindSecret, []string{"a:read"}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := newSvc(store, newClock()).Authenticate(ctx, p); err != nil {
			t.Fatalf("plain service reading sha256$: %v", err)
		}
	})
}

func TestPepper(t *testing.T) {
	cases := []struct {
		name       string
		issuePep   []byte
		authPep    func() []byte // nil = no WithPepper on the verifier
		wantValid  bool
		wantStored string
	}{
		{"same pepper", pepperA, fixedPepper(pepperA), true, SchemeHMACSHA256},
		{"verifier without pepper", pepperA, nil, false, SchemeHMACSHA256},
		{"verifier pepper empty", pepperA, func() []byte { return nil }, false, SchemeHMACSHA256},
		{"rotated pepper", pepperA, fixedPepper(pepperB), false, SchemeHMACSHA256},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			eachStore(t, func(t *testing.T, store Store) {
				ctx := context.Background()
				plain, tok, err := newSvc(store, newClock(), WithPepper(fixedPepper(c.issuePep))).Issue(ctx, org, "k", KindSecret, []string{"a:read"}, nil)
				if err != nil {
					t.Fatal(err)
				}
				if !strings.HasPrefix(tok.TokenHash, c.wantStored+"$") {
					t.Fatalf("stored %q", tok.TokenHash)
				}
				var opts []Option
				if c.authPep != nil {
					opts = append(opts, WithPepper(c.authPep))
				}
				_, err = newSvc(store, newClock(), opts...).Authenticate(ctx, plain)
				if c.wantValid && err != nil {
					t.Fatalf("expected valid: %v", err)
				}
				if !c.wantValid && !errors.Is(err, ErrInvalidToken) {
					t.Fatalf("expected ErrInvalidToken, got %v", err)
				}
			})
		})
	}
}

func TestPepperedHashIsNotPlainSHA256(t *testing.T) {
	// An attacker with a database dump cannot present the stored digest as a
	// valid bare/"sha256$" hash of some other plaintext: the digest depends on
	// the pepper.
	plain := "sk_" + strings.Repeat("A", encodedLen)
	if HashTokenHMAC(plain, pepperA) == HashTokenHMAC(plain, pepperB) {
		t.Fatal("pepper does not change the hash")
	}
	if strings.TrimPrefix(HashTokenHMAC(plain, pepperA), SchemeHMACSHA256+"$") == HashToken(plain) {
		t.Fatal("hmac equals plain sha256")
	}
}

func TestPepperUnavailableOnIssueAndRotate(t *testing.T) {
	eachStore(t, func(t *testing.T, store Store) {
		ctx := context.Background()
		have := true
		svc := newSvc(store, newClock(), WithPepper(func() []byte {
			if have {
				return pepperA
			}
			return nil
		}))
		_, tok, err := svc.Issue(ctx, org, "k", KindSecret, []string{"a:read"}, nil)
		if err != nil {
			t.Fatal(err)
		}
		have = false
		if _, _, err := svc.Issue(ctx, org, "k2", KindSecret, []string{"a:read"}, nil); !errors.Is(err, ErrPepperUnavailable) {
			t.Fatalf("issue: %v", err)
		}
		if _, _, err := svc.Rotate(ctx, tok.ID, time.Hour); !errors.Is(err, ErrPepperUnavailable) {
			t.Fatalf("rotate: %v", err)
		}
		cur, _ := svc.Get(ctx, tok.ID)
		if cur.ExpiresAt != nil || cur.Status != StatusActive {
			t.Fatal("failed rotate must not touch the old token")
		}
	})
}

func TestNamespace(t *testing.T) {
	cases := []struct {
		kind   Kind
		prefix string
	}{
		{KindPublishable, "sup_pk_"},
		{KindSecret, "sup_sk_"},
		{KindPlatform, "sup_pt_"},
	}
	for _, c := range cases {
		t.Run(string(c.kind), func(t *testing.T) {
			eachStore(t, func(t *testing.T, store Store) {
				ctx := context.Background()
				svc := newSvc(store, newClock(), WithNamespace("sup"))
				scope := []string{"a:read"}
				o := org
				if c.kind == KindPlatform {
					o = uuid.Nil
				}
				plain, tok, err := svc.Issue(ctx, o, "n", c.kind, scope, nil)
				if err != nil {
					t.Fatal(err)
				}
				if !strings.HasPrefix(plain, c.prefix) || len(plain) != len(c.prefix)+encodedLen {
					t.Fatalf("plaintext shape %q", plain[:8])
				}
				if tok.Prefix != plain[:16] || len(tok.Prefix) != 16 {
					t.Fatalf("display prefix %q", tok.Prefix)
				}
				if _, err := svc.Authenticate(ctx, plain); err != nil {
					t.Fatalf("authenticate: %v", err)
				}
				// A service without the namespace, or with another one, rejects it.
				for _, other := range []*Service{newSvc(store, newClock()), newSvc(store, newClock(), WithNamespace("hub"))} {
					if _, err := other.Authenticate(ctx, plain); !errors.Is(err, ErrInvalidToken) {
						t.Fatalf("foreign namespace must be invalid: %v", err)
					}
				}
				// The namespaced service rejects an un-namespaced token of the same secret.
				bare := strings.TrimPrefix(plain, "sup_")
				if _, err := svc.Authenticate(ctx, bare); !errors.Is(err, ErrInvalidToken) {
					t.Fatalf("bare prefix under a namespace: %v", err)
				}
			})
		})
	}
}

func TestNamespaceKeepsLegacyPrefixesDefault(t *testing.T) {
	svc := newSvc(NewMemoryStore(), newClock())
	plain, tok, err := svc.Issue(context.Background(), org, "d", KindSecret, []string{"a:read"}, nil)
	if err != nil || !strings.HasPrefix(plain, "sk_") || len(tok.Prefix) != PrefixLen {
		t.Fatalf("default deployment changed: %q %q %v", plain[:6], tok.Prefix, err)
	}
}

func TestNamespaceValidation(t *testing.T) {
	for _, ns := range []string{"", "s", "Sup", "sup_", "1up", "su-p", "toolongns", "sü"} {
		func() {
			defer func() {
				if recover() == nil {
					t.Fatalf("namespace %q accepted", ns)
				}
			}()
			WithNamespace(ns)
		}()
	}
	for _, ns := range []string{"sup", "hub2", "ab", "abcdefgh"} {
		WithNamespace(ns) // must not panic
	}
}

func TestNamespaceDisplayPrefixCapped(t *testing.T) {
	plain, _ := generate("abcdefgh", KindSecret, bytes.NewReader(make([]byte, 32)))
	if got := displayPrefix("abcdefgh", plain); len(got) != maxDisplayPrefix {
		t.Fatalf("display prefix %q exceeds the column", got)
	}
}

func TestRotationGraceUnderNamespaceAndPepper(t *testing.T) {
	eachStore(t, func(t *testing.T, store Store) {
		ctx := context.Background()
		clk := newClock()
		svc := newSvc(store, clk, WithNamespace("sup"), WithPepper(fixedPepper(pepperA)))
		oldPlain, old, err := svc.Issue(ctx, org, "k", KindSecret, []string{"support:write"}, nil)
		if err != nil {
			t.Fatal(err)
		}
		newPlain, nt, err := svc.Rotate(ctx, old.ID, 2*time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(newPlain, "sup_sk_") || !strings.HasPrefix(nt.TokenHash, SchemeHMACSHA256+"$") || nt.RotatedFromID == nil || *nt.RotatedFromID != old.ID {
			t.Fatalf("rotated token %q %q", newPlain[:8], nt.TokenHash)
		}
		for _, p := range []string{oldPlain, newPlain} {
			if _, err := svc.Authenticate(ctx, p); err != nil {
				t.Fatalf("both valid during grace: %v", err)
			}
		}
		clk.Advance(2*time.Hour + time.Second)
		if _, err := svc.Authenticate(ctx, oldPlain); !errors.Is(err, ErrExpired) {
			t.Fatalf("old after grace: %v", err)
		}
		if _, err := svc.Authenticate(ctx, newPlain); err != nil {
			t.Fatalf("new after grace: %v", err)
		}
	})
}

func TestRotateLegacyTokenIntoNamespaceAndPepper(t *testing.T) {
	// Adoption path: an old bare-hash token is rotated by a service that now
	// peppers; the old one keeps working during grace, the new one is peppered.
	eachStore(t, func(t *testing.T, store Store) {
		ctx := context.Background()
		legacyPlain, seeded := seedLegacy(t, store, KindSecret)
		svc := newSvc(store, newClock(), WithPepper(fixedPepper(pepperA)))
		np, nt, err := svc.Rotate(ctx, seeded.ID, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(nt.TokenHash, SchemeHMACSHA256+"$") {
			t.Fatalf("new hash %q", nt.TokenHash)
		}
		for _, p := range []string{legacyPlain, np} {
			if _, err := svc.Authenticate(ctx, p); err != nil {
				t.Fatalf("authenticate during grace: %v", err)
			}
		}
	})
}

func TestMalformedTokens(t *testing.T) {
	eachStore(t, func(t *testing.T, store Store) {
		ctx := context.Background()
		svc := newSvc(store, newClock(), WithNamespace("sup"), WithPepper(fixedPepper(pepperA)))
		good, _, err := svc.Issue(ctx, org, "k", KindSecret, []string{"a:read"}, nil)
		if err != nil {
			t.Fatal(err)
		}
		for name, tok := range map[string]string{
			"empty":         "",
			"prefix only":   "sup_sk_",
			"short":         good[:len(good)-1],
			"long":          good + "A",
			"bad alphabet":  "sup_sk_" + strings.Repeat("!", encodedLen),
			"unknown kind":  "sup_xx_" + good[7:],
			"double prefix": "sup_" + good,
			"upper ns":      "SUP_" + good[4:],
			"scheme as tok": HashTokenHMAC(good, pepperA),
			"digest as tok": HashToken(good),
		} {
			if _, err := svc.Authenticate(ctx, tok); !errors.Is(err, ErrInvalidToken) {
				t.Errorf("%s: %v", name, err)
			}
		}
	})
}

func TestStoredHashNeverAuthenticatesAsToken(t *testing.T) {
	// A leaked column value must not be usable as a bearer token.
	eachStore(t, func(t *testing.T, store Store) {
		ctx := context.Background()
		svc := newSvc(store, newClock())
		_, tok, err := svc.Issue(ctx, org, "k", KindSecret, []string{"a:read"}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := svc.Authenticate(ctx, "sk_"+tok.TokenHash); !errors.Is(err, ErrInvalidToken) {
			t.Fatalf("hash as secret: %v", err)
		}
	})
}

func TestAuthenticateRechecksHashInConstantTime(t *testing.T) {
	ctx := context.Background()
	mem := NewMemoryStore()
	svc := newSvc(mem, newClock())
	plain, tok, err := svc.Issue(ctx, org, "k", KindSecret, []string{"a:read"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	// A Store that returns the row for any hash (e.g. a case-insensitive
	// collation) must still not authenticate a different plaintext.
	any := anyHashStore{Store: mem, row: tok.ID}
	other, _ := generate("", KindSecret, bytes.NewReader(bytes.Repeat([]byte{9}, 32)))
	s2 := newSvc(any, newClock())
	if _, err := s2.Authenticate(ctx, other); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("store returning a foreign row: %v", err)
	}
	if _, err := s2.Authenticate(ctx, plain); err != nil {
		t.Fatalf("right plaintext: %v", err)
	}
}

type anyHashStore struct {
	Store
	row uuid.UUID
}

func (a anyHashStore) GetByHash(ctx context.Context, _ string) (*APIToken, error) {
	return a.Store.Get(ctx, a.row)
}

func TestMatchesAny(t *testing.T) {
	p := "sk_" + strings.Repeat("B", encodedLen)
	cands := hashCandidates(p, pepperA)
	if len(cands) != 3 || !strings.HasPrefix(cands[0], "hmac-sha256$") || !strings.HasPrefix(cands[1], "sha256$") || strings.Contains(cands[2], "$") {
		t.Fatalf("candidates %v", cands)
	}
	if got := hashCandidates(p, nil); len(got) != 2 {
		t.Fatalf("no pepper: %v", got)
	}
	for _, s := range cands {
		if !matchesAny(s, cands) {
			t.Fatalf("%q must match", s)
		}
	}
	for _, s := range []string{"", "sha256$", HashToken(p) + "0", strings.ToUpper(HashToken(p)), HashTokenHMAC(p, pepperB)} {
		if matchesAny(s, cands) {
			t.Fatalf("%q must not match", s)
		}
	}
}

func TestErrorsAndLogsNeverLeakSecret(t *testing.T) {
	var buf bytes.Buffer
	ctx := context.Background()
	store := NewMemoryStore()
	svc := newSvc(store, newClock(), WithNamespace("sup"), WithPepper(fixedPepper(pepperA)))
	plain, tok, err := svc.Issue(ctx, org, "k", KindSecret, []string{"a:read"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	secret := plain[strings.LastIndex(plain, "_")+1:]
	var msgs []string
	collect := func(err error) {
		if err != nil {
			msgs = append(msgs, err.Error())
		}
	}
	_, err = svc.Authenticate(ctx, plain[:len(plain)-2]+"zz")
	collect(err)
	_, err = newSvc(store, newClock(), WithNamespace("sup")).Authenticate(ctx, plain)
	collect(err)
	collect(svc.Revoke(ctx, tok.ID))
	_, err = svc.Authenticate(ctx, plain)
	collect(err)
	_, _, err = newSvc(store, newClock(), WithPepper(func() []byte { return nil })).Rotate(ctx, tok.ID, 0)
	collect(err)
	_, _, err = newSvc(store, newClock(), WithPepper(func() []byte { return nil })).Issue(ctx, org, "x", KindSecret, []string{"a:read"}, nil)
	collect(err)
	buf.WriteString(strings.Join(msgs, "\n"))
	if len(msgs) == 0 {
		t.Fatal("no errors collected")
	}
	for _, leak := range []string{secret, string(pepperA), tok.TokenHash} {
		if strings.Contains(buf.String(), leak) {
			t.Fatalf("error text leaks %q", leak[:6])
		}
	}
}

func TestKindOfIn(t *testing.T) {
	for _, c := range []struct {
		ns, prefix string
		kind       Kind
		ok         bool
	}{
		{"", "pk_", KindPublishable, true},
		{"", "sk_", KindSecret, true},
		{"sup", "sup_pt_", KindPlatform, true},
		{"sup", "sk_", "", false},
		{"", "sup_sk_", "", false},
		{"sup", "hub_sk_", "", false},
	} {
		body := strings.Repeat("A", encodedLen)
		k, ok := KindOfIn(c.ns, c.prefix+body)
		if ok != c.ok || k != c.kind {
			t.Errorf("KindOfIn(%q,%q) = %v %v", c.ns, c.prefix, k, ok)
		}
	}
}
