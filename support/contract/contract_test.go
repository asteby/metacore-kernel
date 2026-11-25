package contract

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ---- fixtures ----

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestFixturesRoundTrip decodes every golden fixture strictly (no unknown
// fields: the Go types must cover the whole fixture) into its type and checks
// that re-encoding gives the same JSON document.
func TestFixturesRoundTrip(t *testing.T) {
	cases := map[string]func() any{
		"escalate_request_v1.json":             func() any { return &EscalateRequest{} },
		"escalate_request_v2.json":             func() any { return &EscalateRequest{} },
		"escalate_response_v1.json":            func() any { return &EscalateResponse{} },
		"escalate_response_v1_duplicate.json":  func() any { return &EscalateResponse{} },
		"escalate_response_v2_pending.json":    func() any { return &EscalateResponse{} },
		"message_request_v1.json":              func() any { return &MessageRequest{} },
		"message_request_v2.json":              func() any { return &MessageRequest{} },
		"message_response.json":                func() any { return &MessageResponse{} },
		"link_request.json":                    func() any { return &LinkRequest{} },
		"link_response_pending.json":           func() any { return &LinkResponse{} },
		"link_response_active.json":            func() any { return &LinkResponse{} },
		"link_ack_request.json":                func() any { return &LinkAckRequest{} },
		"callback_issue_commented.json":        func() any { return &CallbackEvent{} },
		"callback_issue_closed.json":           func() any { return &CallbackEvent{} },
		"callback_desk_message.json":           func() any { return &CallbackEvent{} },
		"callback_desk_resolved.json":          func() any { return &CallbackEvent{} },
		"callback_ai_action.json":              func() any { return &CallbackEvent{} },
		"desk_case_opened.json":                func() any { return &DeskIngestEvent{} },
		"desk_message.json":                    func() any { return &DeskIngestEvent{} },
		"error_not_linked.json":                func() any { return &ErrorResponse{} },
		"error_not_linked_v1.json":             func() any { return &ErrorResponse{} },
		"error_contract_upgrade_required.json": func() any { return &ErrorResponse{} },
		"error_channel_inactive.json":          func() any { return &ErrorResponse{} },
		"desk_case_opened_channel.json":        func() any { return &DeskIngestEvent{} },
		"thread_case.json":                     func() any { return &CaseView{} },
		"thread_message.json":                  func() any { return &MessageView{} },
		"embed_session_claims.json":            func() any { return &EmbedSessionClaims{} },
		"public_profile.json":                  func() any { return &PublicProfile{} },
		"visitor_erased.json":                  func() any { return &VisitorErasedEvent{} },
	}
	// Every fixture file except the vectors must be covered by the table.
	entries, err := os.ReadDir("testdata")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() == "signature_vectors.json" {
			continue
		}
		if _, ok := cases[e.Name()]; !ok {
			t.Errorf("fixture %s has no round-trip case", e.Name())
		}
	}
	for name, mk := range cases {
		t.Run(name, func(t *testing.T) {
			raw := readFixture(t, name)
			v := mk()
			dec := json.NewDecoder(bytes.NewReader(raw))
			dec.DisallowUnknownFields()
			if err := dec.Decode(v); err != nil {
				t.Fatalf("decode: %v", err)
			}
			out, err := json.Marshal(v)
			if err != nil {
				t.Fatal(err)
			}
			var want, got any
			if err := json.Unmarshal(raw, &want); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(out, &got); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(want, got) {
				t.Fatalf("round trip differs\nwant %s\ngot  %s", raw, out)
			}
		})
	}
}

