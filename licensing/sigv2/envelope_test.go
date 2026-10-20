package sigv2

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"
)

func gen(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return pub, priv
}

var t0 = time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)

func TestEnvelope_RoundTrip(t *testing.T) {
	pub, priv := gen(t)
	kid := KeyID(pub)
	ring := NewStaticKeyring().Add(kid, pub)
	tok, err := Seal(priv, kid, DomLease, []byte(`{"a":1}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(tok, Prefix) {
		t.Fatalf("missing prefix: %s", tok)
	}
	o, err := Open(tok, DomLease, ring, Policy{}, t0)
	if err != nil {
		t.Fatal(err)
	}
	if string(o.Payload) != `{"a":1}` || o.KID != kid || o.Legacy {
		t.Fatalf("bad opened: %+v", o)
	}
}

func TestEnvelope_UnknownKID(t *testing.T) {
	pub, priv := gen(t)
	other, _ := gen(t)
	tok, _ := Seal(priv, "k-unknown", DomLease, []byte(`{}`))
	ring := NewStaticKeyring().Add(KeyID(other), other)
	_ = pub
	if _, err := Open(tok, DomLease, ring, Policy{}, t0); !errors.Is(err, ErrUnknownKID) {
		t.Fatalf("want ErrUnknownKID, got %v", err)
	}
}

// A signature minted for one message type must not pass as another.
func TestEnvelope_DomainConfusion(t *testing.T) {
	pub, priv := gen(t)
	kid := KeyID(pub)
	ring := NewStaticKeyring().Add(kid, pub)
	lease, _ := Seal(priv, kid, DomLease, []byte(`{}`))
	if _, err := Open(lease, DomRevocations, ring, Policy{}, t0); !errors.Is(err, ErrWrongDomain) {
		t.Fatalf("lease token accepted as revocations: %v", err)
	}
	// Re-labelling the header to the wanted domain must break the signature,
	// because the domain is part of the signed bytes.
	parts := strings.Split(strings.TrimPrefix(lease, Prefix), ".")
	forged := base64.RawURLEncoding.EncodeToString([]byte(`{"kid":"` + kid + `","dom":"` + DomRevocations + `","alg":"Ed25519"}`))
	tok := Prefix + forged + "." + parts[1] + "." + parts[2]
	if _, err := Open(tok, DomRevocations, ring, Policy{}, t0); !errors.Is(err, ErrBadSig) {
		t.Fatalf("relabelled header must fail the signature, got %v", err)
	}
}

// A key allowed only for one role cannot sign another message type.
func TestEnvelope_KeyRoleBinding(t *testing.T) {
	pub, priv := gen(t)
	kid := KeyID(pub)
	ring := NewStaticKeyring().Add(kid, pub, "release")
	tok, _ := Seal(priv, kid, DomLease, []byte(`{}`))
	if _, err := Open(tok, DomLease, ring, Policy{}, t0); !errors.Is(err, ErrKeyRole) {
		t.Fatalf("release key signed a lease: %v", err)
	}
}

func TestEnvelope_TamperedPayloadAndAlg(t *testing.T) {
	pub, priv := gen(t)
	kid := KeyID(pub)
	ring := NewStaticKeyring().Add(kid, pub)
	tok, _ := Seal(priv, kid, DomLease, []byte(`{"plan":"basic"}`))
	parts := strings.Split(strings.TrimPrefix(tok, Prefix), ".")
	evil := base64.RawURLEncoding.EncodeToString([]byte(`{"plan":"unlimited"}`))
	if _, err := Open(Prefix+parts[0]+"."+evil+"."+parts[2], DomLease, ring, Policy{}, t0); !errors.Is(err, ErrBadSig) {
		t.Fatalf("tampered payload accepted: %v", err)
	}
	none := base64.RawURLEncoding.EncodeToString([]byte(`{"kid":"` + kid + `","dom":"` + DomLease + `","alg":"none"}`))
	if _, err := Open(Prefix+none+"."+parts[1]+"."+parts[2], DomLease, ring, Policy{}, t0); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("alg none accepted: %v", err)
	}
}

func TestEnvelope_Malformed(t *testing.T) {
	ring := NewStaticKeyring()
	for _, tok := range []string{"", "asteby1.a.b", "asteby1.!!.!!.!!", "garbage", "a.b.c"} {
		if _, err := Open(tok, DomLease, ring, Policy{}, t0); !errors.Is(err, ErrMalformed) {
			t.Errorf("%q: want ErrMalformed, got %v", tok, err)
		}
	}
}

func legacyToken(priv ed25519.PrivateKey, payload []byte) string {
	e := base64.RawURLEncoding
	return e.EncodeToString(payload) + "." + e.EncodeToString(ed25519.Sign(priv, payload))
}

func TestOpen_AcceptsLegacyAndV2_WithSameKey(t *testing.T) {
	pub, priv := gen(t)
	ring := NewStaticKeyring().AddLegacy(pub)
	o, err := Open(legacyToken(priv, []byte(`{"x":1}`)), DomLease, ring, Policy{}, t0)
	if err != nil || !o.Legacy || o.KID != LegacyKID {
		t.Fatalf("legacy: %+v %v", o, err)
	}
	// the same key under its derived kid, and under the "legacy" alias
	for _, kid := range []string{KeyID(pub), LegacyKID} {
		tok, _ := Seal(priv, kid, DomLease, []byte(`{"x":1}`))
		o, err = Open(tok, DomLease, ring, Policy{}, t0)
		if err != nil || o.Legacy {
			t.Fatalf("v2 kid %s: %+v %v", kid, o, err)
		}
	}
}

// With RequireV2 a key-less token is refused even though its signature is valid.
func TestOpen_DowngradeRefusedWhenV2Required(t *testing.T) {
	pub, priv := gen(t)
	ring := NewStaticKeyring().AddLegacy(pub)
	_, err := Open(legacyToken(priv, []byte(`{}`)), DomLease, ring, Policy{RequireV2: true}, t0)
	if !errors.Is(err, ErrDowngrade) {
		t.Fatalf("want ErrDowngrade, got %v", err)
	}
	tok, _ := Seal(priv, KeyID(pub), DomLease, []byte(`{}`))
	if _, err := Open(tok, DomLease, ring, Policy{RequireV2: true}, t0); err != nil {
		t.Fatalf("v2 must pass with RequireV2: %v", err)
	}
}

func TestRings_FallThroughButRoleErrorIsFinal(t *testing.T) {
	pubA, _ := gen(t)
	pubB, privB := gen(t)
	a := NewStaticKeyring().Add("a", pubA)
	b := NewStaticKeyring().Add("b", pubB)
	rs := Rings{a, b}
	tok, _ := Seal(privB, "b", DomLease, []byte(`{}`))
	if _, err := Open(tok, DomLease, rs, Policy{}, t0); err != nil {
		t.Fatal(err)
	}
	c := NewStaticKeyring().Add("b", pubB, "release")
	if _, err := Open(tok, DomLease, Rings{c, b}, Policy{}, t0); !errors.Is(err, ErrKeyRole) {
		t.Fatalf("role error must not fall through: %v", err)
	}
}
