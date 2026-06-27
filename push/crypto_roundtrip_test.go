package push

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"io"
	"testing"

	"golang.org/x/crypto/hkdf"
)

// TestEncryptPayloadRoundTrip verifies encryptPayload produces output the
// SUBSCRIBER can decrypt — i.e. it matches RFC 8291 + RFC 8188 so a real browser
// (which holds the subscription private key) can read it. A regression here is
// invisible end-to-end: the push service returns 201 but the User Agent silently
// drops the undecryptable message and no notification ever shows.
func TestEncryptPayloadRoundTrip(t *testing.T) {
	curve := ecdh.P256()

	// Simulate the browser's subscription keypair + auth secret.
	subPriv, err := curve.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	auth := make([]byte, 16)
	if _, err := rand.Read(auth); err != nil {
		t.Fatal(err)
	}
	p256dh := base64.RawURLEncoding.EncodeToString(subPriv.PublicKey().Bytes())
	authStr := base64.RawURLEncoding.EncodeToString(auth)

	plaintext := []byte(`{"title":"Hi","body":"round-trip test"}`)

	enc, err := encryptPayload(p256dh, authStr, plaintext)
	if err != nil {
		t.Fatalf("encryptPayload: %v", err)
	}

	// Parse the aes128gcm header: salt(16) | rs(4) | idlen(1) | keyid(idlen).
	body := enc.ciphertext
	if len(body) < 21 {
		t.Fatalf("payload too short: %d bytes", len(body))
	}
	salt := body[0:16]
	idlen := int(body[20])
	keyid := body[21 : 21+idlen] // ephemeral (application server) public key
	ciphertext := body[21+idlen:]

	ephPub, err := curve.NewPublicKey(keyid)
	if err != nil {
		t.Fatalf("parse ephemeral pubkey: %v", err)
	}
	shared, err := subPriv.ECDH(ephPub)
	if err != nil {
		t.Fatalf("ecdh: %v", err)
	}

	// Reproduce the key schedule from the receiver side.
	keyInfo := append([]byte("WebPush: info\x00"), subPriv.PublicKey().Bytes()...)
	keyInfo = append(keyInfo, keyid...)
	ikm := make([]byte, 32)
	if _, err := io.ReadFull(hkdf.New(sha256.New, shared, auth, keyInfo), ikm); err != nil {
		t.Fatal(err)
	}
	cek := make([]byte, 16)
	if _, err := io.ReadFull(hkdf.New(sha256.New, ikm, salt, []byte("Content-Encoding: aes128gcm\x00")), cek); err != nil {
		t.Fatal(err)
	}
	nonce := make([]byte, 12)
	if _, err := io.ReadFull(hkdf.New(sha256.New, ikm, salt, []byte("Content-Encoding: nonce\x00")), nonce); err != nil {
		t.Fatal(err)
	}

	block, err := aes.NewCipher(cek)
	if err != nil {
		t.Fatal(err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	dec, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		t.Fatalf("decrypt failed — a real browser would drop this push: %v", err)
	}
	dec = bytes.TrimRight(dec, "\x02") // strip the RFC 8188 padding delimiter
	if !bytes.Equal(dec, plaintext) {
		t.Fatalf("round-trip mismatch:\n got  %q\n want %q", dec, plaintext)
	}
}