// TestBackwardCompatV1 pins the v1 wire shape: a v1 body decodes with the v2
// fields at their zero value, and v1 responses never grow v2 keys.
func TestBackwardCompatV1(t *testing.T) {
	var req EscalateRequest
	if err := json.Unmarshal(readFixture(t, "escalate_request_v1.json"), &req); err != nil {
		t.Fatal(err)
	}
	if req.Contract != 0 || req.Severity != "" || req.Source != nil || req.Attachments != nil {
		t.Fatalf("v2 fields not zero in v1 body: %+v", req)
	}
	if req.Ticket.Folio != "TCK-0042" || len(req.Transcript) != 2 || req.Transcript[0].At != "" {
		t.Fatalf("v1 fields lost: %+v", req)
	}
	if got := EffectiveContract("", req.Contract); got != ContractV1 {
		t.Fatalf("v1 body contract = %d", got)
	}

	out, _ := json.Marshal(EscalateResponse{IssueURL: "u", IssueNumber: 3, Status: "open"})
	if string(out) != `{"issue_url":"u","issue_number":3,"status":"open"}` {
		t.Fatalf("v1 response shape changed: %s", out)
	}

	// A v1 client reading a v2 response sees the v1 keys with the same names.
	var m map[string]any
	if err := json.Unmarshal(readFixture(t, "escalate_response_v2_pending.json"), &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"issue_url", "issue_number", "status"} {
		if _, ok := m[k]; !ok {
			t.Errorf("v2 response lost v1 key %q", k)
		}
	}

	// Unknown future fields are ignored by the default decoder.
	var fut EscalateRequest
	if err := json.Unmarshal([]byte(`{"ticket":{"folio":"A1"},"callback":{"url":"x"},"future_field":{"a":1}}`), &fut); err != nil {
		t.Fatal(err)
	}
	var cb CallbackEvent
	if err := json.Unmarshal([]byte(`{"event":"brand_new","delivery_id":"d","timestamp":1,"extra":true}`), &cb); err != nil || cb.Event != "brand_new" {
		t.Fatalf("unknown event/field must decode: %v %+v", err, cb)
	}

	// v1 callbacks keep v1 keys even when empty (the PHP receiver reads them).
	out, _ = json.Marshal(CallbackEvent{Event: EventIssueClosed, DeliveryID: "d", Timestamp: 1})
	for _, k := range []string{`"ticket_folio"`, `"issue_url"`, `"issue_number"`, `"comment"`, `"solution"`, `"delivery_id"`, `"timestamp"`} {
		if !strings.Contains(string(out), k) {
			t.Errorf("v1 callback lost %s: %s", k, out)
		}
	}
	if strings.Contains(string(out), `"contract"`) {
		t.Errorf("v1 callback must not carry contract: %s", out)
	}
}

func TestMessageRequestID(t *testing.T) {
	var v1, v2 MessageRequest
	_ = json.Unmarshal(readFixture(t, "message_request_v1.json"), &v1)
	_ = json.Unmarshal(readFixture(t, "message_request_v2.json"), &v2)
	if v1.ID() != "msg:TCK-0042:7" || v2.ID() != "cm_01HZX" {
		t.Fatalf("ids: %q %q", v1.ID(), v2.ID())
	}
	both := MessageRequest{MessageID: "old", ClientMessageID: "new"}
	if both.ID() != "new" {
		t.Fatal("client_message_id must win")
	}
}

func TestFixtureValuesAreValid(t *testing.T) {
	var req EscalateRequest
	_ = json.Unmarshal(readFixture(t, "escalate_request_v2.json"), &req)
	if !ValidFolio(req.Ticket.Folio) || !ValidPriority(req.Ticket.Priority) || NormalizeSeverity(req.Severity) != SeverityHigh {
		t.Fatalf("v2 fixture invalid: %+v", req)
	}
	var ln LinkResponse
	_ = json.Unmarshal(readFixture(t, "link_response_pending.json"), &ln)
	if !ValidLinkSource(ln.Link.Source) || !ValidLinkState(ln.Link.State) {
		t.Fatalf("link fixture invalid: %+v", ln)
	}
	for _, f := range []string{"callback_issue_commented.json", "callback_issue_closed.json", "callback_desk_message.json", "callback_desk_resolved.json", "callback_ai_action.json"} {
		var ev CallbackEvent
		_ = json.Unmarshal(readFixture(t, f), &ev)
		if !IsCallbackEvent(ev.Event) {
			t.Errorf("%s: unknown event %q", f, ev.Event)
		}
	}
	var e ErrorResponse
	_ = json.Unmarshal(readFixture(t, "error_not_linked.json"), &e)
	if !IsKnownErrorCode(e.Error.Code) || e.Error.Reason != ReasonAutolinkOff {
		t.Fatalf("error fixture: %+v", e)
	}
}

