package legacybridge

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

func TestSignVerify(t *testing.T) {
	secret := []byte("s3cr3t")
	now := time.Unix(1_800_000_000, 0)
	ts := strconv.FormatInt(now.Unix(), 10)
	body := []byte(`{"a":1}`)
	sig := Sign(secret, ts, body)
	if err := Verify(secret, ts, sig, body, now); err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct {
		secret []byte
		ts     string
		sig    string
		body   []byte
		now    time.Time
	}{
		"otro cuerpo":      {secret, ts, sig, []byte(`{"a":2}`), now},
		"otro secreto":     {[]byte("x"), ts, sig, body, now},
		"sin firma":        {secret, ts, "", body, now},
		"sin secreto":      {nil, ts, sig, body, now},
		"fuera de ventana": {secret, ts, sig, body, now.Add(MaxClockSkew + time.Second)},
		"ts no numérico":   {secret, "ayer", sig, body, now},
	} {
		if err := Verify(c.secret, c.ts, c.sig, c.body, c.now); !errors.Is(err, ErrBadSignature) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestHTTPConnector_SendsContractHeadersAndClassifiesResponses(t *testing.T) {
	secret := []byte("k")
	var status int
	var retryAfter string
	var seen *http.Request
	var seenBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r
		seenBody, _ = io.ReadAll(r.Body)
		if retryAfter != "" {
			w.Header().Set("Retry-After", retryAfter)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"success":true,"data":{"external_id":"erp:clients:9","source_version":"v9"}}`))
	}))
	defer srv.Close()
	h := &HTTPConnector{BaseURL: srv.URL + "/api/bridge/", Token: "tok", Secret: secret}
	c := Command{Kind: "customers.upsert", IdempotencyKey: "occ-1", Payload: json.RawMessage(`{"name":"N"}`)}

	status = 200
	d, err := h.Send(context.Background(), c)
	if err != nil || d.ExternalID != "erp:clients:9" || d.SourceVersion != "v9" {
		t.Fatalf("%+v %v", d, err)
	}
	if seen.URL.Path != "/api/bridge/customers/upsert" || seen.Header.Get(HeaderIdempotencyKey) != "occ-1" ||
		seen.Header.Get("Authorization") != "Bearer tok" || seen.Header.Get(HeaderCommand) != "customers.upsert" {
		t.Fatalf("petición = %s %v", seen.URL.Path, seen.Header)
	}
	if err := Verify(secret, seen.Header.Get(HeaderTimestamp), seen.Header.Get(HeaderSignature), seenBody, time.Now()); err != nil {
		t.Fatalf("la firma debe verificar del otro lado: %v", err)
	}

	status = 422
	if _, err := h.Send(context.Background(), c); !IsPermanent(err) {
		t.Fatalf("422 debe ser permanente: %v", err)
	}
	for _, s := range []int{500, 502, 401, 408} {
		status = s
		if _, err := h.Send(context.Background(), c); err == nil || IsPermanent(err) {
			t.Fatalf("%d debe ser transitorio: %v", s, err)
		}
	}
	status, retryAfter = 429, "7"
	_, err = h.Send(context.Background(), c)
	var ra retryAfterErr
	if !errors.As(err, &ra) || ra.delay != 7*time.Second {
		t.Fatalf("429 con Retry-After: %v", err)
	}
	if _, err := (&HTTPConnector{}).Send(context.Background(), c); !IsPermanent(err) {
		t.Fatal("sin BaseURL es permanente")
	}
}

func TestExponentialBackoff(t *testing.T) {
	b := Exponential(time.Second, 8*time.Second)
	for attempt, want := range map[int]time.Duration{0: time.Second, 1: time.Second, 2: 2 * time.Second, 4: 8 * time.Second, 30: 8 * time.Second} {
		for range 20 {
			if got := b(attempt); got < want/2 || got > want {
				t.Fatalf("intento %d: %s fuera de [%s, %s]", attempt, got, want/2, want)
			}
		}
	}
}
