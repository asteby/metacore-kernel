package inbox

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/asteby/metacore-kernel/runtime/webhookin"
)

func env(status int, body string) webhookin.Response {
	raw := `{"status":` + itoa(status) + `,"body":` + body + `}`
	return webhookin.Response{Envelope: true, Status: status, Body: json.RawMessage(body), Output: []byte(raw)}
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

func TestFromDispatchPolicy(t *testing.T) {
	cases := []struct {
		name       string
		resp       webhookin.Response
		err        error
		wantEstado string
		wantAuth   string
		wantStatus int
	}{
		{"plain ok output", webhookin.Response{Output: []byte(`{"success":true}`)}, nil, EstadoProcesada, "", 200},
		{"no output", webhookin.Response{}, nil, EstadoProcesada, "", 200},
		{"envelope 200", env(200, `{"success":true}`), nil, EstadoProcesada, "", 200},
		{"duplicate", env(200, `{"outcome":"duplicate"}`), nil, EstadoDuplicada, "", 200},
		{"ignored", env(200, `{"outcome":"ignored"}`), nil, EstadoIgnorada, "", 200},
		{"bad signature reconciled by api -> 200 (D1)", env(401, `{"auth":"firma_invalida","outcome":"reconciled"}`), nil, EstadoReconciliadaAPI, AuthFirmaInvalida, 200},
		{"bad signature 2xx reconciled", env(200, `{"auth":"firma_invalida","outcome":"reconciled","resource_id":"1"}`), nil, EstadoReconciliadaAPI, AuthFirmaInvalida, 200},
		{"bad signature, not reconciled -> 401", env(401, `{"auth":"firma_invalida"}`), nil, EstadoRequiereRevision, AuthFirmaInvalida, 401},
		{"2xx with bad signature never acknowledged", env(200, `{"auth":"firma_invalida","outcome":"processed"}`), nil, EstadoRequiereRevision, AuthFirmaInvalida, 401},
		{"bare 401", env(401, `{"error":"x"}`), nil, EstadoRequiereRevision, AuthFirmaInvalida, 401},
		{"provider api down", env(503, `{}`), nil, EstadoReintentar, "", 503},
		{"429 becomes 503", env(429, `{}`), nil, EstadoReintentar, "", 503},
		{"400", env(400, `{}`), nil, EstadoRequiereRevision, "", 400},
		{"write failure forces retry", env(200, `{"write_failures":[{"table":"mp_payments","cause":"x"}]}`), nil, EstadoReintentar, "", 503},
		{"kernel signature error", webhookin.Response{}, webhookin.ErrSignatureInvalid, EstadoRequiereRevision, AuthFirmaInvalida, 401},
		{"missing signature", webhookin.Response{}, webhookin.ErrSignatureMissing, EstadoRequiereRevision, AuthSinFirma, 401},
		{"no secret", webhookin.Response{}, errors.Join(webhookin.ErrSecretNotConfigured, webhookin.ErrSignatureInvalid), EstadoRequiereRevision, AuthSinSecreto, 401},
		{"dispatcher error", webhookin.Response{}, errors.New("boom"), EstadoReintentar, "", 503},
		{"safety net errEnvelope unauthorized", webhookin.Response{Envelope: true, Status: 503, Mapped: true, Cause: "unauthorized",
			Output: []byte(`{"success":false,"error":"unauthorized"}`), Body: json.RawMessage(`{"success":false}`)}, nil, EstadoReintentar, AuthFirmaInvalida, 503},
		{"safety net other error", webhookin.Response{Envelope: true, Status: 503, Mapped: true, Cause: "http_error",
			Output: []byte(`{"success":false,"error":"http_error"}`), Body: json.RawMessage(`{"success":false}`)}, nil, EstadoReintentar, "", 503},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res, reply := FromDispatch(c.resp, c.err)
			if res.Estado != c.wantEstado || res.Auth != c.wantAuth || reply.Status != c.wantStatus {
				t.Fatalf("got estado=%s auth=%q status=%d (res %+v), want %s %q %d", res.Estado, res.Auth, reply.Status, res, c.wantEstado, c.wantAuth, c.wantStatus)
			}
		})
	}
}

func TestFromDispatchNeverAcknowledgesAFailure(t *testing.T) {
	// Every estado that needs more work must come with a non-2xx reply.
	for _, c := range []struct {
		resp webhookin.Response
		err  error
	}{
		{env(200, `{"write_failures":[{"table":"t","cause":"c"}]}`), nil},
		{env(200, `{"auth":"firma_invalida"}`), nil},
		{webhookin.Response{}, errors.New("x")},
	} {
		res, reply := FromDispatch(c.resp, c.err)
		if (res.Estado == EstadoReintentar || res.Estado == EstadoRequiereRevision) && reply.Status < 400 {
			t.Errorf("estado %s acknowledged with %d", res.Estado, reply.Status)
		}
	}
}

func TestParseStructuredData(t *testing.T) {
	s := ParseStructured([]byte(`{"success":true,"data":{"auth":"firma_ok","resource_id":"9","payment_id":"x"}}`))
	if s.Auth != "firma_ok" || s.ResourceID != "9" {
		t.Fatalf("%+v", s)
	}
	if got := ParseStructured([]byte(`nope`)); got.Auth != "" {
		t.Fatal("garbage must give zero value")
	}
}
