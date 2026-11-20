package signedenvelope

// Vectors derived from the REAL signing code of the deployed issuers, not from
// this package's own assumptions. Each helper mirrors, line for line, the
// signer named in its comment (read at ops 6291b051 / hub 8160f29) and signs
// with a throwaway key. Where the kernel already owns a real verifier
// (security.VerifyBundle, licensing.Verify) it is used as an oracle, so the
// vector is proven against production code, not only against this package.

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/asteby/metacore-kernel/bundle"
	"github.com/asteby/metacore-kernel/licensing"
	"github.com/asteby/metacore-kernel/manifest"
	"github.com/asteby/metacore-kernel/security"
	"github.com/google/uuid"
)

var b64u = base64.RawURLEncoding

func realVerifier(t *testing.T, p Policy, purposes ...Purpose) *Verifier {
	return ver(t, p, goldenKey(t, "marketplace", purposes...))
}

// hub internal/license/instancelicense.go IssueInstanceLicenseOpts (legacy):
// payload = json.Marshal(claims); token = b64u(payload) "." b64u(Sign(payload)).
func hubInstanceToken(priv ed25519.PrivateKey, c licensing.Claims) string {
	payload, _ := json.Marshal(c)
	return b64u.EncodeToString(payload) + "." + b64u.EncodeToString(ed25519.Sign(priv, payload))
}

func TestRealLicenceInstance(t *testing.T) {
	priv := goldenPriv()
	c := licensing.Claims{Version: 1, Type: "instance", LicenseID: uuid.MustParse("11111111-1111-1111-1111-111111111111"), CustomerID: uuid.MustParse("22222222-2222-2222-2222-222222222222"), IssuedAt: now.Add(-time.Hour).UTC(), ExpiresAt: now.Add(24 * time.Hour).UTC(), GraceDays: 7}
	tok := hubInstanceToken(priv, c)
	// Oracle: the kernel's real licence verifier accepts it.
	if _, err := licensing.Verify(tok, gPub, now); err != nil {
		t.Fatalf("oracle: %v", err)
	}
	// Hub seals lease envelopes with DomLease (instancelicense.go:395).
	p := Policy{Window: RFC3339Window("iat", "exp", true)}
	v := ver(t, p, goldenKey(t, "hub", PurposeLease))
	r, err := v.Open(PurposeLease, tok)
	if err != nil || !r.Legacy {
		t.Fatalf("legacy instance licence: %+v %v", r, err)
	}
	if r.Window.Expires == nil || !r.Window.Expires.Equal(c.ExpiresAt) {
		t.Fatalf("window not read from RFC3339 iat/exp: %+v", r.Window)
	}
	// Native lease envelope (IssueOptions.Envelope): same payload.
	payload, _ := json.Marshal(c)
	env, _ := Seal(priv, "hub", PurposeLease, payload)
	if _, err := v.Open(PurposeLease, env); err != nil {
		t.Fatalf("native lease: %v", err)
	}
	// Expired licence: error AND authenticated payload (grace computation).
	late := ver(t, p, goldenKey(t, "hub", PurposeLease)).WithClock(func() time.Time { return now.Add(48 * time.Hour) })
	r, err = late.Open(PurposeLease, tok)
	if !errors.Is(err, ErrExpired) || r == nil {
		t.Fatalf("expired: %+v %v", r, err)
	}
	var got licensing.Claims
	if json.Unmarshal(r.Payload, &got) != nil || got.GraceDays != 7 {
		t.Fatalf("payload for grace lost: %s", r.Payload)
	}
	// A payload that lost its expiry must NOT become "never expires".
	noExp, _ := json.Marshal(map[string]any{"v": 1, "iat": now})
	tok2 := b64u.EncodeToString(noExp) + "." + b64u.EncodeToString(ed25519.Sign(priv, noExp))
	if _, err := v.Open(PurposeLease, tok2); !errors.Is(err, ErrMalformed) {
		t.Fatalf("missing exp: %v", err)
	}
	// ... and a zero time is not a bound either.
	zero, _ := json.Marshal(licensing.Claims{Version: 1, IssuedAt: now})
	tok3 := b64u.EncodeToString(zero) + "." + b64u.EncodeToString(ed25519.Sign(priv, zero))
	if _, err := v.Open(PurposeLease, tok3); !errors.Is(err, ErrMalformed) {
		t.Fatalf("zero exp: %v", err)
	}
}

