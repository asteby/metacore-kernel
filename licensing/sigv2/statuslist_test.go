package sigv2

import (
	"errors"
	"testing"
	"time"
)

func statusFixture(t *testing.T) (func(seq uint64, issued, until time.Time, items ...RevokedItem) string, Keyring) {
	pub, priv := gen(t)
	kid := KeyID(pub)
	ring := NewStaticKeyring().Add(kid, pub, "status")
	mk := func(seq uint64, issued, until time.Time, items ...RevokedItem) string {
		tok, err := SealStatus(priv, kid, StatusDoc{Seq: seq, IssuedAt: issued, ValidUntil: until, Revoked: items})
		if err != nil {
			t.Fatal(err)
		}
		return tok
	}
	return mk, ring
}

func TestStatus_AcceptsFreshAndAnswersRevoked(t *testing.T) {
	mk, ring := statusFixture(t)
	tr := NewTracker(TrackerConfig{Keys: ring, Now: func() time.Time { return t0 }})
	if err := tr.Offer(mk(5, t0, t0.Add(24*time.Hour), RevokedItem{Kind: KindLicense, ID: "lic-1"},
		RevokedItem{Kind: KindAddon, ID: "crm", Version: "1.2.0"})); err != nil {
		t.Fatal(err)
	}
	if !tr.IsRevoked(KindLicense, "lic-1", "") || tr.IsRevoked(KindLicense, "lic-2", "") {
		t.Fatal("license lookup wrong")
	}
	if !tr.IsRevoked(KindAddon, "crm", "1.2.0") || tr.IsRevoked(KindAddon, "crm", "1.3.0") {
		t.Fatal("addon version lookup wrong")
	}
	if v := tr.Evaluate(t0); !v.Fresh || v.Action != ActionAllow {
		t.Fatalf("verdict %+v", v)
	}
}

func TestStatus_RejectsExpiredList(t *testing.T) {
	mk, ring := statusFixture(t)
	tr := NewTracker(TrackerConfig{Keys: ring, Now: func() time.Time { return t0 }})
	err := tr.Offer(mk(5, t0.Add(-48*time.Hour), t0.Add(-time.Hour)))
	if !errors.Is(err, ErrStatusExpired) {
		t.Fatalf("expired list accepted: %v", err)
	}
	if tr.Seq() != 0 {
		t.Fatal("expired list changed state")
	}
}

func TestStatus_RejectsOldSeq_FreezeAttack(t *testing.T) {
	mk, ring := statusFixture(t)
	tr := NewTracker(TrackerConfig{Keys: ring, Now: func() time.Time { return t0 }})
	if err := tr.Offer(mk(7, t0, t0.Add(24*time.Hour), RevokedItem{Kind: KindLicense, ID: "bad"})); err != nil {
		t.Fatal(err)
	}
	// an attacker replays an older (still unexpired) list that does not carry the revocation
	if err := tr.Offer(mk(6, t0, t0.Add(48*time.Hour))); !errors.Is(err, ErrStatusRollback) {
		t.Fatalf("old seq accepted: %v", err)
	}
	// same seq, shorter validity: also a rollback
	if err := tr.Offer(mk(7, t0, t0.Add(time.Hour))); !errors.Is(err, ErrStatusRollback) {
		t.Fatalf("same seq with earlier valid_until accepted: %v", err)
	}
	// same seq, later validity: a legitimate freshness refresh
	if err := tr.Offer(mk(7, t0, t0.Add(48*time.Hour), RevokedItem{Kind: KindLicense, ID: "bad"})); err != nil {
		t.Fatalf("refresh refused: %v", err)
	}
	if !tr.IsRevoked(KindLicense, "bad", "") {
		t.Fatal("known revocation forgotten")
	}
}

func TestStatus_WrongSignerAndDomain(t *testing.T) {
	mk, _ := statusFixture(t)
	_, otherRing := statusFixture(t)
	tr := NewTracker(TrackerConfig{Keys: otherRing, Now: func() time.Time { return t0 }})
	if err := tr.Offer(mk(1, t0, t0.Add(time.Hour))); err == nil {
		t.Fatal("list from an untrusted key accepted")
	}
	// a LEASE envelope must not pass as a revocation list
	pub, priv := gen(t)
	ring := NewStaticKeyring().Add(KeyID(pub), pub)
	lease, _ := Seal(priv, KeyID(pub), DomLease, []byte(`{"seq":1}`))
	tr2 := NewTracker(TrackerConfig{Keys: ring, Now: func() time.Time { return t0 }})
	if err := tr2.Offer(lease); !errors.Is(err, ErrWrongDomain) {
		t.Fatalf("lease accepted as a list: %v", err)
	}
	// and the legacy format is never a valid list
	if err := tr2.Offer(legacyToken(priv, []byte(`{"seq":1}`))); err == nil {
		t.Fatal("legacy-format list accepted")
	}
}

