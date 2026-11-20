package signedenvelope

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"
)

// Golden vectors: ed25519 is deterministic, so these pin the deployed wire
// formats byte for byte. If one fails, a deployed format changed: STOP.
const (
	gPub    = "cd858c9ced569404f7f21f110619170619db375d9fca60d395a993c4b9fdb121"
	gRaw    = "30bd424515fb972995b1fc57e07bf0b45ec81a8ae62c3735e1168a946e2e6a6033b49e01a08085a9cc6b272dfd800d4c2a6066e67c50f480ccde354d27be830f"
	gHex    = "59254a93ff8aba1679e520a81aad2d42ce67be023565654fee772280fabf486ea6a798b06c898d8514778409dfe0e0be18d3c6b6a4bf23383b0e9bb2a8209e04"
	gOrder  = "cc25d4986231bd14bd0314a35c2023871fd85f0f53759c2ee6dc9106a1e85180841019ad588bd79f4acee457394832a0dda42028a32b73074693ebaf3ada9c02"
	gBrand  = "f74318960fce50336d44ce762b03ad187bb0c9457bcf19832f15e5a0f8f6bd7cc8ff93a1f78166080c66c310ad01f9e603fbc0ad2050efaa849a31c0fe1cf103"
	gTok2   = "eyJ2IjoxLCJleHAiOjIwMDAwMDAwMDB9.oOWyOLhKd53Q0CFZVr7XoF8hdLXyKZaiOfsluwKb6YZ52thos-fqwVIdrOBZhAGaNDubDqU3TyYr73GIjXLEDw"
	gNative = "asteby1.eyJraWQiOiJrMSIsImRvbSI6ImFzdGVieS9ncmFudC92MiIsImFsZyI6IkVkMjU1MTkifQ.eyJ2IjoxLCJleHAiOjIwMDAwMDAwMDB9.6f-8Wf1Qao-vJEu_dFMh1J2shZnqSSw4GYi9GflQAl_DF_xVaRdDQT3NoV9g-KZ4LEhzYPdU7HDgNSOtEnxGCA"
	gClaims = `{"v":1,"exp":2000000000}`
)

var now = time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func keySet(t *testing.T, ks ...Key) *KeySet {
	t.Helper()
	s, err := NewKeySet(ks...)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func goldenKey(t *testing.T, id string, purposes ...Purpose) Key {
	return Key{ID: id, Public: mustHex(t, gPub), Purposes: purposes}
}

func ver(t *testing.T, p Policy, ks ...Key) *Verifier {
	return &Verifier{Keys: keySet(t, ks...), Policy: p, Now: func() time.Time { return now }}
}

func TestGoldenNative(t *testing.T) {
	v := ver(t, Policy{}, goldenKey(t, "k1", PurposeGrant))
	r, err := v.Open(PurposeGrant, gNative)
	if err != nil || r.Legacy || r.KeyID != "k1" || string(r.Payload) != gClaims {
		t.Fatalf("native golden: %+v %v", r, err)
	}
}

func TestGoldenLegacySchemes(t *testing.T) {
	content := []byte("golden-bundle-bytes")
	digest := sha256.Sum256(content)
	v := ver(t, Policy{}, goldenKey(t, "m", PurposeBundle, PurposeOrder, PurposeBrandPack))
	cases := []struct {
		name  string
		p     Purpose
		s     Scheme
		in    []byte
		sigHx string
	}{
		{"digest raw over content", PurposeBundle, SchemeSHA256Raw, content, gRaw},
		{"digest raw given digest", PurposeBundle, SchemeMessage, digest[:], gRaw},
		{"digest hex over content", PurposeBundle, SchemeSHA256Hex, content, gHex},
		{"digest hex given string", PurposeBundle, SchemeMessage, []byte(hex.EncodeToString(digest[:])), gHex},
		{"order prefix", PurposeOrder, SchemePrefixed("asteby-order-v1\x00"), []byte(`{"o":1}`), gOrder},
		{"brand prefixed hash", PurposeBrandPack, SchemePrefixedSHA256("asteby-brand-v1\x00"), []byte("pack"), gBrand},
	}
	for _, c := range cases {
		r, err := v.VerifyLegacy(c.p, c.s, c.in, mustHex(t, c.sigHx))
		if err != nil || !r.Legacy || r.KeyID != "m" {
			t.Errorf("%s: %+v %v", c.name, r, err)
		}
	}
}

func TestGoldenLegacyToken(t *testing.T) {
	v := ver(t, Policy{}, goldenKey(t, "hub", PurposeGrant))
	r, err := v.Open(PurposeGrant, gTok2)
	if err != nil || !r.Legacy || string(r.Payload) != gClaims || r.KeyID != "hub" {
		t.Fatalf("%+v %v", r, err)
	}
}

// Attack: a signature minted for one purpose must not verify as another.
func TestCrossPurposeReplay(t *testing.T) {
	// Native: token sealed for grant, presented as bundle, even though the key
	// is allowed to sign both.
	v := ver(t, Policy{}, goldenKey(t, "k1", PurposeGrant, PurposeBundle))
	if _, err := v.Open(PurposeBundle, gNative); !errors.Is(err, ErrWrongPurpose) {
		t.Fatalf("want ErrWrongPurpose, got %v", err)
	}
	// Rewriting the header's dom to the wanted purpose breaks the signature.
	forged := strings.Replace(gNative, "eyJraWQiOiJrMSIsImRvbSI6ImFzdGVieS9ncmFudC92MiIsImFsZyI6IkVkMjU1MTkifQ", b64(`{"kid":"k1","dom":"asteby/bundle/v2","alg":"Ed25519"}`), 1)
	if _, err := v.Open(PurposeBundle, forged); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("forged dom: want ErrBadSignature, got %v", err)
	}
	// Legacy: key scoping. The key is only declared for grant.
	vg := ver(t, Policy{}, goldenKey(t, "k1", PurposeGrant))
	if _, err := vg.VerifyLegacy(PurposeBundle, SchemeMessage, mustDigest(), mustHex(t, gRaw)); !errors.Is(err, ErrNoKeys) {
		t.Fatalf("legacy cross-purpose: want ErrNoKeys, got %v", err)
	}
	// Native key restricted to another purpose.
	vb := ver(t, Policy{}, goldenKey(t, "k1", PurposeBundle))
	if _, err := vb.Open(PurposeGrant, gNative); !errors.Is(err, ErrKeyPurpose) {
		t.Fatalf("want ErrKeyPurpose, got %v", err)
	}
}

