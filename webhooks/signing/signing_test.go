package signing

import (
	"errors"
	"net/http"
	"net/url"
	"testing"
	"time"
)

// Vector de prueba del contrato de 7 Leguas V2 (CONTRATO_ECOMMERCE_OPS.md §1,
// el mismo que fija ChannelSignatureTest en V2).
const (
	v2Secret  = "s3cr3t-signing-key-for-docs"
	v2TS      = "1763049600"
	v2Nonce   = "Zb3k9QpL2xYt7wQe"
	v2Path    = "/api/channels/v1/orders"
	v2Body    = `{"channel":"mercadolibre","external_reference":"MLM-2000001"}`
	v2SigPOST = "45df85bc7fd70390350fd8cd2ecf49835615e290ae57e644ec6c02a41dca8c81"
	v2SigGET  = "736fdea5c2b361ff0b6c8ef78ace5c4d3f545b415bf36b8174af35f7e0813efd"
)

var v2Now = time.Unix(1763049600, 0)

func TestCanonicalV1MatchesV2Vector(t *testing.T) {
	h, err := Sign(Spec{Profile: HMACCanonicalV1, SecretRef: "v2.sign"}, Secrets{HMACKey: v2Secret},
		Request{Method: "post", Path: v2Path, Body: []byte(v2Body)}, v2Now, v2Nonce)
	if err != nil {
		t.Fatal(err)
	}
	if h["X-7L-Signature"] != v2SigPOST {
		t.Fatalf("sig = %s, want V2 vector %s", h["X-7L-Signature"], v2SigPOST)
	}
	if h["X-7L-Timestamp"] != v2TS || h["X-7L-Nonce"] != v2Nonce {
		t.Fatalf("headers = %v", h)
	}
	if _, ok := h["Authorization"]; ok {
		t.Fatal("hmac_canonical_v1 must not emit Authorization")
	}
	// GET con cuerpo vacio.
	g, err := Sign(Spec{Profile: HMACCanonicalV1, SecretRef: "v2.sign"}, Secrets{HMACKey: v2Secret},
		Request{Method: "GET", Path: "/api/channels/v1/ping"}, v2Now, v2Nonce)
	if err != nil {
		t.Fatal(err)
	}
	if g["X-7L-Signature"] != v2SigGET {
		t.Fatalf("GET sig = %s, want %s", g["X-7L-Signature"], v2SigGET)
	}
}

func TestBearerPlusCanonical(t *testing.T) {
	spec := Spec{Profile: BearerHMACCanonicalV1, SecretRef: "v2.sign", KeyRef: "v2.key"}
	h, err := Sign(spec, Secrets{HMACKey: v2Secret, BearerToken: "chn_abc.def"},
		Request{Method: "POST", Path: v2Path, Body: []byte(v2Body)}, v2Now, v2Nonce)
	if err != nil {
		t.Fatal(err)
	}
	if h["Authorization"] != "Bearer chn_abc.def" || h["X-7L-Signature"] != v2SigPOST {
		t.Fatalf("headers = %v", h)
	}
}

func hdr(m map[string]string) http.Header {
	h := http.Header{}
	for k, v := range m {
		h.Set(k, v)
	}
	return h
}

type nonceSet map[string]bool

func (n nonceSet) CheckAndRecord(s string) error {
	if n[s] {
		return errors.New("seen")
	}
	n[s] = true
	return nil
}

func TestSignVerifyRoundTripAllProfiles(t *testing.T) {
	sec := Secrets{HMACKey: "k", BearerToken: "tok"}
	req := Request{Method: "POST", Path: "/hook", Body: []byte(`{"a":1}`)}
	for _, spec := range []Spec{
		{Profile: HMACSimple, SecretRef: "a.b"},
		{Profile: HMACSimple, SecretRef: "a.b", Header: "X-Hub-Signature-256", Prefix: "sha256="},
		{Profile: HMACCanonicalV1, SecretRef: "a.b"},
		{Profile: HMACCanonicalV1, SecretRef: "a.b", TimestampHeader: "X-T", NonceHeader: "X-N", SignatureHeader: "X-S"},
		{Profile: Bearer, KeyRef: "a.c"},
		{Profile: BearerHMACCanonicalV1, SecretRef: "a.b", KeyRef: "a.c"},
	} {
		now := time.Now()
		h, err := Sign(spec, sec, req, now, "")
		if err != nil {
			t.Fatalf("%s: %v", spec.Profile, err)
		}
		if err := Verify(spec, sec, req, hdr(h), VerifyOptions{Now: now, Nonces: nonceSet{}}); err != nil {
			t.Fatalf("%s: verify: %v", spec.Profile, err)
		}
	}
}

