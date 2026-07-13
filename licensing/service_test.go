package licensing

import (
	"context"
	"log/slog"
	"testing"
	"time"
)

// newTestService builds a store-less service with a fixed pubkey + clock so
// State transitions can be exercised without persistence. recompute
// short-circuits its Save when store == nil.
func newTestService(pubHex string, enforce bool, grace time.Duration, now time.Time) *Service {
	s := &Service{
		log:       slog.Default(),
		enforce:   enforce,
		grace:     grace,
		pubKeyHex: pubHex,
		now:       func() time.Time { return now },
	}
	s.store2(&State{Enforced: enforce, Status: "missing", Entitlements: []string{}})
	return s
}

// persistedRow builds a Record carrying the token, mirroring what persist()
// would store (recompute re-verifies the token from scratch).
func persistedRow(token string) *Record { return &Record{Token: token} }

func TestService_StateValid(t *testing.T) {
	priv, pubHex := testKeypair(t)
	now := time.Now().UTC()
	svc := newTestService(pubHex, true, DefaultGrace, now)

	svc.recompute(context.Background(), persistedRow(mintToken(t, priv, baseClaims())))

	st := svc.Current()
	if !st.Valid || st.Status != "valid" {
		t.Fatalf("expected valid, got %+v", st)
	}
	if st.WritesBlocked() {
		t.Errorf("valid license must not block writes")
	}
	if st.InstallBlocked() {
		t.Errorf("valid license must not block installs")
	}
	if !st.Operable() {
		t.Errorf("valid license must be operable")
	}
	if !st.Entitles("workshop") {
		t.Errorf("expected entitlement workshop")
	}
	if st.DaysRemaining <= 0 {
		t.Errorf("expected positive days remaining, got %d", st.DaysRemaining)
	}
}

func TestService_StateGrace(t *testing.T) {
	priv, pubHex := testKeypair(t)
	now := time.Now().UTC()
	svc := newTestService(pubHex, true, 14*24*time.Hour, now)

	cl := baseClaims()
	cl.IssuedAt = now.Add(-400 * 24 * time.Hour)
	cl.ExpiresAt = now.Add(-3 * 24 * time.Hour) // expired 3d ago, within 14d grace
	svc.recompute(context.Background(), persistedRow(mintToken(t, priv, cl)))

	st := svc.Current()
	if st.Status != "grace" || !st.InGrace {
		t.Fatalf("expected grace, got %+v", st)
	}
	if st.Valid {
		t.Errorf("grace is not valid")
	}
	if st.WritesBlocked() {
		t.Errorf("grace still permits writes")
	}
	if st.InstallBlocked() {
		t.Errorf("grace still permits installs (operable)")
	}
	if !st.Entitles("workshop") {
		t.Errorf("grace should still expose entitlements")
	}
}

func TestService_StateExpiredPastGrace(t *testing.T) {
	priv, pubHex := testKeypair(t)
	now := time.Now().UTC()
	svc := newTestService(pubHex, true, 14*24*time.Hour, now)

	cl := baseClaims()
	cl.IssuedAt = now.Add(-400 * 24 * time.Hour)
	cl.ExpiresAt = now.Add(-30 * 24 * time.Hour) // past the 14d grace
	svc.recompute(context.Background(), persistedRow(mintToken(t, priv, cl)))

	st := svc.Current()
	if st.Status != "expired" || st.InGrace {
		t.Fatalf("expected expired past grace, got %+v", st)
	}
	if !st.WritesBlocked() || !st.InstallBlocked() {
		t.Errorf("expired-past-grace must block writes and installs when enforced")
	}
}

func TestService_StateStaleLease(t *testing.T) {
	priv, pubHex := testKeypair(t)
	now := time.Now().UTC()
	svc := newTestService(pubHex, true, DefaultGrace, now)

	cl := baseClaims()
	cl.MaxOfflineHours = 72
	cl.IssuedAt = now.Add(-100 * time.Hour) // 100h > 72h lease → stale
	svc.recompute(context.Background(), persistedRow(mintToken(t, priv, cl)))

	st := svc.Current()
	if st.Status != "stale" || !st.Stale {
		t.Fatalf("expected stale, got %+v", st)
	}
	// Stale is still operable: writes flow, the UI just nags to renew.
	if !st.Valid || st.WritesBlocked() {
		t.Errorf("stale must remain operable, got %+v", st)
	}
}

func TestService_EnforceOffNeverBlocks(t *testing.T) {
	priv, pubHex := testKeypair(t)
	now := time.Now().UTC()
	svc := newTestService(pubHex, false, 14*24*time.Hour, now) // enforce OFF

	cl := baseClaims()
	cl.ExpiresAt = now.Add(-90 * 24 * time.Hour)
	svc.recompute(context.Background(), persistedRow(mintToken(t, priv, cl)))

	st := svc.Current()
	if st.WritesBlocked() || st.InstallBlocked() || !st.Operable() {
		t.Errorf("enforcement off must never block")
	}
}

func TestService_MissingLicense(t *testing.T) {
	_, pubHex := testKeypair(t)
	svc := newTestService(pubHex, true, DefaultGrace, time.Now().UTC())
	svc.recompute(context.Background(), nil)

	st := svc.Current()
	if st.Status != "missing" || st.Valid {
		t.Fatalf("expected missing, got %+v", st)
	}
	if !st.WritesBlocked() || !st.InstallBlocked() {
		t.Errorf("enforced + missing must block writes and installs")
	}
}

func TestService_InvalidToken(t *testing.T) {
	_, pubHex := testKeypair(t)
	otherPriv, _ := testKeypair(t) // signed by a key the service does not trust
	svc := newTestService(pubHex, true, DefaultGrace, time.Now().UTC())
	svc.recompute(context.Background(), persistedRow(mintToken(t, otherPriv, baseClaims())))

	st := svc.Current()
	if st.Status != "invalid" {
		t.Fatalf("expected invalid, got %+v", st)
	}
	if !st.WritesBlocked() {
		t.Errorf("invalid token must block writes when enforced")
	}
}

// TestService_EntitlementGate asserts install-gate semantics: a valid license
// entitles only the addons in its set, while a wildcard entitles everything.
func TestService_EntitlementGate(t *testing.T) {
	priv, pubHex := testKeypair(t)
	now := time.Now().UTC()
	svc := newTestService(pubHex, true, DefaultGrace, now)

	svc.recompute(context.Background(), persistedRow(mintToken(t, priv, baseClaims())))
	st := svc.Current()
	if !st.Entitles("workshop") {
		t.Errorf("licensed addon must be entitled")
	}
	if st.Entitles("payroll") {
		t.Errorf("unlicensed addon must NOT be entitled (install gate would block)")
	}

	cl := baseClaims()
	cl.Addons = []string{"*"}
	svc.recompute(context.Background(), persistedRow(mintToken(t, priv, cl)))
	if !svc.Current().Entitles("payroll") {
		t.Errorf("wildcard license must entitle any addon")
	}
}
