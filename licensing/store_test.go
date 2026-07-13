package licensing

import (
	"context"
	"log/slog"
	"net/http"
	"path/filepath"
	"testing"
	"time"
)

// newStoreService wires a Service to a concrete store with a fixed pubkey and
// clock, exercising the persistence path (persist/load/Boot/Activate).
func newStoreService(store LicenseStore, pubHex string, enforce bool, now time.Time) *Service {
	s := &Service{
		store:     store,
		log:       slog.Default(),
		http:      &http.Client{Timeout: time.Second},
		hubBase:   DefaultHubBaseURL,
		enforce:   enforce,
		grace:     DefaultGrace,
		recheck:   DefaultRecheck,
		pubKeyHex: pubHex,
		now:       func() time.Time { return now },
	}
	s.store2(&State{Enforced: enforce, Status: "missing", Entitlements: []string{}})
	return s
}

func TestMemoryStore_RoundTrip(t *testing.T) {
	ctx := context.Background()
	st := NewMemoryStore()
	if rec, err := st.Load(ctx); err != nil || rec != nil {
		t.Fatalf("empty store: rec=%v err=%v", rec, err)
	}
	now := time.Now().UTC()
	if err := st.Save(ctx, &Record{Token: "tok", EntitledAddons: []string{"*"}, ValidatedAt: &now}); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := st.Load(ctx)
	if err != nil || got == nil || got.Token != "tok" || !got.Wildcard() {
		t.Fatalf("load after save: got=%+v err=%v", got, err)
	}
	// Load returns a copy: mutating it must not corrupt the store.
	got.Token = "mutated"
	again, _ := st.Load(ctx)
	if again.Token != "tok" {
		t.Fatalf("store must return a defensive copy, got %q", again.Token)
	}
}

func TestFileStore_RoundTrip(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "nested", "license.json")
	fs := NewFileStore(path)

	if rec, err := fs.Load(ctx); err != nil || rec != nil {
		t.Fatalf("absent file must load nil: rec=%v err=%v", rec, err)
	}
	now := time.Now().UTC()
	rec := &Record{Token: "abc.def", OrgID: "cust-1", EntitledAddons: []string{"workshop"}, ExpiresAt: &now, Source: "admin"}
	if err := fs.Save(ctx, rec); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := fs.Load(ctx)
	if err != nil || got == nil || got.Token != "abc.def" || got.OrgID != "cust-1" {
		t.Fatalf("reload: got=%+v err=%v", got, err)
	}
	// A fresh store over the same path reads the persisted record.
	if got2, _ := NewFileStore(path).Load(ctx); got2 == nil || got2.Token != "abc.def" {
		t.Fatalf("persisted file not readable by a new store: %+v", got2)
	}
}

func TestService_BootAdoptsBootstrapToken(t *testing.T) {
	priv, pubHex := testKeypair(t)
	now := time.Now().UTC()
	store := NewMemoryStore()
	svc := newStoreService(store, pubHex, true, now)
	svc.bootstrapToken = mintToken(t, priv, baseClaims())
	svc.bootstrapSource = "env"

	svc.Boot(context.Background())

	st := svc.Current()
	if !st.Valid || st.Status != "valid" {
		t.Fatalf("boot should adopt + validate bootstrap token, got %+v", st)
	}
	// The token was persisted so a restart (Boot with no bootstrap) still works.
	rec, _ := store.Load(context.Background())
	if rec == nil || rec.Source != "env" {
		t.Fatalf("bootstrap token not persisted: %+v", rec)
	}
}

func TestService_ActivatePersistsAndRepublishes(t *testing.T) {
	priv, pubHex := testKeypair(t)
	now := time.Now().UTC()
	store := NewMemoryStore()
	svc := newStoreService(store, pubHex, true, now)

	tok := mintToken(t, priv, baseClaims())
	st, err := svc.Activate(context.Background(), tok)
	if err != nil {
		t.Fatalf("activate: %v", err)
	}
	if !st.Valid {
		t.Fatalf("activated license should be valid, got %+v", st)
	}
	if rec, _ := store.Load(context.Background()); rec == nil || rec.Source != "admin" {
		t.Fatalf("activate must persist with source=admin, got %+v", rec)
	}

	// Activating a token signed by an untrusted key is rejected and does not
	// clobber the good persisted license.
	otherPriv, _ := testKeypair(t)
	if _, err := svc.Activate(context.Background(), mintToken(t, otherPriv, baseClaims())); err == nil {
		t.Fatalf("activate with untrusted key must fail")
	}
	if rec, _ := store.Load(context.Background()); rec.Source != "admin" {
		t.Fatalf("rejected activation must not overwrite the license")
	}
}

func TestService_OfflineFallsBackToPersistedWindow(t *testing.T) {
	// No pubkey configured and no hub reachable → recompute cannot Verify and
	// must trust the persisted (previously signature-checked) window.
	now := time.Now().UTC()
	store := NewMemoryStore()
	exp := now.Add(30 * 24 * time.Hour)
	_ = store.Save(context.Background(), &Record{
		Token:          "opaque",
		OrgID:          "cust-9",
		EntitledAddons: []string{"workshop"},
		ExpiresAt:      &exp,
	})
	svc := newStoreService(store, "", true, now) // no pubkey
	svc.hubBase = "http://127.0.0.1:1"           // unreachable → pubKey() errors

	row, _ := store.Load(context.Background())
	svc.recompute(context.Background(), row)

	st := svc.Current()
	if !st.Valid || st.Status != "valid" || !st.Entitles("workshop") {
		t.Fatalf("offline fallback should keep running on persisted window, got %+v", st)
	}
}
