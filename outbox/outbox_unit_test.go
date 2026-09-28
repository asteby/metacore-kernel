package outbox

import (
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func TestExponentialBackoff_GrowsWithJitterAndCap(t *testing.T) {
	b := ExponentialBackoff(time.Second, 10*time.Second)
	for attempt, want := range map[int]time.Duration{1: time.Second, 2: 2 * time.Second, 3: 4 * time.Second, 4: 8 * time.Second, 5: 10 * time.Second, 50: 10 * time.Second} {
		for range 50 {
			got := b(attempt)
			if got < want/2 || got > want {
				t.Fatalf("attempt %d: %s outside [%s, %s]", attempt, got, want/2, want)
			}
		}
	}
	if got := b(0); got > time.Second {
		t.Fatalf("attempt 0 treated as 1, got %s", got)
	}
}

func TestErrorWrappers(t *testing.T) {
	base := errors.New("boom")
	if Permanent(nil) != nil || RetryAfter(nil, time.Second) != nil {
		t.Fatal("wrapping nil must stay nil")
	}
	p := Permanent(base)
	if !IsPermanent(p) || !errors.Is(p, base) || p.Error() != "boom" {
		t.Fatal("Permanent must be detectable and unwrap")
	}
	if IsPermanent(base) {
		t.Fatal("plain error is not permanent")
	}
	d, ok := retryDelay(RetryAfter(base, 3*time.Second))
	if !ok || d != 3*time.Second {
		t.Fatalf("retryDelay = %s, %v", d, ok)
	}
	if _, ok := retryDelay(base); ok {
		t.Fatal("plain error has no retry delay")
	}
}

func TestEncodePayload(t *testing.T) {
	if b, err := encodePayload(map[string]int{"a": 1}); err != nil || string(b) != `{"a":1}` {
		t.Fatalf("struct payload = %s, %v", b, err)
	}
	if b, err := encodePayload(json.RawMessage(`{"x":true}`)); err != nil || string(b) != `{"x":true}` {
		t.Fatalf("raw payload = %s, %v", b, err)
	}
	if _, err := encodePayload([]byte("not json")); err == nil {
		t.Fatal("invalid []byte payload must fail")
	}
	if _, err := encodePayload(func() {}); err == nil {
		t.Fatal("unencodable payload must fail")
	}
}

func TestStatusValid(t *testing.T) {
	for _, s := range []Status{StatusPending, StatusRunning, StatusDone, StatusDead} {
		if !s.Valid() {
			t.Fatalf("%s should be valid", s)
		}
	}
	if Status("failed").Valid() {
		t.Fatal("unknown status must be invalid")
	}
}

func TestTruncateKeepsValidUTF8(t *testing.T) {
	long := ""
	for len(long) < maxErrorLen+10 {
		long += "ñ"
	}
	got := truncate(long)
	if len(got) > maxErrorLen || !json.Valid([]byte(`"`+got+`"`)) {
		t.Fatalf("truncate produced %d bytes / invalid utf-8", len(got))
	}
}

func TestPrefixed(t *testing.T) {
	if got := prefixed("j.", "id, kind,status"); got != "j.id, j.kind, j.status" {
		t.Fatalf("prefixed = %q", got)
	}
}

func TestNewRejectsNilDB(t *testing.T) {
	if _, err := New(nil, Config{}); err == nil {
		t.Fatal("nil db must fail")
	}
}
