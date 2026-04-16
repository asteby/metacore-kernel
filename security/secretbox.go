package security

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
)

// Encrypt wraps `data` with AES-256-GCM using `masterKey` (32 raw bytes or
// any length hashed down to 32 via the first 32 bytes of SHA-256 on the
// caller side — the kernel does NOT hash, so the caller must provide a
// 32-byte key). The output is `base64(nonce || ciphertext || tag)`.
//
// Intended for at-rest storage of per-installation webhook secrets. It is
// NOT a replacement for a proper KMS in production — hosts SHOULD swap the
// master key source for a KMS-backed resolver.
func Encrypt(masterKey, data []byte) (string, error) {
	if len(masterKey) != 32 {
		return "", fmt.Errorf("secretbox: master key must be 32 bytes, got %d", len(masterKey))
	}
	block, err := aes.NewCipher(masterKey)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	sealed := gcm.Seal(nonce, nonce, data, nil)
	return base64.StdEncoding.EncodeToString(sealed), nil
}

// Decrypt reverses Encrypt. Returns an error if the ciphertext is malformed
// or the tag fails verification (tampering).
func Decrypt(masterKey []byte, encoded string) ([]byte, error) {
	if len(masterKey) != 32 {
		return nil, fmt.Errorf("secretbox: master key must be 32 bytes, got %d", len(masterKey))
	}
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("secretbox: base64: %w", err)
	}
	block, err := aes.NewCipher(masterKey)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	ns := gcm.NonceSize()
	if len(raw) < ns {
		return nil, fmt.Errorf("secretbox: ciphertext too short")
	}
	nonce, ct := raw[:ns], raw[ns:]
	return gcm.Open(nil, nonce, ct, nil)
}
