# Auditoría `installer.verifySignature` — `Signature.Checksums` per-archivo

- **Fecha:** 2026-05-04
- **Archivos auditados:**
  - [`installer/installer.go`](../../installer/installer.go) (`verifySignature`)
  - [`security/signature.go`](../../security/signature.go) (`VerifyBundle`)
  - [`manifest/manifest.go`](../../manifest/manifest.go) (`type Signature`)
  - [`bundle/bundle.go`](../../bundle/bundle.go) (`Read`)
- **Pregunta:** ¿`installer.verifySignature` enforcea `Signature.Checksums` (SHA-256 por entry del bundle) además del Ed25519 global?
- **Alcance:** sólo lectura, no se modifica código en esta tarea.

## Conclusión

**NO.** `installer.verifySignature` **no** valida `Signature.Checksums` per-archivo. El único control criptográfico aplicado hoy es el Ed25519 global sobre el SHA-256 del tarball completo. El campo `Signature.Checksums map[string]string` está declarado en el schema pero **no se lee en ninguna parte del kernel**: es metadata muerta que viaja con el manifest sin enforcement.

Esto significa que un atacante con capacidad de manipular el contenido individual de entries dentro del tarball **antes** de que se firme (insider en el publisher / pipeline de build comprometido) no es detectado por entry; sólo se detecta tampering **después** de la firma porque rompe el digest global. La granularidad por archivo que insinúa el campo `Checksums` no existe.

## Evidencia

### 1. `verifySignature` sólo delega en `security.VerifyBundle`

`installer/installer.go:354-375`:

```go
// verifySignature enforces the supply-chain trust model on a bundle before
// any other Install step touches the database or filesystem. The decision
// matrix is:
//
//	PublicKeys non-empty  → always verify; reject on missing or invalid sig.
//	PublicKeys empty + AllowUnsigned → permit (dev / sideload).
//	PublicKeys empty + !AllowUnsigned → reject every bundle (fail-closed).
func (i *Installer) verifySignature(b *bundle.Bundle) error {
	if len(i.PublicKeys) == 0 {
		if i.AllowUnsigned {
			return nil
		}
		return ErrSignatureRequired
	}
	if err := security.VerifyBundle(b, i.PublicKeys); err != nil {
		return fmt.Errorf("installer: bundle signature rejected: %w", err)
	}
	return nil
}
```

No hay otra rama: el camino feliz es exclusivamente `security.VerifyBundle`. Si esa función no inspecciona `Checksums`, el installer tampoco.

### 2. `security.VerifyBundle` jamás referencia `Checksums`

`security/signature.go:64-125` — la función completa, sin elisiones relevantes:

```go
func VerifyBundle(b *bundle.Bundle, trustedKeys []ed25519.PublicKey) error {
	if b == nil {
		return errors.New("security: nil bundle")
	}
	if b.Manifest.Signature == nil || strings.TrimSpace(b.Manifest.Signature.Value) == "" {
		return ErrUnsignedBundle
	}
	if len(b.Raw) == 0 {
		return errors.New("security: bundle.Raw is empty (was the bundle constructed without bundle.Read?)")
	}
	if len(trustedKeys) == 0 {
		return errors.New("security: no trusted public keys configured")
	}

	sig := b.Manifest.Signature

	alg := strings.ToLower(strings.TrimSpace(sig.Algorithm))
	if alg == "" {
		alg = "ed25519"
	}
	if alg != "ed25519" {
		return fmt.Errorf("%w: %q", ErrUnsupportedAlgorithm, sig.Algorithm)
	}

	sigBytes, err := hex.DecodeString(strings.TrimSpace(sig.Value))
	// ... length checks ...

	// Recompute the bundle digest from the original tarball bytes.
	sum := sha256.Sum256(b.Raw)
	digestHex := hex.EncodeToString(sum[:])

	if want := strings.TrimSpace(sig.Digest); want != "" && want != digestHex {
		// digest drift error
	}

	for _, pub := range trustedKeys {
		if len(pub) != ed25519.PublicKeySize {
			continue
		}
		if ed25519.Verify(pub, sum[:], sigBytes) {
			return nil
		}
	}
	return ErrSignatureMismatch
}
```

