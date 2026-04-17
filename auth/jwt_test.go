package auth

import (
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

func TestGenerateAndValidateToken_Roundtrip(t *testing.T) {
	secret := []byte("test-secret-do-not-use-in-prod")
	userID := uuid.New()
	orgID := uuid.New()

	claims := Claims{
		UserID:         userID,
		OrganizationID: orgID,
		Email:          "alice@example.com",
		Role:           "owner",
	}

	token, expiresAt, err := GenerateToken(claims, secret, time.Hour)
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}
	if token == "" {
		t.Fatal("expected non-empty token")
	}
	if time.Until(expiresAt) <= 0 {
		t.Fatalf("expected future expiry, got %s", expiresAt)
	}

	parsed, err := ValidateToken(token, secret)
	if err != nil {
		t.Fatalf("ValidateToken: %v", err)
	}
	if parsed.UserID != userID {
		t.Errorf("UserID mismatch: got %s want %s", parsed.UserID, userID)
	}
	if parsed.OrganizationID != orgID {
		t.Errorf("OrgID mismatch: got %s want %s", parsed.OrganizationID, orgID)
	}
	if parsed.Email != "alice@example.com" {
		t.Errorf("Email mismatch: got %s", parsed.Email)
	}
	if parsed.Role != "owner" {
		t.Errorf("Role mismatch: got %s", parsed.Role)
	}
}

func TestValidateToken_Expired(t *testing.T) {
	secret := []byte("test-secret")
	past := time.Now().Add(-1 * time.Hour)
	claims := Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(past),
			IssuedAt:  jwt.NewNumericDate(past.Add(-time.Minute)),
		},
		UserID: uuid.New(),
	}
	token, _, err := GenerateToken(claims, secret, time.Hour)
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}

	_, err = ValidateToken(token, secret)
	if err != ErrExpiredToken {
		t.Fatalf("expected ErrExpiredToken, got %v", err)
	}
}

func TestValidateToken_WrongSecret(t *testing.T) {
	token, _, err := GenerateToken(Claims{UserID: uuid.New()}, []byte("a"), time.Hour)
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}
	if _, err := ValidateToken(token, []byte("b")); err == nil {
		t.Fatal("expected error validating with wrong secret")
	}
}

func TestValidateToken_Missing(t *testing.T) {
	if _, err := ValidateToken("", []byte("x")); err != ErrMissingToken {
		t.Fatalf("expected ErrMissingToken, got %v", err)
	}
}

func TestGenerateToken_EmptySecret(t *testing.T) {
	if _, _, err := GenerateToken(Claims{}, nil, time.Hour); err == nil {
		t.Fatal("expected error with empty secret")
	}
}
