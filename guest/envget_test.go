package guest

import "testing"

func TestDecodeEnvValue_Present(t *testing.T) {
	got, ok := decodeEnvValue([]byte("sk_live_abc123"))
	if !ok {
		t.Fatal("found = false, want true")
	}
	if got != "sk_live_abc123" {
		t.Errorf("value = %q, want sk_live_abc123", got)
	}
}

func TestDecodeEnvValue_NilIsMissing(t *testing.T) {
	got, ok := decodeEnvValue(nil)
	if ok {
		t.Errorf("found = true, want false (host returned 0)")
	}
	if got != "" {
		t.Errorf("value = %q, want empty", got)
	}
}

func TestDecodeEnvValue_EmptyIsMissing(t *testing.T) {
	// Host folds empty values into the same "0" return as a missing
	// key (capabilities.go line 134-137). The decoder mirrors that —
	// callers can't distinguish empty from missing.
	got, ok := decodeEnvValue([]byte{})
	if ok {
		t.Errorf("found = true on empty buffer, want false")
	}
	if got != "" {
		t.Errorf("value = %q, want empty", got)
	}
}

func TestDecodeEnvValue_WhitespaceIsValuePresent(t *testing.T) {
	// A literal space is a non-empty value — the host returns it as
	// a 1-byte buffer, which decodes to "found".
	got, ok := decodeEnvValue([]byte(" "))
	if !ok {
		t.Errorf("found = false, want true for whitespace value")
	}
	if got != " " {
		t.Errorf("value = %q, want single space", got)
	}
}