Los únicos campos consumidos del `Signature` son `Algorithm`, `Value` y `Digest`. `Checksums` es ignorado por completo.

### 3. El campo `Checksums` existe en el schema pero nadie lo lee

`manifest/manifest.go:292-302`:

```go
// Signature is the cryptographic provenance info stamped by the marketplace.
type Signature struct {
	DeveloperID   string            `json:"developer_id"`
	DeveloperName string            `json:"developer_name"`
	Verified      bool              `json:"verified"`
	SignedAt      string            `json:"signed_at"`
	Algorithm     string            `json:"algorithm"`
	Digest        string            `json:"digest"`
	Value         string            `json:"value"`
	Checksums     map[string]string `json:"checksums,omitempty"`
}
```

Búsqueda exhaustiva en el repo (`grep -rn "Checksums"`):

- `manifest/manifest.go:301` — declaración del campo.
- `docs/RELEASE.md:94` — referencia no relacionada (checksums de GoReleaser para el release de Go module, no del bundle).

Búsqueda de accesos al campo (`grep -rn "\.Checksums"`): **0 matches**. Nadie en el kernel hace `sig.Checksums[...]` ni itera el mapa.

### 4. El parser de bundle ya tiene los bytes per-entry pero no los hashea

`bundle/bundle.go:75-127` lee cada entry del tar (`manifest.json`, `migrations/*.sql`, `frontend/*`, `backend/*`, `README.md`) y los almacena en `b.Frontend[name]`, `b.Backend[name]`, etc. Sería trivial, en este mismo loop, computar `sha256(data)` y compararlo contra `b.Manifest.Signature.Checksums[h.Name]`. **Esa lógica no existe.**

### 5. Los tests confirman que el contrato actual ignora `Checksums`

`installer/signature_gate_test.go:73-84` — el test "happy path" arma una `manifest.Signature` con `Algorithm`, `Digest`, `Value`, **sin** `Checksums`, y `verifySignature` la acepta:

```go
b.Manifest.Signature = &manifest.Signature{
	Algorithm: "ed25519",
	Digest:    hex.EncodeToString(digest[:]),
	Value:     hex.EncodeToString(sig),
}
if err := i.verifySignature(b); err != nil {
	t.Fatalf("verifySignature: %v", err)
}
```

Ningún test del paquete `security` ni del paquete `installer` poblar `Checksums` ni ejercita drift per-archivo.

## Implicancias

El modelo actual confía exclusivamente en que el Ed25519 global cubra integridad por transitividad (cualquier byte cambiado en el tarball rompe el SHA-256 global). Eso es **suficiente contra MITM en la distribución** pero **insuficiente** para:

1. **Auditoría parcial / streaming verification**: un consumer que quiera validar sólo `frontend/remoteEntry.js` por separado (p. ej. CDN edge serving sin re-firmar) no tiene cómo.
2. **Detección granular post-mortem**: si el bundle se rompió después del unpack, no se puede aislar qué archivo fue alterado sin conservar el tarball original.
3. **Diff-friendly provenance**: dos bundles con un único asset distinto se diferencian sólo en el digest global, no por entry.
4. **Rotación parcial de assets WASM**: la visión menciona "add-ons WASM verificados por hash"; hoy ese hash sólo existe a nivel tarball, no a nivel `backend/backend.wasm`.

## Recomendación (no implementada en esta tarea)

1. En `bundle.Read`, computar `sha256` per entry y exponerlo como `Bundle.EntryDigests map[string]string`.
2. En `security.VerifyBundle`, si `sig.Checksums` está poblado, recorrer `EntryDigests` y exigir match exacto entry-por-entry; cualquier missing/extra/mismatch → error explícito (`ErrChecksumMismatch`).
3. Mantener el comportamiento legacy: si `Checksums` está vacío, no romper bundles antiguos — sólo enforcear cuando el publisher lo declara.
4. Sumar tests en `signature_gate_test.go` y `security/signature_test.go`: happy path con checksums correctos, drift en un único entry, missing entry, extra entry no firmado.
5. Coordinar con el hub publisher para que stamp de `Checksums` sea automático al firmar.
