package push

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/base64"
)

// GenerateVAPIDKeys returns a new P-256 keypair suitable for VAPID, encoded
// as base64url-nopad strings. Typical use is a one-off CLI at deploy time:
//
//	pub, priv, _ := push.GenerateVAPIDKeys()
//	// store pub/priv in your secret store, surface pub to the web client
func GenerateVAPIDKeys() (publicKey, privateKey string, err error) {
	curve := elliptic.P256()
	priv, err := ecdsa.GenerateKey(curve, rand.Reader)
	if err != nil {
		return "", "", err
	}
	pub := elliptic.MarshalCompressed(curve, priv.PublicKey.X, priv.PublicKey.Y)
	// webpush-go expects uncompressed point (0x04 | X | Y) — emit both forms.
	uncompressed := elliptic.Marshal(curve, priv.PublicKey.X, priv.PublicKey.Y)
	_ = pub
	publicKey = base64.RawURLEncoding.EncodeToString(uncompressed)
	privateKey = base64.RawURLEncoding.EncodeToString(priv.D.Bytes())
	return publicKey, privateKey, nil
}
