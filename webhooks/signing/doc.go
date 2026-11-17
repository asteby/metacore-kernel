// Package signing es el catalogo CERRADO de perfiles de firma de los
// webhooks declarativos (RFC 0003, D3) y su verificacion simetrica.
//
// Un perfil es un dato (Spec), no codigo: anadir uno nuevo es un cambio del
// kernel con pruebas, nunca configuracion del cliente. Los secretos NO viajan
// en Spec: solo referencias (SecretRef / KeyRef) que el host resuelve a
// Secrets justo antes de firmar o verificar.
//
// # Perfiles
//
//	hmac_simple              hex(HMAC-SHA256(secret, body)) en Spec.Header
//	                         (por omision X-Signature; Spec.Prefix opcional, p. ej. "sha256=").
//	hmac_canonical_v1        hex(HMAC-SHA256(secret, "{ts}.{nonce}.{METHOD}.{path}.{sha256hex(body)}"))
//	                         con ts (unix s) y nonce en cabeceras. Es la firma de la API de canales de
//	                         7 Leguas V2 (ChannelSignature.php). Cabeceras por omision:
//	                         X-7L-Timestamp, X-7L-Nonce, X-7L-Signature.
//	bearer                   Authorization: Bearer <token>.
//	bearer+hmac_canonical_v1 ambos (V2 exige los dos).
//
// path es la ruta con "/" inicial y SIN query (use PathOf); METHOD va en
// mayusculas; en GET el cuerpo es la cadena vacia. El nonce son 16 a 64
// caracteres [A-Za-z0-9_-] y se regenera en cada intento de entrega (el
// receptor rechaza un nonce repetido); la idempotencia del evento NO depende
// del nonce sino de la llave de idempotencia, estable entre reintentos.
//
// # Interfaz para quien construye la RECEPCION
//
//	spec := signing.Spec{Profile: signing.HMACCanonicalV1}
//	err := signing.Verify(spec,
//	    signing.Secrets{HMACKey: key, BearerToken: tok}, // resueltos por referencia
//	    signing.Request{Method: r.Method, Path: signing.PathOf(r.URL), Body: body},
//	    r.Header,
//	    signing.VerifyOptions{
//	        Now:    time.Now(),                                // ventana +-300 s por omision (MaxSkew)
//	        Nonces: security.NewNonceCache(10 * time.Minute), // anti-replay; satisface NonceRecorder
//	    })
//	switch signing.CodeOf(err) {
//	// "unauthenticated" | "invalid_signature" | "stale_timestamp" | "nonce_reused" | "bad_request"
//	}
//
// Verify compara en tiempo constante, valida ventana de reloj y formato del
// nonce ANTES de registrarlo y registra el nonce solo con la firma valida
// (quien no tiene la llave no puede agotar la cache). Sign y Verify son
// simetricos: lo que Sign emite, Verify lo acepta con los mismos Spec y Secrets.
//
// El vector de prueba del contrato de V2 (docs/CONTRATO_ECOMMERCE_OPS.md §1)
// esta fijado en signing_test.go.
package signing