func TestStatus_StalePolicies(t *testing.T) {
	mk, ring := statusFixture(t)
	issued := t0
	until := t0.Add(24 * time.Hour)
	later := until.Add(8 * 24 * time.Hour) // past the 7d default margin
	cases := map[StalePolicy]Action{
		"": ActionWarn, PolicyWarn: ActionWarn, PolicyReadOnly: ActionReadOnly,
		PolicyBlock: ActionBlock, PolicyIgnore: ActionAllow,
	}
	for pol, want := range cases {
		tr := NewTracker(TrackerConfig{Keys: ring, Policy: pol, Now: func() time.Time { return t0 }})
		if err := tr.Offer(mk(1, issued, until)); err != nil {
			t.Fatal(err)
		}
		if v := tr.Evaluate(later); v.Fresh || v.Action != want || (want != ActionAllow && v.Reason == "") {
			t.Errorf("policy %q: %+v want %s", pol, v, want)
		}
	}
}

func TestStatus_MarginKeepsOperatingSilently(t *testing.T) {
	mk, ring := statusFixture(t)
	tr := NewTracker(TrackerConfig{Keys: ring, Policy: PolicyBlock, Margin: 48 * time.Hour, Now: func() time.Time { return t0 }})
	until := t0.Add(24 * time.Hour)
	_ = tr.Offer(mk(1, t0, until))
	if v := tr.Evaluate(until.Add(47 * time.Hour)); !v.Fresh || v.Action != ActionAllow {
		t.Fatalf("inside margin: %+v", v)
	}
	if v := tr.Evaluate(until.Add(49 * time.Hour)); v.Action != ActionBlock {
		t.Fatalf("past margin: %+v", v)
	}
}

func TestStatus_NoListYetFollowsPolicy_DefaultKeepsOperating(t *testing.T) {
	_, ring := statusFixture(t)
	tr := NewTracker(TrackerConfig{Keys: ring})
	if v := tr.Evaluate(t0); v.Action != ActionWarn || v.Reason == "" {
		t.Fatalf("default with no list must warn and keep operating: %+v", v)
	}
}

func TestStatus_RestoreKeepsSeqFloorAndRevocations(t *testing.T) {
	mk, ring := statusFixture(t)
	old := mk(9, t0.Add(-72*time.Hour), t0.Add(-48*time.Hour), RevokedItem{Kind: KindKey, ID: "k-bad"})
	tr := NewTracker(TrackerConfig{Keys: ring, Now: func() time.Time { return t0 }})
	if err := tr.Restore(old); err != nil {
		t.Fatalf("restoring an old persisted list: %v", err)
	}
	if !tr.IsRevoked(KindKey, "k-bad", "") || tr.Seq() != 9 {
		t.Fatal("restore lost state")
	}
	if err := tr.Offer(mk(8, t0, t0.Add(time.Hour))); !errors.Is(err, ErrStatusRollback) {
		t.Fatalf("seq floor not kept after restore: %v", err)
	}
	if tr.Snapshot() != old {
		t.Fatal("snapshot mismatch")
	}
}

func TestParseStalePolicy_TypoNeverLocksOut(t *testing.T) {
	if ParseStalePolicy("bloc") != PolicyWarn || ParseStalePolicy("") != PolicyWarn {
		t.Fatal("unknown value must fall back to warn")
	}
	if ParseStalePolicy(" BLOCK ") != PolicyBlock || ParseStalePolicy("readonly") != PolicyReadOnly {
		t.Fatal("known values not parsed")
	}
}

// Restore after Offer must not lower the anti-rollback floor.
func TestStatus_RestoreCannotLowerSeqFloor(t *testing.T) {
	mk, ring := statusFixture(t)
	tr := NewTracker(TrackerConfig{Keys: ring, Now: func() time.Time { return t0 }})
	if err := tr.Offer(mk(9, t0, t0.Add(24*time.Hour), RevokedItem{Kind: KindLicense, ID: "bad"})); err != nil {
		t.Fatal(err)
	}
	if err := tr.Restore(mk(3, t0, t0.Add(24*time.Hour))); !errors.Is(err, ErrStatusRollback) {
		t.Fatalf("restore of an older list must be refused: %v", err)
	}
	if tr.Seq() != 9 || !tr.IsRevoked(KindLicense, "bad", "") {
		t.Fatal("restore lowered the floor / dropped a revocation")
	}
}

// The public list carries HashedID, not the raw license id.
func TestStatus_HashedIDMatches(t *testing.T) {
	mk, ring := statusFixture(t)
	tr := NewTracker(TrackerConfig{Keys: ring, Now: func() time.Time { return t0 }})
	if err := tr.Offer(mk(1, t0, t0.Add(time.Hour), RevokedItem{Kind: KindLicense, ID: HashedID(KindLicense, "lic-9")})); err != nil {
		t.Fatal(err)
	}
	if !tr.IsRevoked(KindLicense, "lic-9", "") || tr.IsRevoked(KindLicense, "lic-8", "") {
		t.Fatal("hashed id lookup wrong")
	}
	// golden vector shared with the hub (internal/license/statuslist_test.go)
	if got := HashedID(KindLicense, "lic-9"); got != "c8a9c101c0d96ee9658f3222cb2a50aae07137ae7656d8cde2088a126c720464" {
		t.Fatalf("golden hashed id changed: %s", got)
	}
}
