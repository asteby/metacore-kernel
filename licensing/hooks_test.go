package licensing

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

// verifyLikeHub replicates the hub's verifyInstanceSignature: X-Signature =
// "ed25519:" + base64(sig over sha256(body || "\n" || ts)), with the pubkey in
// X-Instance-Pubkey and a ±5 min timestamp window.
func verifyLikeHub(r *http.Request, body []byte, wantPub ed25519.PublicKey) error {
	sigHdr, ts := r.Header.Get("X-Signature"), r.Header.Get("X-Signature-Timestamp")
	pubHex := r.Header.Get("X-Instance-Pubkey")
	if !strings.HasPrefix(sigHdr, "ed25519:") || ts == "" || pubHex == "" {
		return errors.New("missing signature headers")
	}
	sig, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(sigHdr, "ed25519:"))
	if err != nil {
		return err
	}
	n, err := strconv.ParseInt(ts, 10, 64)
	if err != nil || time.Since(time.Unix(n, 0)).Abs() > 5*time.Minute {
		return errors.New("bad timestamp")
	}
	pub, err := hex.DecodeString(pubHex)
	if err != nil || !bytes.Equal(pub, wantPub) {
		return errors.New("unexpected pubkey")
	}
	h := sha256.New()
	h.Write(body)
	h.Write([]byte("\n"))
	h.Write([]byte(ts))
	if !ed25519.Verify(ed25519.PublicKey(pub), h.Sum(nil), sig) {
		return errors.New("signature mismatch")
	}
	return nil
}

type fakeHub struct {
	t       *testing.T
	instPub ed25519.PublicKey
	hubPriv ed25519.PrivateKey
	respond func(body map[string]any) (int, string) // renew response
	mu      sync.Mutex
	bodies  map[string][][]byte
	sigErrs []error
	srv     *httptest.Server
}

func newFakeHub(t *testing.T, hubPriv ed25519.PrivateKey, instPub ed25519.PublicKey) *fakeHub {
	h := &fakeHub{t: t, hubPriv: hubPriv, instPub: instPub, bodies: map[string][][]byte{}}
	h.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		h.mu.Lock()
		h.bodies[r.URL.Path] = append(h.bodies[r.URL.Path], body)
		if err := verifyLikeHub(r, body, h.instPub); err != nil {
			h.sigErrs = append(h.sigErrs, err)
			h.mu.Unlock()
			http.Error(w, err.Error(), http.StatusUnauthorized)
			return
		}
		h.mu.Unlock()
		switch r.URL.Path {
		case "/v1/licenses/renew":
			var m map[string]any
			_ = json.Unmarshal(body, &m)
			code, out := h.respond(m)
			w.WriteHeader(code)
			_, _ = io.WriteString(w, out)
		case "/v1/licenses/brand":
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(h.srv.Close)
	return h
}

func (h *fakeHub) calls(path string) [][]byte {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.sigErrs) > 0 {
		h.t.Fatalf("hub rejected signatures: %v", h.sigErrs)
	}
	return h.bodies[path]
}

// renewedJSON is what today's hub answers to renew (no brand field).
func (h *fakeHub) renewedJSON(extra string) string {
	c := baseClaims()
	tok := mintToken(h.t, h.hubPriv, c)
	return `{"license_token":"` + tok + `","expires_at":"` + c.ExpiresAt.Format(time.RFC3339) + `"` + extra + `}`
}

type hookEnv struct {
	hub     *fakeHub
	instID  uuid.UUID
	newSvc  func(cfg Config) *Service
	hubPriv ed25519.PrivateKey
}

// setup returns a factory for linked services whose bootstrap token expires
// in `expiresIn` (inside the 30-day window ⇒ RefreshNow renews).
func setup(t *testing.T, expiresIn time.Duration) hookEnv {
	t.Helper()
	hubPriv, hubPubHex := testKeypair(t)
	instPub, instPriv, _ := ed25519.GenerateKey(nil)
	hub := newFakeHub(t, hubPriv, instPub)
	instID := uuid.New()
	cl := baseClaims()
	cl.ExpiresAt = time.Now().UTC().Add(expiresIn)
	token := mintToken(t, hubPriv, cl)
	return hookEnv{hub: hub, instID: instID, hubPriv: hubPriv, newSvc: func(cfg Config) *Service {
		cfg.PubKeyHex = hubPubHex
		cfg.HubBaseURL = hub.srv.URL
		cfg.InstanceID = instID
		cfg.InstancePriv = instPriv
		cfg.Version = "1.2.3"
		cfg.Store = NewMemoryStore()
		cfg.BootstrapToken = token
		s := New(cfg)
		s.Boot(context.Background())
		return s
	}}
}

func TestRenew_NoHooksSendsTodaysBody(t *testing.T) {
	env := setup(t, 10*24*time.Hour)
	env.hub.respond = func(map[string]any) (int, string) { return 200, env.hub.renewedJSON("") }
	s := env.newSvc(Config{})
	s.RefreshNow(context.Background())
	got := env.hub.calls("/v1/licenses/renew")
	if len(got) != 1 {
		t.Fatalf("renew calls = %d", len(got))
	}
	want := `{"instance_id":"` + env.instID.String() + `","version":"1.2.3"}`
	if string(got[0]) != want {
		t.Fatalf("body = %s, want %s", got[0], want)
	}
	if !s.Current().Valid {
		t.Fatalf("state after renew = %+v", s.Current())
	}
}

