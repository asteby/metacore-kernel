package setupflow

import (
	"errors"
	"strings"
	"testing"
)

func TestStatusHelpers(t *testing.T) {
	for _, s := range []Status{StatusPending, StatusRunning, StatusOK, StatusWarn, StatusError, StatusBlocked} {
		if !s.Valid() {
			t.Errorf("%s must be valid", s)
		}
	}
	if Status("done").Valid() || Status("").Valid() {
		t.Error("unknown statuses must be invalid")
	}
	settled := map[Status]bool{StatusOK: true, StatusWarn: true, StatusError: true, StatusBlocked: true}
	for _, s := range []Status{StatusPending, StatusRunning, StatusOK, StatusWarn, StatusError, StatusBlocked} {
		if s.Settled() != settled[s] {
			t.Errorf("%s settled = %v", s, s.Settled())
		}
	}
	if !StatusOK.Done() || !StatusWarn.Done() || StatusError.Done() || StatusPending.Done() {
		t.Error("only ok and warn are done")
	}
}

func TestParseCheckResult_Envelope(t *testing.T) {
	raw := `{"success":true,"data":{"status":"pending","message_key":"mail.dns.propagating","params":{"missing":2},"detail":"MX not visible","retry_after_s":60,"fix":{"label_key":"mail.dns.apply","action":"apply_dns_cloudflare"}},"meta":{"took_ms":12}}`
	r, err := ParseCheckResult([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != StatusPending || r.MessageKey != "mail.dns.propagating" || r.RetryAfterS != 60 || r.Fix == nil || r.Fix.Action != "apply_dns_cloudflare" {
		t.Fatalf("decoded = %+v", r)
	}
	if r.Params["missing"].(float64) != 2 {
		t.Fatalf("params = %v", r.Params)
	}
}

func TestParseCheckResult_Bare(t *testing.T) {
	r, err := ParseCheckResult([]byte(`{"status":"ok","message_key":"x.ok"}`))
	if err != nil || r.Status != StatusOK {
		t.Fatalf("bare result: %+v %v", r, err)
	}
}

func TestParseCheckResult_Rejects(t *testing.T) {
	longKey := strings.Repeat("a", MaxMessageKeyLen+1)
	cases := map[string]string{
		"empty":                 ``,
		"not an object":         `[1]`,
		"failure envelope":      `{"success":false,"error":"boom"}`,
		"envelope without data": `{"success":true}`,
		"null data":             `{"success":true,"data":null}`,
		"unknown status":        `{"status":"done","message_key":"x.y"}`,
		"missing status":        `{"message_key":"x.y"}`,
		"missing message_key":   `{"status":"ok"}`,
		"bad message_key":       `{"status":"ok","message_key":"has space"}`,
		"long message_key":      `{"status":"ok","message_key":"` + longKey + `"}`,
		"unknown field":         `{"status":"ok","message_key":"x.y","retry":5}`,
		"negative retry":        `{"status":"pending","message_key":"x.y","retry_after_s":-1}`,
		"huge retry":            `{"status":"pending","message_key":"x.y","retry_after_s":999999}`,
		"huge detail":           `{"status":"ok","message_key":"x.y","detail":"` + strings.Repeat("d", MaxDetailBytes+1) + `"}`,
		"fix on ok":             `{"status":"ok","message_key":"x.y","fix":{"label_key":"a.b","action":"a"}}`,
		"fix without target":    `{"status":"error","message_key":"x.y","fix":{"label_key":"a.b"}}`,
		"fix two targets":       `{"status":"error","message_key":"x.y","fix":{"label_key":"a.b","action":"a","step":"s"}}`,
		"fix http url":          `{"status":"error","message_key":"x.y","fix":{"label_key":"a.b","url":"http://x.com"}}`,
		"fix bad label":         `{"status":"error","message_key":"x.y","fix":{"label_key":"","action":"a"}}`,
		"trailing garbage":      `{"status":"ok","message_key":"x.y"} {"x":1}`,
	}
	for name, raw := range cases {
		if _, err := ParseCheckResult([]byte(raw)); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
	_, err := ParseCheckResult([]byte(`{"success":false,"error":"boom"}`))
	if !errors.Is(err, ErrHandlerFailed) || !strings.Contains(err.Error(), "boom") {
		t.Errorf("failure envelope error = %v", err)
	}
}

func TestFixValidate_OK(t *testing.T) {
	for _, f := range []Fix{
		{LabelKey: "a.b", Action: "apply"},
		{LabelKey: "a.b", Step: "dns"},
		{LabelKey: "a.b", URL: "https://docs.example.com/dns"},
	} {
		if err := f.Validate(); err != nil {
			t.Errorf("%+v: %v", f, err)
		}
	}
}

func TestHealthResult(t *testing.T) {
	raw := `{"success":true,"data":{"status":"warn","summary_key":"mail.health.degraded","metrics":[{"key":"queue.depth","value":12,"unit":"msgs"},{"key":"delivery_rate","value":98.2,"unit":"%"}],"issues":[{"severity":"warn","message_key":"mail.issue.dkim","params":{"domain":"a.com"},"fix":{"label_key":"mail.fix","step":"dns_check"}}]}}`
	h, err := ParseHealthResult([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if h.Status != HealthWarn || len(h.Metrics) != 2 || h.Issues[0].Fix.Step != "dns_check" {
		t.Fatalf("decoded = %+v", h)
	}
	bad := map[string]string{
		"unknown status":     `{"status":"great","summary_key":"a.b"}`,
		"unknown reserved":   `{"status":"unknown","summary_key":"a.b"}`,
		"no summary":         `{"status":"ok"}`,
		"metric bad key":     `{"status":"ok","summary_key":"a.b","metrics":[{"key":"Bad Key","value":1}]}`,
		"metric dup":         `{"status":"ok","summary_key":"a.b","metrics":[{"key":"a","value":1},{"key":"a","value":2}]}`,
		"issue bad severity": `{"status":"warn","summary_key":"a.b","issues":[{"severity":"ok","message_key":"a.b"}]}`,
		"issue no key":       `{"status":"warn","summary_key":"a.b","issues":[{"severity":"warn"}]}`,
		"ok hides error":     `{"status":"ok","summary_key":"a.b","issues":[{"severity":"error","message_key":"a.b"}]}`,
		"warn hides error":   `{"status":"warn","summary_key":"a.b","issues":[{"severity":"error","message_key":"a.b"}]}`,
		"ok hides warn":      `{"status":"ok","summary_key":"a.b","issues":[{"severity":"warn","message_key":"a.b"}]}`,
		"unknown field":      `{"status":"ok","summary_key":"a.b","score":1}`,
	}
	for name, raw := range bad {
		if _, err := ParseHealthResult([]byte(raw)); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
	if Worst(HealthOK, HealthError, HealthWarn) != HealthError || Worst() != HealthUnknown || Worst(HealthOK, HealthWarn) != HealthWarn {
		t.Error("Worst folds to the most severe status")
	}
}