// ---- validators (same expressions as the Hub) ----

func TestValidFolio(t *testing.T) {
	long64 := "A" + strings.Repeat("b", 63)
	cases := []struct {
		in string
		ok bool
	}{
		{"TCK-0042", true}, {"A", true}, {"7", true}, {"OPS/2026/0001", true}, {"a.b_c-d", true},
		{long64, true}, {long64 + "x", false},
		{"", false}, {"-lead", false}, {".lead", false}, {"/lead", false}, {"_lead", false},
		{"has space", false}, {"a<b", false}, {"a>b", false}, {"a@b", false}, {"a:b", false},
		{"a\nb", false}, {"a\n", false}, {"ñandú", false}, {"a#b", false},
	}
	for _, c := range cases {
		if got := ValidFolio(c.in); got != c.ok {
			t.Errorf("ValidFolio(%q) = %v, want %v", c.in, got, c.ok)
		}
	}
}

func TestValidMessageID(t *testing.T) {
	long128 := "m" + strings.Repeat("1", 127)
	cases := []struct {
		in string
		ok bool
	}{
		{"cm_01HZX", true}, {"msg:TCK-0042:7", true}, {"a", true}, {"9.9", true},
		{long128, true}, {long128 + "1", false},
		{"", false}, {":lead", false}, {"-lead", false}, {"_lead", false},
		{"a/b", false}, {"a b", false}, {"a@b", false}, {"a\n", false},
	}
	for _, c := range cases {
		if got := ValidMessageID(c.in); got != c.ok {
			t.Errorf("ValidMessageID(%q) = %v, want %v", c.in, got, c.ok)
		}
	}
}

func TestValidPriorityAndAuthor(t *testing.T) {
	for in, ok := range map[string]bool{"high": true, "P1_urgent-2": true, "": false, "a b": false, strings.Repeat("x", 32): true, strings.Repeat("x", 33): false} {
		if got := ValidPriority(in); got != ok {
			t.Errorf("ValidPriority(%q) = %v", in, got)
		}
	}
	for in, ok := range map[string]bool{"reporter": true, "agent": true, "ai": false, "": false, "Agent": false} {
		if got := ValidAuthorType(in); got != ok {
			t.Errorf("ValidAuthorType(%q) = %v", in, got)
		}
	}
}