func TestVerifyRejections(t *testing.T) {
	spec := Spec{Profile: BearerHMACCanonicalV1, SecretRef: "a.b", KeyRef: "a.c"}
	sec := Secrets{HMACKey: "k", BearerToken: "tok"}
	req := Request{Method: "POST", Path: "/hook", Body: []byte(`{"a":1}`)}
	now := time.Unix(1_800_000_000, 0)
	good, _ := Sign(spec, sec, req, now, "")
	cases := []struct {
		name string
		mut  func(m map[string]string) (Request, VerifyOptions)
		code string
	}{
		{"body tampered", func(m map[string]string) (Request, VerifyOptions) {
			r := req
			r.Body = []byte(`{"a":2}`)
			return r, VerifyOptions{Now: now}
		}, "invalid_signature"},
		{"path tampered", func(m map[string]string) (Request, VerifyOptions) {
			r := req
			r.Path = "/other"
			return r, VerifyOptions{Now: now}
		}, "invalid_signature"},
		{"method tampered", func(m map[string]string) (Request, VerifyOptions) {
			r := req
			r.Method = "PUT"
			return r, VerifyOptions{Now: now}
		}, "invalid_signature"},
		{"wrong bearer", func(m map[string]string) (Request, VerifyOptions) {
			m["Authorization"] = "Bearer nope"
			return req, VerifyOptions{Now: now}
		}, "unauthenticated"},
		{"no bearer", func(m map[string]string) (Request, VerifyOptions) {
			delete(m, "Authorization")
			return req, VerifyOptions{Now: now}
		}, "unauthenticated"},
		{"stale +301s", func(m map[string]string) (Request, VerifyOptions) {
			return req, VerifyOptions{Now: now.Add(301 * time.Second)}
		}, "stale_timestamp"},
		{"future -301s", func(m map[string]string) (Request, VerifyOptions) {
			return req, VerifyOptions{Now: now.Add(-301 * time.Second)}
		}, "stale_timestamp"},
		{"missing sig", func(m map[string]string) (Request, VerifyOptions) {
			delete(m, "X-7L-Signature")
			return req, VerifyOptions{Now: now}
		}, "bad_request"},
		{"short nonce", func(m map[string]string) (Request, VerifyOptions) {
			m["X-7L-Nonce"] = "short"
			return req, VerifyOptions{Now: now}
		}, "bad_request"},
		{"non numeric ts", func(m map[string]string) (Request, VerifyOptions) {
			m["X-7L-Timestamp"] = "abc"
			return req, VerifyOptions{Now: now}
		}, "bad_request"},
	}
	for _, c := range cases {
		m := map[string]string{}
		for k, v := range good {
			m[k] = v
		}
		r, o := c.mut(m)
		err := Verify(spec, sec, r, hdr(m), o)
		if CodeOf(err) != c.code {
			t.Errorf("%s: code = %q (%v), want %q", c.name, CodeOf(err), err, c.code)
		}
	}
	// El limite exacto (300 s) se acepta.
	if err := Verify(spec, sec, req, hdr(good), VerifyOptions{Now: now.Add(300 * time.Second)}); err != nil {
		t.Errorf("300s boundary should pass: %v", err)
	}
}

func TestNonceReplayRejectedOnlyAfterValidSignature(t *testing.T) {
	spec := Spec{Profile: HMACCanonicalV1, SecretRef: "a.b"}
	sec := Secrets{HMACKey: "k"}
	req := Request{Method: "POST", Path: "/h", Body: []byte("x")}
	now := time.Now()
	h, _ := Sign(spec, sec, req, now, "")
	cache := nonceSet{}
	// Una firma invalida NO quema el nonce.
	bad := req
	bad.Body = []byte("y")
	if CodeOf(Verify(spec, sec, bad, hdr(h), VerifyOptions{Now: now, Nonces: cache})) != "invalid_signature" {
		t.Fatal("expected invalid_signature")
	}
	if len(cache) != 0 {
		t.Fatal("invalid signature must not record the nonce")
	}
	if err := Verify(spec, sec, req, hdr(h), VerifyOptions{Now: now, Nonces: cache}); err != nil {
		t.Fatal(err)
	}
	if CodeOf(Verify(spec, sec, req, hdr(h), VerifyOptions{Now: now, Nonces: cache})) != "nonce_reused" {
		t.Fatal("replay must be nonce_reused")
	}
}