// ops services/applicense: Payload with issued_at/expires_at (time.Time).
func TestRealApplicenseRFC3339(t *testing.T) {
	type payload struct {
		V         int       `json:"v"`
		Vertical  string    `json:"vertical"`
		AccountID string    `json:"account_id"`
		Email     string    `json:"email"`
		Kind      string    `json:"kind"`
		IssuedAt  time.Time `json:"issued_at"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	p := payload{1, "pitsline", "a1", "x@y.z", "annual", now.Add(-time.Hour).UTC(), now.Add(time.Hour).UTC()}
	raw, _ := json.Marshal(p)
	tok := b64u.EncodeToString(raw) + "." + b64u.EncodeToString(ed25519.Sign(goldenPriv(), raw))
	v := ver(t, Policy{Window: RFC3339Window("issued_at", "expires_at", true)}, goldenKey(t, "app", PurposeGrant))
	if _, err := v.Open(PurposeGrant, tok); err != nil {
		t.Fatal(err)
	}
	// The unix-seconds window would have rejected every real licence.
	bad := ver(t, Policy{Window: UnixWindow("issued_at", "expires_at", true)}, goldenKey(t, "app", PurposeGrant))
	if _, err := bad.Open(PurposeGrant, tok); !errors.Is(err, ErrMalformed) {
		t.Fatalf("unix window on RFC3339 payload must be ErrMalformed, got %v", err)
	}
	// not yet valid keeps the payload too.
	early := ver(t, Policy{Window: RFC3339Window("issued_at", "expires_at", true)}, goldenKey(t, "app", PurposeGrant)).
		WithClock(func() time.Time { return now.Add(-2 * time.Hour) })
	if r, err := early.Open(PurposeGrant, tok); !errors.Is(err, ErrNotYetValid) || r == nil {
		t.Fatalf("early: %+v %v", r, err)
	}
}

// kernel security.VerifyBundle is the real bundle verifier; hub signer.Sign
// signs digest[:] (raw 32 bytes). Both agree with SchemeSHA256Raw.
func TestRealBundleRawDigest(t *testing.T) {
	raw := []byte("tarball-bytes")
	sum := sha256.Sum256(raw)
	sig := ed25519.Sign(goldenPriv(), sum[:])
	b := &bundle.Bundle{Raw: raw, Manifest: manifest.Manifest{Signature: &manifest.Signature{Algorithm: "ed25519", Digest: hex.EncodeToString(sum[:]), Value: hex.EncodeToString(sig)}}}
	if err := security.VerifyBundle(b, []ed25519.PublicKey{mustHex(t, gPub)}); err != nil {
		t.Fatalf("oracle: %v", err)
	}
	v := realVerifier(t, Policy{}, PurposeBundle)
	if _, err := v.VerifyLegacy(PurposeBundle, SchemeSHA256Raw, raw, sig); err != nil {
		t.Fatal(err)
	}
	// ops hub_orders.go:179 / kernel_marketplace_edge.go:59 (digest as input).
	if _, err := v.VerifyLegacy(PurposeBundle, SchemeMessage, sum[:], sig); err != nil {
		t.Fatal(err)
	}
}

// ops asteby-bundle / addonbundle ComputeBundleDigest + Verify(pub, []byte(hex), sig).
func TestRealBundleHexDirDigest(t *testing.T) {
	files := map[string]string{"manifest.json": "{}", "backend/a.wasm": "wasm"}
	names := []string{"backend/a.wasm", "manifest.json"} // sorted, as the real code
	final := sha256.New()
	for _, n := range names {
		h := sha256.Sum256([]byte(files[n]))
		final.Write([]byte(n))
		final.Write([]byte{0})
		final.Write([]byte(hex.EncodeToString(h[:])))
		final.Write([]byte{'\n'})
	}
	digest := hex.EncodeToString(final.Sum(nil))
	sig := ed25519.Sign(goldenPriv(), []byte(digest))
	if _, err := realVerifier(t, Policy{}, PurposeBundle).VerifyLegacy(PurposeBundle, SchemeMessage, []byte(digest), sig); err != nil {
		t.Fatal(err)
	}
}

// ops cmd/appliance/selfupdate_apply.go (raw digest) and asteby-update (hex
// string of sha256 of the binary).
func TestRealRelease(t *testing.T) {
	bin := []byte("binary")
	sum := sha256.Sum256(bin)
	v := realVerifier(t, Policy{}, PurposeRelease)
	if _, err := v.VerifyLegacy(PurposeRelease, SchemeMessage, sum[:], ed25519.Sign(goldenPriv(), sum[:])); err != nil {
		t.Fatalf("appliance raw: %v", err)
	}
	if _, err := v.VerifyLegacy(PurposeRelease, SchemeSHA256Hex, bin, ed25519.Sign(goldenPriv(), []byte(hex.EncodeToString(sum[:])))); err != nil {
		t.Fatalf("asteby-update hex: %v", err)
	}
	// ops edge/update/update.go SigningMessage.
	msg := fmt.Sprintf("%s\nversion=%s\nbuild_unix=%d\nplatform=%s\nfile=%s\nsha256=%s\nsize=%d\n",
		"ops-edge-agent/update/v1", "1.2.3", int64(1700000000), "linux-amd64", "agent", hex.EncodeToString(sum[:]), 6)
	ve := realVerifier(t, Policy{}, PurposeEdgeUpdate)
	if _, err := ve.VerifyLegacy(PurposeEdgeUpdate, SchemeMessage, []byte(msg), ed25519.Sign(goldenPriv(), []byte(msg))); err != nil {
		t.Fatalf("edge manifest: %v", err)
	}
}

// hub orders_sign.go orderDomain / ops hubordersclient/verify.go:177.
func TestRealOrder(t *testing.T) {
	env := []byte(`{"schema":"asteby.order/v1"}`)
	sig := ed25519.Sign(goldenPriv(), append([]byte("asteby-order-v1\x00"), env...))
	if _, err := realVerifier(t, Policy{}, PurposeOrder).VerifyLegacy(PurposeOrder, SchemePrefixed("asteby-order-v1\x00"), env, sig); err != nil {
		t.Fatal(err)
	}
}

// hub activation_links.go:1459 signs domain+"\n"+ENC where ENC is the base64
// STRING of the document; the verifier input is that string, not the decoded doc.
func TestRealBootstrap(t *testing.T) {
	enc := base64.StdEncoding.EncodeToString([]byte(`{"hub":"x"}`))
	sig := ed25519.Sign(goldenPriv(), []byte("asteby/act-bootstrap/v1"+"\n"+enc))
	v := realVerifier(t, Policy{}, PurposeBootstrap)
	if _, err := v.VerifyLegacy(PurposeBootstrap, SchemePrefixed("asteby/act-bootstrap/v1\n"), []byte(enc), sig); err != nil {
		t.Fatal(err)
	}
	// Feeding the decoded document must fail.
	if _, err := v.VerifyLegacy(PurposeBootstrap, SchemePrefixed("asteby/act-bootstrap/v1\n"), []byte(`{"hub":"x"}`), sig); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("decoded doc verified: %v", err)
	}
}

// ops services/brandpack/brandpack.go: Domain = "metacore-brand-pack/v1\n",
// signature over sha256(Domain || pack).
func TestRealBrandPack(t *testing.T) {
	const domain = "metacore-brand-pack/v1\n"
	pack := []byte(`{"brand":"x"}`)
	h := sha256.New()
	h.Write([]byte(domain))
	h.Write(pack)
	sig := ed25519.Sign(goldenPriv(), h.Sum(nil))
	if _, err := realVerifier(t, Policy{}, PurposeBrandPack).VerifyLegacy(PurposeBrandPack, SchemePrefixedSHA256(domain), pack, sig); err != nil {
		t.Fatal(err)
	}
}

// A legacy token has no domain on the wire. With ONE key declared for both
// purposes a licence token verifies as an enter token: inherent to the legacy
// format and the reason the plan demands distinct keys per purpose before the
// migration. Scoping the keys apart stops it.
func TestLegacyTokenCrossPurposeSharedKey(t *testing.T) {
	tok := gTok2
	shared := ver(t, Policy{}, goldenKey(t, "k", PurposeGrant, PurposeEnter))
	if _, err := shared.Open(PurposeEnter, tok); err != nil {
		t.Fatalf("documented legacy limitation changed: %v", err)
	}
	enterOnly := ver(t, Policy{}, goldenKey(t, "k", PurposeEnter))
	if _, err := enterOnly.Open(PurposeGrant, tok); !errors.Is(err, ErrNoKeys) {
		t.Fatalf("scoped key must not verify grant: %v", err)
	}
	native := ver(t, Policy{RequireNative: true}, goldenKey(t, "k", PurposeGrant, PurposeEnter))
	if _, err := native.Open(PurposeEnter, tok); !errors.Is(err, ErrDowngrade) {
		t.Fatalf("RequireNative must close it: %v", err)
	}
}

func TestLeewayValidation(t *testing.T) {
	ks := keySet(t, goldenKey(t, "k", PurposeGrant))
	if _, err := NewVerifier(ks, Policy{Leeway: -time.Second}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("negative leeway: %v", err)
	}
	if _, err := NewVerifier(nil, Policy{}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("nil keys: %v", err)
	}
	// Leeway applies to key validity too.
	exp := now.Add(-30 * time.Second)
	k := goldenKey(t, "k1", PurposeGrant)
	k.NotAfter = &exp
	if _, err := ver(t, Policy{}, k).Open(PurposeGrant, gNative); !errors.Is(err, ErrKeyNotActive) {
		t.Fatalf("no leeway: %v", err)
	}
	if _, err := ver(t, Policy{Leeway: time.Minute}, k).Open(PurposeGrant, gNative); err != nil {
		t.Fatalf("leeway on key window: %v", err)
	}
	// Revoked stays revoked regardless of leeway.
	k.Revoked = true
	if _, err := ver(t, Policy{Leeway: time.Hour}, k).Open(PurposeGrant, gNative); !errors.Is(err, ErrKeyNotActive) {
		t.Fatalf("revoked + leeway: %v", err)
	}
}

func TestKeySetIsDeeplyImmutable(t *testing.T) {
	nb := now.Add(-time.Hour)
	purposes := []Purpose{PurposeGrant}
	pub := append(ed25519.PublicKey(nil), mustHex(t, gPub)...)
	k := Key{ID: "k1", Public: pub, Purposes: purposes, NotBefore: &nb}
	v := ver(t, Policy{}, k)
	// Mutate every alias the caller still holds.
	nb = now.Add(24 * time.Hour)
	purposes[0] = PurposeBundle
	pub[0] ^= 0xff
	if _, err := v.Open(PurposeGrant, gNative); err != nil {
		t.Fatalf("set changed through caller aliases: %v", err)
	}
}

// Hot revocation: SetKeys swaps atomically while verifications run (-race).
func TestSetKeysConcurrent(t *testing.T) {
	good := keySet(t, goldenKey(t, "k1", PurposeGrant))
	rev := goldenKey(t, "k1", PurposeGrant)
	rev.Revoked = true
	revoked := keySet(t, rev)
	v := ver(t, Policy{}, goldenKey(t, "k1", PurposeGrant))
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 300; j++ {
				_, err := v.Open(PurposeGrant, gNative)
				if err != nil && !errors.Is(err, ErrKeyNotActive) {
					t.Errorf("unexpected: %v", err)
					return
				}
			}
		}()
	}
	for j := 0; j < 300; j++ {
		if j%2 == 0 {
			v.SetKeys(revoked)
		} else {
			v.SetKeys(good)
		}
	}
	wg.Wait()
	v.SetKeys(revoked)
	if _, err := v.Open(PurposeGrant, gNative); !errors.Is(err, ErrKeyNotActive) {
		t.Fatalf("revocation not effective: %v", err)
	}
	v.SetKeys(good)
	if _, err := v.Open(PurposeGrant, gNative); err != nil {
		t.Fatal(err)
	}
}

func TestWindowRequiredAndOptional(t *testing.T) {
	f := RFC3339Window("nbf", "exp", false)
	w, err := f([]byte(`{}`))
	if err != nil || w.Expires != nil {
		t.Fatalf("optional absent: %+v %v", w, err)
	}
	if _, err := RFC3339Window("nbf", "exp", true)([]byte(`{"nbf":"2026-01-01T00:00:00Z"}`)); !errors.Is(err, ErrMalformed) {
		t.Fatalf("required exp absent: %v", err)
	}
	if _, err := RFC3339Window("", "exp", true)([]byte(`{"exp":"yesterday"}`)); !errors.Is(err, ErrMalformed) {
		t.Fatalf("bad time: %v", err)
	}
	if _, err := UnixWindow("", "exp", true)([]byte(`{"exp":null}`)); !errors.Is(err, ErrMalformed) {
		t.Fatalf("null required: %v", err)
	}
	if _, err := UnixWindow("", "exp", true)([]byte(`[1]`)); !errors.Is(err, ErrMalformed) {
		t.Fatalf("non-object: %v", err)
	}
}