func TestRenew_OnRenewReceivesBrandAndExtrasAreSigned(t *testing.T) {
	env := setup(t, 10*24*time.Hour)
	brandEnv := `{"spec":1,"version":4,"sha256":"ab","pack":"e30","sig":"x"}`
	env.hub.respond = func(m map[string]any) (int, string) {
		if v, ok := m["brand_version"].(float64); ok && v < 4 {
			return 200, env.hub.renewedJSON(`,"brand":` + brandEnv)
		}
		return 200, env.hub.renewedJSON("")
	}
	var got []RenewResponse
	s := env.newSvc(Config{
		RenewExtra: func() map[string]any {
			return map[string]any{"brand_version": 3, "instance_id": "spoofed"}
		},
		OnRenew: func(_ context.Context, r RenewResponse) { got = append(got, r) },
	})
	s.RefreshNow(context.Background())

	bodies := env.hub.calls("/v1/licenses/renew")
	if len(bodies) != 1 {
		t.Fatalf("renew calls = %d", len(bodies))
	}
	var sent map[string]any
	if err := json.Unmarshal(bodies[0], &sent); err != nil {
		t.Fatal(err)
	}
	if sent["brand_version"] != float64(3) || sent["instance_id"] != env.instID.String() || sent["version"] != "1.2.3" {
		t.Fatalf("sent body = %s", bodies[0])
	}
	if len(got) != 1 || got[0].LicenseToken == "" || got[0].ExpiresAt == nil {
		t.Fatalf("OnRenew got %+v", got)
	}
	if string(got[0].Brand) != brandEnv {
		t.Fatalf("Brand = %s, want %s", got[0].Brand, brandEnv)
	}
}

func TestRenew_OldHubBrandNil(t *testing.T) {
	for name, extra := range map[string]string{"absent": "", "null": `,"brand":null`} {
		env := setup(t, 10*24*time.Hour)
		env.hub.respond = func(map[string]any) (int, string) { return 200, env.hub.renewedJSON(extra) }
		var got []RenewResponse
		s := env.newSvc(Config{
			RenewExtra: func() map[string]any { return map[string]any{"brand_version": 0} },
			OnRenew:    func(_ context.Context, r RenewResponse) { got = append(got, r) },
		})
		s.RefreshNow(context.Background())
		if len(got) != 1 || got[0].Brand != nil || got[0].LicenseToken == "" {
			t.Fatalf("%s: OnRenew got %+v", name, got)
		}
		if !s.Current().Valid {
			t.Fatalf("%s: renew must still apply: %+v", name, s.Current())
		}
	}
}

func TestRefresh_AfterRefreshWithoutRenew(t *testing.T) {
	env := setup(t, 300*24*time.Hour) // far from expiry: no renew
	env.hub.respond = func(map[string]any) (int, string) { return 200, env.hub.renewedJSON("") }
	after, renewed := 0, 0
	var s *Service
	s = env.newSvc(Config{
		OnRenew: func(context.Context, RenewResponse) { renewed++ },
		AfterRefresh: func(ctx context.Context) {
			after++
			if !s.Current().Valid {
				t.Error("AfterRefresh must see the recomputed state")
			}
		},
	})
	s.RefreshNow(context.Background())
	s.RefreshNow(context.Background())
	if after != 2 || renewed != 0 || len(env.hub.calls("/v1/licenses/renew")) != 0 {
		t.Fatalf("after=%d renewed=%d renewCalls=%d", after, renewed, len(env.hub.calls("/v1/licenses/renew")))
	}
}

func TestRefresh_RenewFailureSkipsOnRenewButRunsAfterRefresh(t *testing.T) {
	env := setup(t, 10*24*time.Hour)
	env.hub.respond = func(map[string]any) (int, string) { return 403, `{"error":"revoked"}` }
	after, renewed := 0, 0
	s := env.newSvc(Config{
		OnRenew:      func(context.Context, RenewResponse) { renewed++ },
		AfterRefresh: func(context.Context) { after++; panic("host bug") },
	})
	s.RefreshNow(context.Background()) // must not panic
	if renewed != 0 || after != 1 {
		t.Fatalf("renewed=%d after=%d", renewed, after)
	}
}

func TestSignedPost_AcceptedByHubVerifier(t *testing.T) {
	env := setup(t, 300*24*time.Hour)
	s := env.newSvc(Config{})
	body := map[string]any{"instance_id": env.instID.String(), "brand_version": 2}
	resp, err := s.SignedPost(context.Background(), "v1/licenses/brand", body)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	raw := json.RawMessage(`{"instance_id":"` + env.instID.String() + `","brand_version":7}`)
	resp, err = s.SignedPost(context.Background(), "/v1/licenses/brand", raw)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	got := env.hub.calls("/v1/licenses/brand")
	if len(got) != 2 || string(got[1]) != string(raw) {
		t.Fatalf("brand calls = %q", got)
	}

	unlinked := New(Config{HubBaseURL: env.hub.srv.URL})
	if _, err := unlinked.SignedPost(context.Background(), "/v1/licenses/brand", body); !errors.Is(err, ErrNotLinked) {
		t.Fatalf("unlinked: err = %v", err)
	}
}