func TestEachSignGetsFreshValidNonce(t *testing.T) {
	spec := Spec{Profile: HMACCanonicalV1, SecretRef: "a.b"}
	a, _ := Sign(spec, Secrets{HMACKey: "k"}, Request{Method: "POST", Path: "/"}, time.Now(), "")
	b, _ := Sign(spec, Secrets{HMACKey: "k"}, Request{Method: "POST", Path: "/"}, time.Now(), "")
	if a["X-7L-Nonce"] == b["X-7L-Nonce"] || !nonceRe.MatchString(a["X-7L-Nonce"]) {
		t.Fatalf("nonces %q %q", a["X-7L-Nonce"], b["X-7L-Nonce"])
	}
}

func TestSpecValidate(t *testing.T) {
	bad := []Spec{
		{Profile: "rot13"},
		{Profile: HMACSimple},                                        // sin secret_ref
		{Profile: HMACSimple, SecretRef: "has space"},                // no es una referencia
		{Profile: HMACSimple, SecretRef: "wh_5f2c9a", KeyRef: "x.y"}, // key_ref sobra
		{Profile: Bearer},                                            // sin key_ref
		{Profile: Bearer, KeyRef: "x.y", SecretRef: "a.b"},           // secret_ref sobra
		{Profile: HMACSimple, SecretRef: "a.b", Header: "Host"},      // reservada
		{Profile: HMACSimple, SecretRef: "a.b", Header: "X\r\nEvil"},
		{Profile: HMACSimple, SecretRef: "a.b", Prefix: "a\r\nb"},
		{Profile: HMACCanonicalV1, SecretRef: "a.b", Header: "X-S"}, // header solo simple
		{Profile: HMACSimple, SecretRef: "a.b", NonceHeader: "X-N"},
		{Profile: HMACCanonicalV1, SecretRef: "a.b", NonceHeader: "X-7L-Signature"}, // repetida
		{Profile: HMACCanonicalV1, SecretRef: "a.b", SignatureHeader: "Authorization"},
	}
	for i, s := range bad {
		if err := s.Validate(); err == nil {
			t.Errorf("case %d (%+v) should fail", i, s)
		}
	}
	if err := (Spec{Profile: "rot13"}).Validate(); !errors.Is(err, ErrUnknownProfile) {
		t.Errorf("want ErrUnknownProfile, got %v", err)
	}
	// Una URL/valor tipico de secreto no pasa por referencia.
	if ValidRef("sk_live_abc 123") || ValidRef("") || !ValidRef("connector.v2_signing_secret") {
		t.Error("ValidRef misbehaves")
	}
}

func TestMissingSecretsAndErrorsDoNotLeak(t *testing.T) {
	spec := Spec{Profile: BearerHMACCanonicalV1, SecretRef: "a.b", KeyRef: "a.c"}
	if _, err := Sign(spec, Secrets{HMACKey: "k"}, Request{}, time.Now(), ""); !errors.Is(err, ErrMissingSecret) {
		t.Fatalf("want ErrMissingSecret, got %v", err)
	}
	_, err := Sign(Spec{Profile: HMACCanonicalV1, SecretRef: "a.b"}, Secrets{HMACKey: "TOPSECRET"},
		Request{Method: "POST", Path: "/"}, time.Now(), "bad nonce")
	if err == nil || contains(err.Error(), "TOPSECRET") {
		t.Fatalf("err = %v", err)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func TestPathOf(t *testing.T) {
	u, _ := url.Parse("https://x.example/api/v1/orders?a=1#f")
	if PathOf(u) != "/api/v1/orders" {
		t.Fatal(PathOf(u))
	}
	u2, _ := url.Parse("https://x.example")
	if PathOf(u2) != "/" {
		t.Fatal(PathOf(u2))
	}
}
