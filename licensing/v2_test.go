package licensing

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/asteby/metacore-kernel/licensing/sigv2"
	"github.com/google/uuid"
)

func mintV2(t *testing.T, priv ed25519.PrivateKey, kid, dom string, c Claims) string {
	t.Helper()
	if c.Version == 0 {
		c.Version = 1
	}
	if c.Type == "" {
		c.Type = claimType
	}
	b, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	tok, err := sigv2.Seal(priv, kid, dom, b)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

// A v2 token signed by the current (legacy) key verifies under its derived
// kid, under the "legacy" alias, and the legacy format still verifies too.
func TestVerifyWith_AcceptsBothFormats(t *testing.T) {
	priv, pubHex := testKeypair(t)
	pub := priv.Public().(ed25519.PublicKey)
	ring, err := legacyRing(pubHex)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	for name, tok := range map[string]string{
		"legacy":      mintToken(t, priv, baseClaims()),
		"v2-kid":      mintV2(t, priv, sigv2.KeyID(pub), sigv2.DomLease, baseClaims()),
		"v2-legacyid": mintV2(t, priv, sigv2.LegacyKID, sigv2.DomLease, baseClaims()),
	} {
		c, err := VerifyWith(tok, ring, sigv2.Policy{}, now)
		if err != nil || c == nil {
			t.Errorf("%s: %v", name, err)
		}
		// the original entry point is untouched and accepts all three as well
		if _, err := Verify(tok, pubHex, now); err != nil {
			t.Errorf("%s via Verify: %v", name, err)
		}
	}
}

func TestVerifyWith_UnknownKidDomainAndDowngrade(t *testing.T) {
	priv, pubHex := testKeypair(t)
	ring, _ := legacyRing(pubHex)
	now := time.Now().UTC()
	if _, err := VerifyWith(mintV2(t, priv, "someone-else", sigv2.DomLease, baseClaims()), ring, sigv2.Policy{}, now); !errors.Is(err, ErrUnknownKID) {
		t.Errorf("unknown kid: %v", err)
	}
	if _, err := VerifyWith(mintV2(t, priv, sigv2.LegacyKID, sigv2.DomRevocations, baseClaims()), ring, sigv2.Policy{}, now); !errors.Is(err, ErrWrongDomain) {
		t.Errorf("domain confusion: %v", err)
	}
	if _, err := VerifyWith(mintToken(t, priv, baseClaims()), ring, sigv2.Policy{RequireV2: true}, now); !errors.Is(err, ErrDowngrade) {
		t.Errorf("downgrade must be refused when v2 is required: %v", err)
	}
	// wrong key under a known kid → plain bad signature, same sentinel as before
	other, _ := testKeypair(t)
	if _, err := VerifyWith(mintV2(t, other, sigv2.LegacyKID, sigv2.DomLease, baseClaims()), ring, sigv2.Policy{}, now); !errors.Is(err, ErrBadSig) {
		t.Errorf("bad sig: %v", err)
	}
}

// The service exposes the policy: RequireV2 turns a valid legacy token into
// "invalid", while a v2 token keeps the instance valid.
func TestService_RequireV2(t *testing.T) {
	priv, pubHex := testKeypair(t)
	now := time.Now().UTC()
	svc := newTestService(pubHex, true, DefaultGrace, now)
	svc.pol = sigv2.Policy{RequireV2: true}
	svc.recompute(context.Background(), persistedRow(mintToken(t, priv, baseClaims())))
	if st := svc.Current(); st.Status != "invalid" || st.Valid {
		t.Fatalf("legacy token must be refused under RequireV2: %+v", st)
	}
	svc.recompute(context.Background(), persistedRow(mintV2(t, priv, sigv2.LegacyKID, sigv2.DomLease, baseClaims())))
	if st := svc.Current(); !st.Valid {
		t.Fatalf("v2 token must be valid: %+v", st)
	}
}

// Extra keys (a key set) are consulted before the legacy key; a rotated-in
// key signs v2 tokens that verify even though it is not the legacy pubkey.
func TestService_ExtraKeysVerifyRotatedKey(t *testing.T) {
	_, pubHex := testKeypair(t)
	newPub, newPriv, _ := ed25519.GenerateKey(rand.Reader)
	extra := sigv2.NewStaticKeyring().Add("lease-2027", newPub, "lease")
	now := time.Now().UTC()
	svc := newTestService(pubHex, true, DefaultGrace, now)
	svc.extraKeys = extra
	svc.recompute(context.Background(), persistedRow(mintV2(t, newPriv, "lease-2027", sigv2.DomLease, baseClaims())))
	if st := svc.Current(); !st.Valid {
		t.Fatalf("rotated key not honoured: %+v", st)
	}
}

func TestClaims_CounterRoundTripAndLegacyBytesUnchanged(t *testing.T) {
	priv, pubHex := testKeypair(t)
	c := baseClaims()
	c.Counter = 41
	got, err := Verify(mintToken(t, priv, c), pubHex, time.Now())
	if err != nil || got.Counter != 41 {
		t.Fatalf("counter: %+v %v", got, err)
	}
	// ctr=0 must not appear on the wire (omitempty): old tokens stay byte-identical.
	b, _ := json.Marshal(baseClaims())
	if strings.Contains(string(b), `"ctr"`) {
		t.Fatal("ctr leaked into a legacy-shaped claim set")
	}
}

// renewHub answers /v1/licenses/renew with a fixed token and records the body.
func renewHub(t *testing.T, token string, body *map[string]any) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, body)
		_ = json.NewEncoder(w).Encode(map[string]string{"license_token": token})
	}))
}