func mustDigest() []byte { d := sha256.Sum256([]byte("golden-bundle-bytes")); return d[:] }

func TestTruncatedAndMalformed(t *testing.T) {
	v := ver(t, Policy{}, goldenKey(t, "k1", PurposeGrant, PurposeBundle))
	sig := mustHex(t, gRaw)
	for _, n := range []int{0, 1, 32, 63} {
		if _, err := v.VerifyLegacy(PurposeBundle, SchemeMessage, mustDigest(), sig[:n]); !errors.Is(err, ErrMalformed) {
			t.Errorf("sig truncated to %d: want ErrMalformed, got %v", n, err)
		}
	}
	if _, err := v.VerifyLegacy(PurposeBundle, SchemeMessage, mustDigest(), append(append([]byte{}, sig...), 0)); !errors.Is(err, ErrMalformed) {
		t.Errorf("sig with trailing byte: %v", err)
	}
	for _, tok := range []string{"", "asteby1.", "asteby1.a.b", "asteby1.a.b.c.d", gNative[:len(gNative)-8], gNative + "AAAA", "x", "a.b.c", gTok2[:len(gTok2)-4]} {
		if _, err := v.Open(PurposeGrant, tok); err == nil {
			t.Errorf("token %q verified", tok)
		}
	}
	// Flip a payload byte: bad signature.
	parts := strings.Split(gNative, ".")
	parts[2] = b64(`{"v":1,"exp":9999999999}`)
	if _, err := v.Open(PurposeGrant, strings.Join(parts, ".")); !errors.Is(err, ErrBadSignature) {
		t.Errorf("tampered payload: %v", err)
	}
	// Wrong-bytes signature of right length.
	bad := append([]byte{}, sig...)
	bad[0] ^= 1
	if _, err := v.VerifyLegacy(PurposeBundle, SchemeMessage, mustDigest(), bad); !errors.Is(err, ErrBadSignature) {
		t.Errorf("flipped sig: %v", err)
	}
}