func TestNormalizeSeverity(t *testing.T) {
	for in, want := range map[string]string{
		"blocker": "blocker", "HIGH": "high", " low ": "low", "normal": "normal",
		"": "normal", "critical": "normal", "urgent!": "normal",
	} {
		if got := NormalizeSeverity(in); got != want {
			t.Errorf("NormalizeSeverity(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestContractVersion(t *testing.T) {
	for in, want := range map[string]int{"": 1, "1": 1, "2": 2, " 2 ": 2, "abc": 1, "0": 1, "-3": 1, "3": 3, "2.0": 1} {
		if got := ParseContract(in); got != want {
			t.Errorf("ParseContract(%q) = %d, want %d", in, got, want)
		}
	}
	for _, c := range []struct {
		h    string
		body int
		want int
	}{{"", 0, 1}, {"", 2, 2}, {"2", 0, 2}, {"1", 2, 1}, {"junk", 2, 1}, {"", -1, 1}} {
		if got := EffectiveContract(c.h, c.body); got != c.want {
			t.Errorf("EffectiveContract(%q,%d) = %d, want %d", c.h, c.body, got, c.want)
		}
	}
}

// ---- constants are frozen: they are wire values ----

func TestWireConstantsFrozen(t *testing.T) {
	if !reflect.DeepEqual(CallbackEvents, []string{"issue_commented", "issue_closed", "desk_message", "desk_resolved", "desk_reopened", "ai_action"}) {
		t.Fatalf("callback events changed: %v", CallbackEvents)
	}
	codes := map[string]int{
		"unauthorized": 401, "replay": 408, "not_paired": 403, "not_linked": 403, "not_pending": 409,
		"callback_not_configured": 409, "in_progress": 409, "contract_upgrade_required": 409,
		"already_resolved": 409, "state_changed": 409, "callback_mismatch": 400, "callback_invalid": 400,
		"invalid_request": 400, "not_found": 404, "message_too_large": 413, "rate_limited": 429,
		"insufficient_sparks": 402, "github_error": 502, "unavailable": 503, "internal": 500,
		"origin_not_allowed": 403, "channel_inactive": 403, "session_expired": 401, "identity_invalid": 401,
		"captcha_required": 428, "channel_quota_exceeded": 402, "channel_ai_cap_reached": 402, "desk_not_verified": 409,
	}
	for code, status := range codes {
		if StatusForCode(code) != status || !IsKnownErrorCode(code) {
			t.Errorf("code %q: status %d known %v", code, StatusForCode(code), IsKnownErrorCode(code))
		}
	}
	if len(codes) != len(codeStatus) {
		t.Errorf("code table has %d entries, test pins %d", len(codeStatus), len(codes))
	}
	if StatusForCode("nope") != 0 || IsKnownErrorCode("nope") {
		t.Error("unknown code must have no status")
	}
	if PathEscalate != "/v1/support/escalate" || PathLinkAck != "/v1/support/link/ack" || HeaderHubSignature != "X-Hub-Signature" {
		t.Error("paths or headers changed")
	}
	if !ValidLinkSource("manual") || !ValidLinkSource("auto") || ValidLinkSource("x") || ValidLinkState("") {
		t.Error("link enums")
	}
}

func TestAPIError(t *testing.T) {
	e := NewAPIError(0, ErrCodeNotLinked, "x")
	if e.Status != 403 || e.Error() != "not_linked: x" {
		t.Fatalf("%+v", e)
	}
	var err error = e
	if !errors.Is(err, NewAPIError(0, ErrCodeNotLinked, "")) || errors.Is(err, NewAPIError(0, ErrCodeReplay, "")) {
		t.Fatal("errors.Is must match by code")
	}
	if errors.Is(err, &APIError{}) {
		t.Fatal("empty code sentinel must not match")
	}
}

// ---- signatures ----

type vectors struct {
	Callback []struct {
		Secret, Body, Event, Signature string
		DeliveryID                     string `json:"delivery_id"`
		Timestamp                      int64
	} `json:"callback"`
	Ack []struct {
		Secret, Proof string
		InstanceID    string `json:"instance_id"`
		TS            string `json:"ts"`
	} `json:"ack"`
}

func TestSignatureVectors(t *testing.T) {
	var v vectors
	if err := json.Unmarshal(readFixture(t, "signature_vectors.json"), &v); err != nil {
		t.Fatal(err)
	}
	if len(v.Callback) == 0 || len(v.Ack) == 0 {
		t.Fatal("no vectors")
	}
	for _, c := range v.Callback {
		if got := SignBody(c.Secret, []byte(c.Body)); got != c.Signature {
			t.Errorf("SignBody = %s, want %s", got, c.Signature)
		}
		ts := time.Unix(c.Timestamp, 0)
		h := SignCallback(c.Secret, []byte(c.Body), ts, c.DeliveryID, c.Event)
		if h.Get(HeaderHubSignature) != c.Signature || h.Get(HeaderHubTimestamp) != "1700000000" ||
			h.Get(HeaderHubDelivery) != c.DeliveryID || h.Get(HeaderHubEvent) != c.Event {
			t.Errorf("headers: %v", h)
		}
		m, err := VerifyCallback(c.Secret, []byte(c.Body), h, ts, 0)
		if err != nil || m.DeliveryID != c.DeliveryID || m.Event != c.Event || m.Timestamp != c.Timestamp || m.Contract != ContractV1 {
			t.Errorf("verify vector: %v %+v", err, m)
		}
	}
	for _, a := range v.Ack {
		if got := AckProof(a.Secret, a.InstanceID, a.TS); got != a.Proof {
			t.Errorf("AckProof = %s, want %s", got, a.Proof)
		}
		if !VerifyAckProof(a.Secret, a.InstanceID, a.TS, a.Proof) {
			t.Error("VerifyAckProof rejected the vector")
		}
	}
	// The ack fixture request carries the vector proof.
	var ack LinkAckRequest
	_ = json.Unmarshal(readFixture(t, "link_ack_request.json"), &ack)
	if ack.Proof != v.Ack[0].Proof {
		t.Error("link_ack_request.json out of sync with vectors")
	}
}

func signed(t *testing.T, secret string, ev CallbackEvent, at time.Time) ([]byte, http.Header) {
	t.Helper()
	ev.Timestamp = at.Unix()
	body, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	return body, SignCallback(secret, body, at, ev.DeliveryID, ev.Event)
}

func TestVerifyCallbackTable(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	const secret = "s3cret"
	good := CallbackEvent{Event: EventDeskMessage, DeliveryID: "d-1", Contract: 2}

	mk := func(at time.Time) ([]byte, http.Header) { return signed(t, secret, good, at) }
	cases := []struct {
		name   string
		secret string
		window time.Duration
		mut    func(body []byte, h http.Header, at time.Time) ([]byte, http.Header)
		at     time.Time
		want   error
	}{
		{"ok now", secret, 0, nil, now, nil},
		{"ok past edge 300s", secret, 0, nil, now.Add(-300 * time.Second), nil},
		{"ok future edge 300s", secret, 0, nil, now.Add(300 * time.Second), nil},
		{"past 301s", secret, 0, nil, now.Add(-301 * time.Second), ErrOutsideWindow},
		{"future 301s", secret, 0, nil, now.Add(301 * time.Second), ErrOutsideWindow},
		{"custom window ok", secret, time.Hour, nil, now.Add(-time.Hour), nil},
		{"custom window narrow", secret, 10 * time.Second, nil, now.Add(-11 * time.Second), ErrOutsideWindow},
		{"negative window means default", secret, -1, nil, now.Add(-299 * time.Second), nil},
		{"wrong secret", "other", 0, nil, now, ErrBadSignature},
		{"empty secret", "", 0, nil, now, ErrNoSecret},
		{"tampered body", secret, 0, func(b []byte, h http.Header, _ time.Time) ([]byte, http.Header) {
			return bytes.Replace(b, []byte("d-1"), []byte("d-2"), 1), h
		}, now, ErrBadSignature},
		{"missing signature", secret, 0, func(b []byte, h http.Header, _ time.Time) ([]byte, http.Header) {
			h.Del(HeaderHubSignature)
			return b, h
		}, now, ErrBadSignature},
		{"signature without prefix", secret, 0, func(b []byte, h http.Header, _ time.Time) ([]byte, http.Header) {
			h.Set(HeaderHubSignature, strings.TrimPrefix(h.Get(HeaderHubSignature), "sha256="))
			return b, h
		}, now, ErrBadSignature},
		{"uppercase hex rejected", secret, 0, func(b []byte, h http.Header, _ time.Time) ([]byte, http.Header) {
			h.Set(HeaderHubSignature, "sha256="+strings.ToUpper(strings.TrimPrefix(h.Get(HeaderHubSignature), "sha256=")))
			return b, h
		}, now, ErrBadSignature},
		{"missing timestamp header", secret, 0, func(b []byte, h http.Header, _ time.Time) ([]byte, http.Header) {
			h.Del(HeaderHubTimestamp)
			return b, h
		}, now, ErrBadTimestamp},
		{"garbage timestamp header", secret, 0, func(b []byte, h http.Header, _ time.Time) ([]byte, http.Header) {
			h.Set(HeaderHubTimestamp, "yesterday")
			return b, h
		}, now, ErrBadTimestamp},
		{"timestamp header differs from body", secret, 0, func(b []byte, h http.Header, at time.Time) ([]byte, http.Header) {
			h.Set(HeaderHubTimestamp, "1799999999")
			return b, h
		}, now, ErrTimestampMismatch},
		{"delivery header differs from body", secret, 0, func(b []byte, h http.Header, _ time.Time) ([]byte, http.Header) {
			h.Set(HeaderHubDelivery, "other")
			return b, h
		}, now, ErrDeliveryMismatch},
		{"delivery header absent is fine", secret, 0, func(b []byte, h http.Header, _ time.Time) ([]byte, http.Header) {
			h.Del(HeaderHubDelivery)
			return b, h
		}, now, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			body, h := mk(c.at)
			if c.mut != nil {
				body, h = c.mut(body, h, c.at)
			}
			m, err := VerifyCallback(c.secret, body, h, now, c.window)
			if !errors.Is(err, c.want) || (c.want == nil) != (err == nil) {
				t.Fatalf("err = %v, want %v", err, c.want)
			}
			if err == nil && (m.DeliveryID != "d-1" || m.Event != EventDeskMessage || m.Contract != 2) {
				t.Fatalf("meta %+v", m)
			}
		})
	}

	t.Run("body without delivery_id", func(t *testing.T) {
		body := []byte(`{"event":"x","timestamp":` + "1800000000" + `}`)
		h := SignCallback(secret, body, now, "", "x")
		if _, err := VerifyCallback(secret, body, h, now, 0); !errors.Is(err, ErrBadBody) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("body not json", func(t *testing.T) {
		body := []byte(`not json`)
		h := SignCallback(secret, body, now, "d", "x")
		if _, err := VerifyCallback(secret, body, h, now, 0); !errors.Is(err, ErrBadBody) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("signature checked before body is parsed", func(t *testing.T) {
		if _, err := VerifyCallback(secret, []byte(`not json`), http.Header{}, now, 0); !errors.Is(err, ErrBadSignature) {
			t.Fatalf("err = %v", err)
		}
	})
}

// TestVerifyMatchesHubReference re-implements the Hub's VerifySupportCallback
// (support_callback_worker.go) inline and checks both accept and reject the
// same inputs, so the migration of the Hub to this package changes nothing.
func TestVerifyMatchesHubReference(t *testing.T) {
	ref := func(secret string, body []byte, sig, tsHeader string, now time.Time, skew time.Duration) bool {
		if sig != SignBody(secret, body) {
			return false
		}
		var b struct {
			Timestamp int64 `json:"timestamp"`
		}
		if json.Unmarshal(body, &b) != nil || tsHeader != itoa(b.Timestamp) {
			return false
		}
		d := now.Sub(time.Unix(b.Timestamp, 0))
		return d <= skew && d >= -skew
	}
	now := time.Unix(1_800_000_000, 0)
	for _, off := range []int{-400, -300, -1, 0, 1, 300, 400} {
		at := now.Add(time.Duration(off) * time.Second)
		body, h := signed(t, "k", CallbackEvent{Event: EventIssueClosed, DeliveryID: "x", TicketFolio: "F1"}, at)
		_, err := VerifyCallback("k", body, h, now, 5*time.Minute)
		want := ref("k", body, h.Get(HeaderHubSignature), h.Get(HeaderHubTimestamp), now, 5*time.Minute)
		if (err == nil) != want {
			t.Errorf("offset %d: verify=%v ref=%v", off, err, want)
		}
	}
}

func itoa(n int64) string { b, _ := json.Marshal(n); return string(b) }

func TestAckProof(t *testing.T) {
	p := AckProof("sec", "inst-1", "100")
	if len(p) != 64 || p != strings.ToLower(p) {
		t.Fatalf("proof %q", p)
	}
	if !VerifyAckProof("sec", "inst-1", "100", p) || !VerifyAckProof("sec", "inst-1", "100", " "+p+" ") {
		t.Fatal("good proof rejected")
	}
	for _, bad := range []struct{ s, i, ts, p string }{
		{"sec2", "inst-1", "100", p}, {"sec", "inst-2", "100", p}, {"sec", "inst-1", "101", p},
		{"sec", "inst-1", "100", ""}, {"sec", "inst-1", "100", "zz"}, {"", "inst-1", "100", AckProof("", "inst-1", "100")},
	} {
		if VerifyAckProof(bad.s, bad.i, bad.ts, bad.p) {
			t.Errorf("accepted %+v", bad)
		}
	}
}

// ---- dedupe ----

func TestMemoryDeduper(t *testing.T) {
	clock := time.Unix(1_800_000_000, 0)
	d := &MemoryDeduper{Now: func() time.Time { return clock }}
	ctx := context.Background()
	if first, err := d.FirstSeen(ctx, "a", time.Hour); err != nil || !first {
		t.Fatalf("first: %v %v", first, err)
	}
	if first, _ := d.FirstSeen(ctx, "a", time.Hour); first {
		t.Fatal("repeat within ttl must not be first")
	}
	if first, _ := d.FirstSeen(ctx, "b", time.Hour); !first {
		t.Fatal("other id is first")
	}
	clock = clock.Add(time.Hour)
	if first, _ := d.FirstSeen(ctx, "a", time.Hour); !first {
		t.Fatal("expired id is first again")
	}
}

func TestMemoryDeduperFullFailsClosed(t *testing.T) {
	clock := time.Unix(1_800_000_000, 0)
	d := &MemoryDeduper{MaxEntries: 2, Now: func() time.Time { return clock }}
	ctx := context.Background()
	_, _ = d.FirstSeen(ctx, "a", time.Hour)
	_, _ = d.FirstSeen(ctx, "b", time.Hour)
	if _, err := d.FirstSeen(ctx, "c", time.Hour); !errors.Is(err, ErrDedupeFull) {
		t.Fatalf("err = %v", err)
	}
	clock = clock.Add(2 * time.Hour) // both expire, space is reclaimed
	if first, err := d.FirstSeen(ctx, "c", time.Hour); err != nil || !first {
		t.Fatalf("after expiry: %v %v", first, err)
	}
}

func TestMemoryDeduperConcurrentExactlyOne(t *testing.T) {
	d := &MemoryDeduper{}
	var wins atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if first, err := d.FirstSeen(context.Background(), "same", time.Minute); err == nil && first {
				wins.Add(1)
			}
		}()
	}
	wg.Wait()
	if wins.Load() != 1 {
		t.Fatalf("winners = %d, want 1", wins.Load())
	}
}

type failingDeduper struct{}

func (failingDeduper) FirstSeen(context.Context, string, time.Duration) (bool, error) {
	return false, errors.New("store down")
}

func TestAcceptCallback(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	body, h := signed(t, "k", CallbackEvent{Event: EventDeskResolved, DeliveryID: "d-9"}, now)
	dd := &MemoryDeduper{}
	ctx := context.Background()

	if m, err := AcceptCallback(ctx, dd, "k", body, h, now, 0); err != nil || m.DeliveryID != "d-9" {
		t.Fatalf("first: %v %+v", err, m)
	}
	if m, err := AcceptCallback(ctx, dd, "k", body, h, now, 0); !errors.Is(err, ErrDuplicate) || m == nil {
		t.Fatalf("repeat: %v %+v", err, m)
	}
	// A forged delivery must not burn the id of a real one.
	body2, h2 := signed(t, "k", CallbackEvent{Event: EventDeskResolved, DeliveryID: "d-10"}, now)
	if _, err := AcceptCallback(ctx, dd, "wrong", body2, h2, now, 0); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("forged: %v", err)
	}
	if _, err := AcceptCallback(ctx, dd, "k", body2, h2, now, 0); err != nil {
		t.Fatalf("real after forged: %v", err)
	}
	// Outside the window is rejected before dedupe records anything.
	body3, h3 := signed(t, "k", CallbackEvent{Event: EventDeskResolved, DeliveryID: "d-11"}, now.Add(-time.Hour))
	if _, err := AcceptCallback(ctx, dd, "k", body3, h3, now, 0); !errors.Is(err, ErrOutsideWindow) {
		t.Fatalf("stale: %v", err)
	}
	if first, _ := dd.FirstSeen(ctx, "d-11", time.Minute); !first {
		t.Fatal("stale delivery was recorded")
	}
	// nil deduper verifies only; a failing store surfaces so the Hub retries.
	if _, err := AcceptCallback(ctx, nil, "k", body, h, now, 0); err != nil {
		t.Fatalf("nil deduper: %v", err)
	}
	if _, err := AcceptCallback(ctx, failingDeduper{}, "k", body, h, now, 0); err == nil || errors.Is(err, ErrDuplicate) {
		t.Fatalf("store error: %v", err)
	}
}