func renewService(t *testing.T, srv *httptest.Server, pubHex string, tok string, ctr bool) *Service {
	_, ipriv, _ := ed25519.GenerateKey(rand.Reader)
	st := NewMemoryStore()
	_ = st.Save(context.Background(), &Record{Token: tok})
	s := newStoreService(st, pubHex, false, time.Now().UTC())
	s.http = srv.Client()
	s.hubBase = srv.URL
	s.instanceID = uuid.New()
	s.instancePriv = ipriv
	s.leaseCounter = ctr
	return s
}

// The renew body only gains lease_ctr when the flag is on (opt-in, additive).
func TestRenew_LeaseCounterIsOptIn(t *testing.T) {
	priv, pubHex := testKeypair(t)
	c := baseClaims()
	c.Counter = 7
	tok := mintV2(t, priv, sigv2.LegacyKID, sigv2.DomLease, c)

	var off map[string]any
	srv := renewHub(t, tok, &off)
	defer srv.Close()
	if _, err := renewService(t, srv, pubHex, tok, false).renew(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, has := off["lease_ctr"]; has {
		t.Fatalf("flag off: body must not carry lease_ctr, got %v", off)
	}

	var on map[string]any
	srv2 := renewHub(t, tok, &on)
	defer srv2.Close()
	if _, err := renewService(t, srv2, pubHex, tok, true).renew(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f, _ := on["token_formats"].([]any); len(f) != 1 || f[0] != "asteby1" {
		t.Fatalf("renew must announce the v2 envelope via token_formats, got %v", on)
	}
	if v, ok := on["lease_ctr"].(float64); !ok || v != 7 {
		t.Fatalf("flag on: lease_ctr must echo the signed counter (7), got %v", on)
	}
}

// A key-set rotation can actually retire the hub's legacy key.
func TestService_RetireLegacyKeyStopsTrustingHubKey(t *testing.T) {
	priv, pubHex := testKeypair(t)
	newPub, newPriv, _ := ed25519.GenerateKey(rand.Reader)
	extra := sigv2.NewStaticKeyring().Add("lease-2027", newPub, "lease")
	now := time.Now().UTC()
	oldTok := mintV2(t, priv, sigv2.LegacyKID, sigv2.DomLease, baseClaims())
	newTok := mintV2(t, newPriv, "lease-2027", sigv2.DomLease, baseClaims())

	svc := newTestService(pubHex, true, DefaultGrace, now)
	svc.extraKeys = extra
	svc.recompute(context.Background(), persistedRow(oldTok))
	if st := svc.Current(); !st.Valid {
		t.Fatalf("without retirement the legacy key keeps verifying: %+v", st)
	}
	svc.retireLegacy = true
	svc.recompute(context.Background(), persistedRow(oldTok))
	if st := svc.Current(); st.Valid {
		t.Fatalf("retired legacy key still verifies: %+v", st)
	}
	svc.recompute(context.Background(), persistedRow(newTok))
	if st := svc.Current(); !st.Valid {
		t.Fatalf("key-set key must keep verifying after retirement: %+v", st)
	}
}