func TestUnknownKey(t *testing.T) {
	other, _, _ := ed25519.GenerateKey(nil)
	v := ver(t, Policy{}, Key{ID: "other", Public: other, Purposes: []Purpose{PurposeGrant}})
	if _, err := v.Open(PurposeGrant, gNative); !errors.Is(err, ErrUnknownKey) {
		t.Fatalf("unknown kid: %v", err)
	}
	// Right kid, wrong key material.
	v2 := ver(t, Policy{}, Key{ID: "k1", Public: other, Purposes: []Purpose{PurposeGrant}})
	if _, err := v2.Open(PurposeGrant, gNative); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("wrong key: %v", err)
	}
	if _, err := v2.VerifyLegacy(PurposeGrant, SchemeMessage, mustDigest(), mustHex(t, gRaw)); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("legacy wrong key: %v", err)
	}
}

func TestRotationAndKeyWindows(t *testing.T) {
	other, _, _ := ed25519.GenerateKey(nil)
	past := now.Add(-time.Hour)
	future := now.Add(time.Hour)
	// Two live keys: verification succeeds through the second.
	v := ver(t, Policy{},
		Key{ID: "new", Public: other, Purposes: []Purpose{PurposeBundle}},
		goldenKey(t, "old", PurposeBundle))
	if r, err := v.VerifyLegacy(PurposeBundle, SchemeMessage, mustDigest(), mustHex(t, gRaw)); err != nil || r.KeyID != "old" {
		t.Fatalf("rotation: %+v %v", r, err)
	}
	// Expired key.
	k := goldenKey(t, "k1", PurposeGrant)
	k.NotAfter = &past
	if _, err := ver(t, Policy{}, k).Open(PurposeGrant, gNative); !errors.Is(err, ErrKeyNotActive) {
		t.Fatalf("expired key: %v", err)
	}
	// Not-yet-active key.
	k.NotAfter, k.NotBefore = nil, &future
	if _, err := ver(t, Policy{}, k).Open(PurposeGrant, gNative); !errors.Is(err, ErrKeyNotActive) {
		t.Fatalf("future key: %v", err)
	}
	// Revoked key.
	k.NotBefore, k.Revoked = nil, true
	if _, err := ver(t, Policy{}, k).Open(PurposeGrant, gNative); !errors.Is(err, ErrKeyNotActive) {
		t.Fatalf("revoked key: %v", err)
	}
	if _, err := ver(t, Policy{}, k).VerifyLegacy(PurposeGrant, SchemeMessage, mustDigest(), mustHex(t, gRaw)); !errors.Is(err, ErrKeyNotActive) {
		t.Fatalf("revoked legacy: %v", err)
	}
}

func TestDowngradeProtection(t *testing.T) {
	v := ver(t, Policy{RequireNative: true}, goldenKey(t, "k1", PurposeGrant, PurposeBundle))
	if _, err := v.Open(PurposeGrant, gTok2); !errors.Is(err, ErrDowngrade) {
		t.Fatalf("legacy token: %v", err)
	}
	if _, err := v.VerifyLegacy(PurposeBundle, SchemeMessage, mustDigest(), mustHex(t, gRaw)); !errors.Is(err, ErrDowngrade) {
		t.Fatalf("legacy scheme: %v", err)
	}
	if _, err := v.Open(PurposeGrant, gNative); err != nil {
		t.Fatalf("native must still verify: %v", err)
	}
}

func TestPayloadWindow(t *testing.T) {
	p := Policy{Window: UnixWindow("nbf", "exp")}
	k := goldenKey(t, "k1", PurposeGrant)
	// gClaims expires 2033: valid in 2026.
	if r, err := ver(t, p, k).Open(PurposeGrant, gNative); err != nil || r.Window.Expires == nil {
		t.Fatalf("valid window: %+v %v", r, err)
	}
	late := ver(t, p, k)
	late.Now = func() time.Time { return time.Unix(2000000000, 0) }
	if _, err := late.Open(PurposeGrant, gNative); !errors.Is(err, ErrExpired) {
		t.Fatalf("expired: %v", err)
	}
	late.Policy.Leeway = time.Minute
	if _, err := late.Open(PurposeGrant, gNative); err != nil {
		t.Fatalf("leeway: %v", err)
	}
	// not-before in the future (own-signed token).
	priv := goldenPriv()
	tok, _ := Seal(priv, "k1", PurposeGrant, []byte(`{"nbf":4102444800}`))
	if _, err := ver(t, p, k).Open(PurposeGrant, tok); !errors.Is(err, ErrNotYetValid) {
		t.Fatalf("nbf: %v", err)
	}
	// Garbage window fields are rejected, not ignored.
	tok, _ = Seal(priv, "k1", PurposeGrant, []byte(`{"exp":"soon"}`))
	if _, err := ver(t, p, k).Open(PurposeGrant, tok); !errors.Is(err, ErrMalformed) {
		t.Fatalf("garbage exp: %v", err)
	}
}

