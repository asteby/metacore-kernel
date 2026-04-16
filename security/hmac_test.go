package security_test

import (
	"testing"
	"time"

	"github.com/asteby/metacore-kernel/security"
)

func TestSigner_RoundTrip(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	s := security.NewSigner([]byte("super-secret"))
	s.Clock = func() time.Time { return now }

	headers := s.Sign("POST", "/hooks/order.created", []byte(`{"id":42}`))
	if err := s.Verify("POST", "/hooks/order.created", []byte(`{"id":42}`),
		headers["X-Metacore-Timestamp"], headers["X-Metacore-Signature"], time.Minute); err != nil {
		t.Fatalf("verify: %v", err)
	}
}

func TestSigner_TamperedBody(t *testing.T) {
	s := security.NewSigner([]byte("k"))
	headers := s.Sign("POST", "/x", []byte("a"))
	err := s.Verify("POST", "/x", []byte("b"),
		headers["X-Metacore-Timestamp"], headers["X-Metacore-Signature"], time.Minute)
	if err == nil {
		t.Fatal("expected signature mismatch on tampered body")
	}
}

func TestSigner_Skew(t *testing.T) {
	s := security.NewSigner([]byte("k"))
	s.Clock = func() time.Time { return time.Unix(1000, 0) }
	headers := s.Sign("GET", "/", nil)
	s.Clock = func() time.Time { return time.Unix(1000+3600, 0) }
	err := s.Verify("GET", "/", nil,
		headers["X-Metacore-Timestamp"], headers["X-Metacore-Signature"], time.Minute)
	if err == nil {
		t.Fatal("expected skew rejection")
	}
}
