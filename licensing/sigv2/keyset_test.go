package sigv2

import (
	"crypto/ed25519"
	"encoding/hex"
	"errors"
	"testing"
	"time"
)

type rootKeys struct {
	pub  map[string]ed25519.PublicKey
	priv map[string]ed25519.PrivateKey
}

func newKeys(t *testing.T, names ...string) rootKeys {
	rk := rootKeys{pub: map[string]ed25519.PublicKey{}, priv: map[string]ed25519.PrivateKey{}}
	for _, n := range names {
		p, s := gen(t)
		rk.pub[n], rk.priv[n] = p, s
	}
	return rk
}

func (rk rootKeys) entry(n string) KeyEntry { return KeyEntry{Pub: hex.EncodeToString(rk.pub[n])} }

func (rk rootKeys) sign(t *testing.T, doc RootDoc, signers ...string) []byte {
	m := map[string]ed25519.PrivateKey{}
	for _, s := range signers {
		m[s] = rk.priv[s]
	}
	raw, err := SignRoot(doc, m)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func baseRoot(rk rootKeys, version int, expires time.Time) RootDoc {
	return RootDoc{
		Type: "root", Version: version, Expires: expires,
		Keys: map[string]KeyEntry{
			"r1": rk.entry("r1"), "r2": rk.entry("r2"), "r3": rk.entry("r3"),
			"lease1": rk.entry("lease1"), "status1": rk.entry("status1"), "rel1": rk.entry("rel1"),
		},
		Roles: map[string]RoleEntry{
			"root":    {KeyIDs: []string{"r1", "r2", "r3"}, Threshold: 2},
			"lease":   {KeyIDs: []string{"lease1"}, Threshold: 1},
			"status":  {KeyIDs: []string{"status1"}, Threshold: 1},
			"release": {KeyIDs: []string{"rel1"}, Threshold: 1},
		},
	}
}

func TestKeySet_BootstrapThreshold(t *testing.T) {
	rk := newKeys(t, "r1", "r2", "r3", "lease1", "status1", "rel1")
	exp := t0.Add(365 * 24 * time.Hour)
	if _, err := Bootstrap(rk.sign(t, baseRoot(rk, 1, exp), "r1", "r2"), t0); err != nil {
		t.Fatalf("2-of-3 must bootstrap: %v", err)
	}
	if _, err := Bootstrap(rk.sign(t, baseRoot(rk, 1, exp), "r1"), t0); !errors.Is(err, ErrRootThreshold) {
		t.Fatalf("1-of-3 must fail threshold: %v", err)
	}
	// the same key signing twice counts once
	raw := rk.sign(t, baseRoot(rk, 1, exp), "r1")
	if _, err := Bootstrap(raw, t0); err == nil {
		t.Fatal("single signer accepted")
	}
	if _, err := Bootstrap(rk.sign(t, baseRoot(rk, 1, t0.Add(-time.Hour)), "r1", "r2"), t0); !errors.Is(err, ErrRootExpired) {
		t.Fatalf("expired root bootstrapped: %v", err)
	}
}

func TestKeySet_ResolveByRoleAndWindow(t *testing.T) {
	rk := newKeys(t, "r1", "r2", "r3", "lease1", "status1", "rel1")
	doc := baseRoot(rk, 1, t0.Add(24*time.Hour))
	nb := t0.Add(time.Hour)
	e := doc.Keys["lease1"]
	e.NotBefore = &nb
	doc.Keys["lease1"] = e
	ks, err := Bootstrap(rk.sign(t, doc, "r1", "r2"), t0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ks.Resolve("lease1", DomLease, t0); !errors.Is(err, ErrKeyNotActive) {
		t.Fatalf("not_before ignored: %v", err)
	}
	if _, err := ks.Resolve("lease1", DomLease, t0.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := ks.Resolve("lease1", DomRevocations, t0.Add(2*time.Hour)); !errors.Is(err, ErrKeyRole) {
		t.Fatalf("lease key resolved for status: %v", err)
	}
	if _, err := ks.Resolve("rel1", DomLease, t0); !errors.Is(err, ErrKeyRole) {
		t.Fatalf("release key resolved for lease: %v", err)
	}
	if _, err := ks.Resolve("nope", DomLease, t0); !errors.Is(err, ErrUnknownKID) {
		t.Fatalf("unknown kid: %v", err)
	}
}

func TestKeySet_EndToEnd_EnvelopeVerifiedThroughKeySet(t *testing.T) {
	rk := newKeys(t, "r1", "r2", "r3", "lease1", "status1", "rel1")
	ks, err := Bootstrap(rk.sign(t, baseRoot(rk, 1, t0.Add(24*time.Hour)), "r1", "r2"), t0)
	if err != nil {
		t.Fatal(err)
	}
	tok, _ := Seal(rk.priv["lease1"], "lease1", DomLease, []byte(`{}`))
	if _, err := Open(tok, DomLease, ks, Policy{}, t0); err != nil {
		t.Fatal(err)
	}
}

func TestKeySet_UpdateChainRotatesKeyWithoutReflash(t *testing.T) {
	rk := newKeys(t, "r1", "r2", "r3", "lease1", "status1", "rel1", "lease2")
	exp := t0.Add(365 * 24 * time.Hour)
	ks, err := Bootstrap(rk.sign(t, baseRoot(rk, 1, exp), "r1", "r2"), t0)
	if err != nil {
		t.Fatal(err)
	}
	// v2 drops lease1, adds lease2
	next := baseRoot(rk, 2, exp)
	delete(next.Keys, "lease1")
	next.Keys["lease2"] = rk.entry("lease2")
	next.Roles["lease"] = RoleEntry{KeyIDs: []string{"lease2"}, Threshold: 1}
	if err := ks.Update(rk.sign(t, next, "r2", "r3"), t0); err != nil {
		t.Fatalf("chained update: %v", err)
	}
	if ks.Version() != 2 {
		t.Fatalf("version=%d", ks.Version())
	}
	old, _ := Seal(rk.priv["lease1"], "lease1", DomLease, []byte(`{}`))
	if _, err := Open(old, DomLease, ks, Policy{}, t0); !errors.Is(err, ErrUnknownKID) {
		t.Fatalf("revoked-by-rotation key still trusted: %v", err)
	}
	fresh, _ := Seal(rk.priv["lease2"], "lease2", DomLease, []byte(`{}`))
	if _, err := Open(fresh, DomLease, ks, Policy{}, t0); err != nil {
		t.Fatal(err)
	}
}

func TestKeySet_UpdateRejections(t *testing.T) {
	rk := newKeys(t, "r1", "r2", "r3", "lease1", "status1", "rel1")
	exp := t0.Add(365 * 24 * time.Hour)
	boot := func() *KeySet {
		ks, err := Bootstrap(rk.sign(t, baseRoot(rk, 1, exp), "r1", "r2"), t0)
		if err != nil {
			t.Fatal(err)
		}
		return ks
	}
	cases := map[string]struct {
		raw  []byte
		want error
	}{
		"rollback (same version)": {rk.sign(t, baseRoot(rk, 1, exp), "r1", "r2"), ErrRootRollback},
		"skip a version":          {rk.sign(t, baseRoot(rk, 3, exp), "r1", "r2"), ErrRootRollback},
		"below threshold":         {rk.sign(t, baseRoot(rk, 2, exp), "r1"), ErrRootThreshold},
		"expired new root":        {rk.sign(t, baseRoot(rk, 2, t0.Add(-time.Minute)), "r1", "r2"), ErrRootExpired},
	}
	for name, c := range cases {
		ks := boot()
		if err := ks.Update(c.raw, t0); !errors.Is(err, c.want) {
			t.Errorf("%s: want %v, got %v", name, c.want, err)
		}
		if ks.Version() != 1 {
			t.Errorf("%s: trusted root changed on a failed update", name)
		}
	}
	// New root signed only by keys the CURRENT root does not trust.
	rk2 := newKeys(t, "x1", "x2")
	evil := baseRoot(rk, 2, exp)
	evil.Keys["x1"], evil.Keys["x2"] = rk2.entry("x1"), rk2.entry("x2")
	evil.Roles["root"] = RoleEntry{KeyIDs: []string{"x1", "x2"}, Threshold: 2}
	raw, _ := SignRoot(evil, map[string]ed25519.PrivateKey{"x1": rk2.priv["x1"], "x2": rk2.priv["x2"]})
	ks := boot()
	if err := ks.Update(raw, t0); !errors.Is(err, ErrRootThreshold) {
		t.Fatalf("self-signed takeover accepted: %v", err)
	}
}

func TestKeySet_ExpiredReportedNotEnforcedInResolve(t *testing.T) {
	rk := newKeys(t, "r1", "r2", "r3", "lease1", "status1", "rel1")
	ks, err := Bootstrap(rk.sign(t, baseRoot(rk, 1, t0.Add(time.Hour)), "r1", "r2"), t0)
	if err != nil {
		t.Fatal(err)
	}
	later := t0.Add(48 * time.Hour)
	if !ks.Expired(later) {
		t.Fatal("Expired not reported")
	}
	if _, err := ks.Resolve("lease1", DomLease, later); err != nil {
		t.Fatalf("an offline box must keep the last verified root: %v", err)
	}
}
