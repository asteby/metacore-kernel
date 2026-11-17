package signing

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestVerifierRotationOverlapAndPerOrgNonces(t *testing.T) {
	orgA, orgB := uuid.New(), uuid.New()
	now := time.Unix(1_800_000_000, 0)
	spec := Spec{Profile: HMACCanonicalV1, SecretRef: "a.b"}
	v := &Verifier{
		Spec: spec, Now: func() time.Time { return now },
		Secrets: func(_ context.Context, org uuid.UUID) ([]Secrets, error) {
			switch org {
			case orgA: // rotating: old and new both valid
				return []Secrets{{HMACKey: "new"}, {HMACKey: "old"}}, nil
			case orgB:
				return []Secrets{{HMACKey: "new"}}, nil
			}
			return nil, errors.New("unknown org")
		},
	}
	req := Request{Method: "POST", Path: "/p", Body: []byte(`{"a":1}`)}
	signWith := func(key string) VerifyRequest {
		h, _ := Sign(spec, Secrets{HMACKey: key}, req, now, "")
		return VerifyRequest{Org: orgA, Method: req.Method, Path: req.Path, Header: hdr(h), Body: req.Body}
	}
	ctx := context.Background()
	if err := v.Verify(ctx, signWith("old")); err != nil {
		t.Fatalf("old secret inside the overlap window: %v", err)
	}
	if err := v.Verify(ctx, signWith("new")); err != nil {
		t.Fatalf("new secret: %v", err)
	}
	if CodeOf(v.Verify(ctx, signWith("revoked"))) != "invalid_signature" {
		t.Fatal("unknown secret must be rejected")
	}
	// replay of the same request is rejected; the nonce is scoped per org
	r := signWith("new")
	if err := v.Verify(ctx, r); err != nil {
		t.Fatal(err)
	}
	if CodeOf(v.Verify(ctx, r)) != "nonce_reused" {
		t.Fatal("replay must be nonce_reused")
	}
	rb := r
	rb.Org = orgB
	if err := v.Verify(ctx, rb); err != nil {
		t.Fatalf("same nonce on another org is a different key: %v", err)
	}
	// unknown org
	ru := r
	ru.Org = uuid.New()
	if CodeOf(v.Verify(ctx, ru)) != "unauthenticated" {
		t.Fatal("unknown org must be rejected")
	}
	// window
	v.Now = func() time.Time { return now.Add(301 * time.Second) }
	if CodeOf(v.Verify(ctx, signWith("new"))) != "stale_timestamp" {
		t.Fatal("stale")
	}
}

func TestVerifierBearerHMACAndSimple(t *testing.T) {
	now := time.Now()
	org := uuid.New()
	for _, spec := range []Spec{
		{Profile: Bearer, KeyRef: "a.b"},
		{Profile: HMACSimple, SecretRef: "a.b"},
		{Profile: BearerHMACCanonicalV1, SecretRef: "a.b", KeyRef: "a.c"},
	} {
		sec := Secrets{HMACKey: "k", BearerToken: "tok"}
		v := &Verifier{Spec: spec, Secrets: func(context.Context, uuid.UUID) ([]Secrets, error) { return []Secrets{sec}, nil }}
		req := Request{Method: "POST", Path: "/p", Body: []byte("x")}
		h, _ := Sign(spec, sec, req, now, "")
		vr := VerifyRequest{Org: org, Method: "POST", Path: "/p", Header: hdr(h), Body: []byte("x")}
		if err := v.Verify(context.Background(), vr); err != nil {
			t.Fatalf("%s: %v", spec.Profile, err)
		}
		vr.Body = []byte("tampered")
		if spec.Profile != Bearer && v.Verify(context.Background(), vr) == nil {
			t.Fatalf("%s: tampered body accepted", spec.Profile)
		}
	}
	if err := (&Verifier{Spec: Spec{Profile: Bearer, KeyRef: "a.b"}}).Verify(context.Background(), VerifyRequest{}); err == nil {
		t.Fatal("no resolver must fail closed")
	}
}

func TestVerifierNonceCacheFailsClosedWhenFull(t *testing.T) {
	now := time.Now()
	spec := Spec{Profile: HMACCanonicalV1, SecretRef: "a.b"}
	v := &Verifier{Spec: spec, MaxNonces: 2, Now: func() time.Time { return now },
		Secrets: func(context.Context, uuid.UUID) ([]Secrets, error) { return []Secrets{{HMACKey: "k"}}, nil }}
	var last error
	for i := 0; i < 3; i++ {
		h, _ := Sign(spec, Secrets{HMACKey: "k"}, Request{Method: "POST", Path: "/"}, now, "")
		last = v.Verify(context.Background(), VerifyRequest{Method: "POST", Path: "/", Header: hdr(h)})
	}
	if last == nil {
		t.Fatal("a full nonce set must fail closed")
	}
}

func TestFingerprintStable(t *testing.T) {
	if Fingerprint([]byte("a")) != Fingerprint([]byte("a")) || Fingerprint([]byte("a")) == Fingerprint([]byte("b")) {
		t.Fatal("fingerprint")
	}
	if Fingerprint(nil) != "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855" {
		t.Fatal("empty body hash")
	}
}

func TestNonceTTLIsAtLeastTwiceTheSkew(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	clock := now
	spec := Spec{Profile: HMACCanonicalV1, SecretRef: "a.b"}
	v := &Verifier{Spec: spec, NonceTTL: time.Second, Now: func() time.Time { return clock },
		Secrets: func(context.Context, uuid.UUID) ([]Secrets, error) { return []Secrets{{HMACKey: "k"}}, nil }}
	h, _ := Sign(spec, Secrets{HMACKey: "k"}, Request{Method: "POST", Path: "/"}, now, "")
	vr := VerifyRequest{Method: "POST", Path: "/", Header: hdr(h)}
	if err := v.Verify(context.Background(), vr); err != nil {
		t.Fatal(err)
	}
	clock = now.Add(290 * time.Second) // still inside the window: the same request must still be a replay
	if CodeOf(v.Verify(context.Background(), vr)) != "nonce_reused" {
		t.Fatal("a nonce forgotten before its timestamp expires allows replay")
	}
}
