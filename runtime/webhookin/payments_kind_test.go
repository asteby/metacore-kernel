package webhookin

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"
)

type outDispatcher struct {
	out []byte
	err error
}

func (d outDispatcher) Dispatch(context.Context, uuid.UUID, string, map[string]any) error {
	return d.err
}
func (d outDispatcher) DispatchResult(context.Context, uuid.UUID, string, map[string]any) ([]byte, error) {
	return d.out, d.err
}

func dispatchOn(t *testing.T, kind string, out string) Response {
	t.Helper()
	r := New(map[string]Dispatcher{"wasm": outDispatcher{out: []byte(out)}}, nil)
	if err := r.Register(Route{Key: "mp", Path: "/mp", Verify: VerifyAddon, Do: "wasm:mp_webhook", Kind: kind}); err != nil {
		t.Fatal(err)
	}
	resp, err := r.DispatchRequest(context.Background(), uuid.New(), Inbound{Path: "/mp", Method: http.MethodPost, Body: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// The incident: the guest's errEnvelope used to leave as HTTP 200.
func TestPaymentsRouteMapsSuccessFalseTo503(t *testing.T) {
	resp := dispatchOn(t, KindPayments, `{"success":false,"error":"unauthorized"}`)
	if !resp.Envelope || resp.Status != http.StatusServiceUnavailable || !resp.Mapped {
		t.Fatalf("want mapped 503 envelope, got %+v", resp)
	}
	if resp.Cause != "unauthorized" || resp.Headers["Retry-After"] == "" {
		t.Errorf("cause/retry-after: %+v", resp)
	}
	if string(resp.BodyBytes()) == "" || string(resp.Output) == "" {
		t.Error("body and original output must be available")
	}
}

func TestPaymentsRouteLeavesGoodAnswersAlone(t *testing.T) {
	for _, out := range []string{
		`{"success":true}`,
		`{"status":401,"body":{"error":"unauthorized"}}`,
		`{"status":200,"body":{"success":false}}`, // an explicit envelope is the handler's decision
		`not json`,
		``,
		`{"success":false` + `}x`,
	} {
		resp := dispatchOn(t, KindPayments, out)
		if resp.Mapped {
			t.Errorf("%q must not be remapped: %+v", out, resp)
		}
	}
	if r := dispatchOn(t, KindPayments, `{"status":401,"body":{"error":"unauthorized"}}`); r.Status != 401 {
		t.Errorf("explicit 401 must pass through, got %d", r.Status)
	}
}

func TestPlainRouteKeepsLegacyBehaviour(t *testing.T) {
	resp := dispatchOn(t, "", `{"success":false,"error":"x"}`)
	if resp.Envelope || resp.Mapped {
		t.Fatalf("non-payments routes are untouched: %+v", resp)
	}
}

func TestLookupExposesKind(t *testing.T) {
	r := New(nil, nil)
	_ = r.Register(Route{Key: "mp", Path: "/mp", Kind: KindPayments})
	if got, ok := r.Lookup("/mp"); !ok || got.Kind != KindPayments {
		t.Fatalf("%+v %v", got, ok)
	}
	if _, ok := r.Lookup("/none"); ok {
		t.Fatal("unknown path")
	}
}