func goldenPriv() ed25519.PrivateKey {
	seed := sha256.Sum256([]byte("signedenvelope-golden-seed-v1"))
	return ed25519.NewKeyFromSeed(seed[:])
}

func TestKeySetValidation(t *testing.T) {
	pub := goldenPriv().Public().(ed25519.PublicKey)
	if hex.EncodeToString(pub) != gPub {
		t.Fatal("golden seed drifted")
	}
	bad := []Key{
		{Public: pub[:10], Purposes: []Purpose{PurposeGrant}},
		{Public: pub},                              // no purposes: skeleton key
		{Public: pub, Purposes: []Purpose{""}},     // empty purpose
		{Public: pub, Purposes: []Purpose{"a\nb"}}, // control char
	}
	for i, k := range bad {
		if _, err := NewKeySet(k); !errors.Is(err, ErrInvalidConfig) {
			t.Errorf("case %d accepted: %v", i, err)
		}
	}
	if _, err := NewKeySet(Key{ID: "a", Public: pub, Purposes: []Purpose{PurposeGrant}}, Key{ID: "a", Public: pub, Purposes: []Purpose{PurposeGrant}}); !errors.Is(err, ErrInvalidConfig) {
		t.Errorf("duplicate id accepted")
	}
	// Empty set fails closed.
	v := &Verifier{Keys: keySet(t), Now: func() time.Time { return now }}
	if _, err := v.Open(PurposeGrant, gNative); err == nil {
		t.Error("empty key set verified")
	}
	if _, err := v.VerifyLegacy(PurposeBundle, SchemeMessage, mustDigest(), mustHex(t, gRaw)); !errors.Is(err, ErrNoKeys) {
		t.Errorf("empty set legacy: %v", err)
	}
	// Derived id round trip.
	ks := keySet(t, Key{Public: pub, Purposes: []Purpose{PurposeGrant}})
	tok, _ := Seal(goldenPriv(), ks.order[0], PurposeGrant, []byte(`{}`))
	if _, err := (&Verifier{Keys: ks, Now: func() time.Time { return now }}).Open(PurposeGrant, tok); err != nil {
		t.Errorf("derived kid: %v", err)
	}
}

func TestParseHexKeys(t *testing.T) {
	ks, err := ParseHexKeys(" "+gPub+" , ,"+gPub[:62]+"00", PurposeBundle)
	if err != nil || len(ks) != 2 {
		t.Fatalf("%v %d", err, len(ks))
	}
	if _, err := ParseHexKeys("zz", PurposeBundle); !errors.Is(err, ErrInvalidConfig) {
		t.Errorf("bad hex: %v", err)
	}
	if _, err := ParseHexKeys(gPub[:60], PurposeBundle); !errors.Is(err, ErrInvalidConfig) {
		t.Errorf("short key: %v", err)
	}
}

func TestVerifyDetached(t *testing.T) {
	pub := ed25519.PublicKey(mustHex(t, gPub))
	if err := VerifyDetached(pub, mustDigest(), mustHex(t, gRaw)); err != nil {
		t.Fatal(err)
	}
	if err := VerifyDetached(pub, []byte("other"), mustHex(t, gRaw)); !errors.Is(err, ErrBadSignature) {
		t.Error(err)
	}
	if err := VerifyDetached(pub, mustDigest(), mustHex(t, gRaw)[:63]); !errors.Is(err, ErrMalformed) {
		t.Error(err)
	}
	if err := VerifyDetached(pub[:5], nil, mustHex(t, gRaw)); !errors.Is(err, ErrMalformed) {
		t.Error(err)
	}
}

func TestLegacyTokenWrongKeyAmongMany(t *testing.T) {
	other, _, _ := ed25519.GenerateKey(nil)
	v := ver(t, Policy{}, Key{ID: "x", Public: other, Purposes: []Purpose{PurposeGrant}}, goldenKey(t, "hub", PurposeGrant))
	r, err := v.Open(PurposeGrant, gTok2)
	if err != nil || r.KeyID != "hub" {
		t.Fatalf("%+v %v", r, err)
	}
}

func b64(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
