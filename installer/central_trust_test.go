package installer

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestFetchCentralPubKey covers the happy path + the major error modes the
// installer's boot-time fetch needs to distinguish: 503 (hub up but no
// signing seed), arbitrary 5xx, malformed body, wrong algorithm, wrong key
// length.
func TestFetchCentralPubKey(t *testing.T) {
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	pubHex := hex.EncodeToString(pub)

	t.Run("happy path", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/v1/marketplace/pubkey" {
				http.NotFound(w, r)
				return
			}
			_ = json.NewEncoder(w).Encode(marketplacePubKeyResponse{
				PubKey:    pubHex,
				Algorithm: "ed25519",
			})
		}))
		defer srv.Close()
		got, err := FetchCentralPubKey(t.Context(), srv.URL)
		if err != nil {
			t.Fatalf("FetchCentralPubKey: %v", err)
		}
		if got != pubHex {
			t.Fatalf("want %s, got %s", pubHex, got)
		}
	})

	t.Run("503 — hub up but no seed", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusServiceUnavailable)
		}))
		defer srv.Close()
		_, err := FetchCentralPubKey(t.Context(), srv.URL)
		if err == nil || !strings.Contains(err.Error(), "503") {
			t.Fatalf("want 503 error, got %v", err)
		}
	})

	t.Run("malformed JSON", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte("not json"))
		}))
		defer srv.Close()
		_, err := FetchCentralPubKey(t.Context(), srv.URL)
		if err == nil {
			t.Fatalf("want error, got nil")
		}
	})

	t.Run("wrong algorithm rejected", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(marketplacePubKeyResponse{
				PubKey:    pubHex,
				Algorithm: "rsa",
			})
		}))
		defer srv.Close()
		_, err := FetchCentralPubKey(t.Context(), srv.URL)
		if err == nil || !strings.Contains(err.Error(), "ed25519") {
			t.Fatalf("want algorithm rejection, got %v", err)
		}
	})

	t.Run("non-hex pubkey rejected", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(marketplacePubKeyResponse{
				PubKey:    "not-hex",
				Algorithm: "ed25519",
			})
		}))
		defer srv.Close()
		_, err := FetchCentralPubKey(t.Context(), srv.URL)
		if err == nil {
			t.Fatalf("want decode error, got nil")
		}
	})

	t.Run("short pubkey rejected", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(marketplacePubKeyResponse{
				PubKey:    "deadbeef",
				Algorithm: "ed25519",
			})
		}))
		defer srv.Close()
		_, err := FetchCentralPubKey(t.Context(), srv.URL)
		if err == nil || !strings.Contains(err.Error(), "bytes") {
			t.Fatalf("want length rejection, got %v", err)
		}
	})

	t.Run("empty base URL", func(t *testing.T) {
		_, err := FetchCentralPubKey(t.Context(), "")
		if err == nil {
			t.Fatalf("want error, got nil")
		}
	})
}

// TestLoadCentralPubKeyIfNeeded covers the precedence rules: explicit env
// keys are respected (the central fetch is skipped), and the fetched key
// is appended when no env key is pinned.
func TestLoadCentralPubKeyIfNeeded(t *testing.T) {
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	pubHex := hex.EncodeToString(pub)

	t.Run("env keys win, fetch skipped", func(t *testing.T) {
		other, _, _ := ed25519.GenerateKey(rand.Reader)
		// Spin a server that would panic if called; the function under
		// test must NOT reach the network when env keys are present.
		called := false
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			called = true
			w.WriteHeader(http.StatusOK)
		}))
		defer srv.Close()
		t.Setenv("MARKETPLACE_URL", srv.URL)
		got := loadCentralPubKeyIfNeeded([]ed25519.PublicKey{other})
		if len(got) != 1 {
			t.Fatalf("want 1 key, got %d", len(got))
		}
		if called {
			t.Fatalf("central fetch must not run when env keys are pinned")
		}
	})

	t.Run("appends fetched key when env empty", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(marketplacePubKeyResponse{
				PubKey:    pubHex,
				Algorithm: "ed25519",
			})
		}))
		defer srv.Close()
		t.Setenv("MARKETPLACE_URL", srv.URL)
		got := loadCentralPubKeyIfNeeded(nil)
		if len(got) != 1 {
			t.Fatalf("want 1 fetched key, got %d", len(got))
		}
		if hex.EncodeToString(got[0]) != pubHex {
			t.Fatalf("fetched key mismatch")
		}
	})

	t.Run("fetch failure logs and returns empty", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusServiceUnavailable)
		}))
		defer srv.Close()
		t.Setenv("MARKETPLACE_URL", srv.URL)
		got := loadCentralPubKeyIfNeeded(nil)
		if len(got) != 0 {
			t.Fatalf("want 0 keys when fetch fails, got %d", len(got))
		}
	})
}

// TestAppendTrustedPubKey covers the runtime rotation knob. The installer
// is expected to accept the new key and verify under either the old or
// new key (the "verify under ANY" semantics).
func TestAppendTrustedPubKey(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	pubHex := hex.EncodeToString(pub)

	i := &Installer{}
	if err := i.AppendTrustedPubKey(pubHex); err != nil {
		t.Fatalf("AppendTrustedPubKey: %v", err)
	}
	if len(i.PublicKeys) != 1 {
		t.Fatalf("want 1 trusted key, got %d", len(i.PublicKeys))
	}

	t.Run("non-hex rejected", func(t *testing.T) {
		err := i.AppendTrustedPubKey("not-hex")
		if err == nil {
			t.Fatalf("want error, got nil")
		}
	})
	t.Run("short rejected", func(t *testing.T) {
		err := i.AppendTrustedPubKey("deadbeef")
		if err == nil {
			t.Fatalf("want error, got nil")
		}
	})
}
