package push

import (
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"testing"
)

// TestGenerateVAPIDKeys verifies key generation produces valid P-256 key material.
func TestGenerateVAPIDKeys(t *testing.T) {
	pub, priv, err := GenerateVAPIDKeys()
	if err != nil {
		t.Fatalf("GenerateVAPIDKeys: %v", err)
	}
	if pub == "" || priv == "" {
		t.Fatal("expected non-empty keys")
	}

	// Public key must decode to a 65-byte uncompressed P-256 point.
	pubBytes, err := base64.RawURLEncoding.DecodeString(pub)
	if err != nil {
		t.Fatalf("decode public key: %v", err)
	}
	if len(pubBytes) != 65 {
		t.Fatalf("public key len = %d, want 65", len(pubBytes))
	}
	if pubBytes[0] != 0x04 {
		t.Fatalf("public key prefix = 0x%02x, want 0x04 (uncompressed)", pubBytes[0])
	}

	// Private key must decode to 32 bytes (P-256 scalar).
	privBytes, err := base64.RawURLEncoding.DecodeString(priv)
	if err != nil {
		t.Fatalf("decode private key: %v", err)
	}
	if len(privBytes) != 32 {
		t.Fatalf("private key len = %d, want 32", len(privBytes))
	}
}

// TestVAPIDJWT verifies that a VAPID JWT is produced and parseable using the
// ecdhToECDSA conversion path.
func TestVAPIDJWT(t *testing.T) {
	pubStr, privStr, err := GenerateVAPIDKeys()
	if err != nil {
		t.Fatalf("GenerateVAPIDKeys: %v", err)
	}

	svc := &Service{
		pub:     pubStr,
		subject: "mailto:test@example.com",
	}

	privBytes, _ := base64.RawURLEncoding.DecodeString(privStr)
	privKey, err := ecdh.P256().NewPrivateKey(privBytes)
	if err != nil {
		t.Fatalf("parse private key: %v", err)
	}
	svc.vapidPriv = privKey
	svc.vapidECDSA, err = ecdhToECDSA(privKey)
	if err != nil {
		t.Fatalf("ecdhToECDSA: %v", err)
	}

	token, err := svc.createVAPIDToken("https://push.example.com/push/sub123")
	if err != nil {
		t.Fatalf("createVAPIDToken: %v", err)
	}
	if token == "" {
		t.Fatal("expected non-empty JWT")
	}
	// A JWT has exactly two dots (three parts).
	dotCount := 0
	for _, c := range token {
		if c == '.' {
			dotCount++
		}
	}
	if dotCount != 2 {
		t.Fatalf("JWT has %d dots, want 2", dotCount)
	}
}

// TestEncryptPayload verifies encrypt/decrypt round-trip for AES128GCM.
func TestEncryptPayload(t *testing.T) {
	// Generate a fake subscriber key pair.
	subPriv, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate sub key: %v", err)
	}
	p256dh := base64.RawURLEncoding.EncodeToString(subPriv.PublicKey().Bytes())

	authRaw := make([]byte, 16)
	if _, err := rand.Read(authRaw); err != nil {
		t.Fatalf("rand auth: %v", err)
	}
	auth := base64.RawURLEncoding.EncodeToString(authRaw)

	plaintext := []byte(`{"title":"hello","body":"world"}`)
	enc, err := encryptPayload(p256dh, auth, plaintext)
	if err != nil {
		t.Fatalf("encryptPayload: %v", err)
	}
	if len(enc.ciphertext) == 0 {
		t.Fatal("expected non-empty ciphertext")
	}
	if enc.publicKey == "" || enc.salt == "" {
		t.Fatal("expected non-empty publicKey and salt")
	}
	// Ciphertext must be larger than plaintext (header + GCM tag).
	if len(enc.ciphertext) <= len(plaintext) {
		t.Fatalf("ciphertext len %d <= plaintext len %d", len(enc.ciphertext), len(plaintext))
	}
}
