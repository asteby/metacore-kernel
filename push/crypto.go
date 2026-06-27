package push

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"io"

	"golang.org/x/crypto/hkdf"
)

// encryptedPayload holds the output of AES128GCM Web Push encryption.
type encryptedPayload struct {
	ciphertext []byte
	publicKey  string // base64url ephemeral public key (informational)
	salt       string // base64url salt (informational)
}

// encryptPayload encrypts a plaintext notification payload using the
// aesgcm / aes128gcm scheme defined in RFC 8291.
//
//   - p256dh: subscriber's P-256 public key (base64url, uncompressed 65 bytes)
//   - authSecret: 16-byte auth secret (base64url)
//   - payload: plaintext bytes to encrypt
func encryptPayload(p256dh, authSecret string, payload []byte) (*encryptedPayload, error) {
	subPubBytes, err := base64.RawURLEncoding.DecodeString(p256dh)
	if err != nil {
		return nil, err
	}
	authBytes, err := base64.RawURLEncoding.DecodeString(authSecret)
	if err != nil {
		return nil, err
	}

	// Ephemeral key pair for ECDH
	curve := ecdh.P256()
	ephPriv, err := curve.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	ephPub := ephPriv.PublicKey()

	// Subscriber's public key
	subPub, err := curve.NewPublicKey(subPubBytes)
	if err != nil {
		return nil, err
	}

	// ECDH shared secret
	shared, err := ephPriv.ECDH(subPub)
	if err != nil {
		return nil, err
	}

	// 16-byte random salt
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return nil, err
	}

	// Key derivation per RFC 8291 §3.4 (key agreement) + RFC 8188 §2.2 (aes128gcm).
	//
	// IMPORTANT: this previously mixed the legacy "aesgcm" draft step
	// (Content-Encoding: auth) into the combine and used the message salt where
	// the auth secret belongs, producing a CEK/nonce the browser could NOT
	// reproduce — so the User Agent failed to decrypt and silently dropped every
	// push (the push service still returns 201 Created, so it looked like it
	// "sent"). The correct derivation is a single combine keyed on auth_secret:
	//
	//   IKM = HKDF(salt=auth_secret, ikm=ecdh_secret,
	//              info="WebPush: info\0" || ua_public || as_public, 32)
	//   CEK   = HKDF(salt=salt, ikm=IKM, "Content-Encoding: aes128gcm\0", 16)
	//   NONCE = HKDF(salt=salt, ikm=IKM, "Content-Encoding: nonce\0", 12)
	//
	// key_info = "WebPush: info\0" || ua_public (subscriber) || as_public (ephemeral)
	keyInfo := append([]byte("WebPush: info\x00"), subPubBytes...)
	keyInfo = append(keyInfo, ephPub.Bytes()...)

	// IKM = HKDF(salt=auth_secret, IKM=ecdh_secret, info=key_info, 32)
	ikmReader := hkdf.New(sha256.New, shared, authBytes, keyInfo)
	ikm := make([]byte, 32)
	if _, err := io.ReadFull(ikmReader, ikm); err != nil {
		return nil, err
	}

	// CEK (16 bytes) and nonce (12 bytes), derived from the per-message salt.
	cekReader := hkdf.New(sha256.New, ikm, salt, []byte("Content-Encoding: aes128gcm\x00"))
	cek := make([]byte, 16)
	if _, err := io.ReadFull(cekReader, cek); err != nil {
		return nil, err
	}

	nonceReader := hkdf.New(sha256.New, ikm, salt, []byte("Content-Encoding: nonce\x00"))
	nonce := make([]byte, 12)
	if _, err := io.ReadFull(nonceReader, nonce); err != nil {
		return nil, err
	}

	// AES-GCM encrypt
	block, err := aes.NewCipher(cek)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	padded := append(payload, 0x02) // padding delimiter, no padding
	ciphertext := gcm.Seal(nil, nonce, padded, nil)

	// Build aes128gcm content-encoding header (RFC 8188):
	//   salt (16) | rs (4) | idlen (1) | keyid (65 = ephPub uncompressed)
	header := make([]byte, 0, 86)
	header = append(header, salt...)
	header = append(header, 0x00, 0x00, 0x10, 0x00) // record size 4096
	header = append(header, byte(len(ephPub.Bytes())))
	header = append(header, ephPub.Bytes()...)

	return &encryptedPayload{
		ciphertext: append(header, ciphertext...),
		publicKey:  base64.RawURLEncoding.EncodeToString(ephPub.Bytes()),
		salt:       base64.RawURLEncoding.EncodeToString(salt),
	}, nil
}
