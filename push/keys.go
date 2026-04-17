package push

import (
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
)

// GenerateVAPIDKeys returns a new P-256 keypair suitable for VAPID, encoded
// as base64url-nopad strings. Typical use is a one-off CLI at deploy time:
//
//	pub, priv, _ := push.GenerateVAPIDKeys()
//	// store pub/priv in your secret store, surface pub to the web client
//
// The public key is the 65-byte uncompressed point (0x04 || X || Y) which
// browsers expect in navigator.serviceWorker PushSubscription.
func GenerateVAPIDKeys() (publicKey, privateKey string, err error) {
	priv, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		return "", "", err
	}
	// PublicKey().Bytes() returns the uncompressed 65-byte point for P-256.
	publicKey = base64.RawURLEncoding.EncodeToString(priv.PublicKey().Bytes())
	privateKey = base64.RawURLEncoding.EncodeToString(priv.Bytes())
	return publicKey, privateKey, nil
}
