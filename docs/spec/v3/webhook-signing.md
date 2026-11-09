# Firma de acciones webhook salientes: `asteby-req-v1`

Contrato del runtime `runtime/webhookout` (RFC-01, fase 2) para `handler.type: webhook` con
`handler.signing.scheme: instance-ed25519-v1`. El nombre `asteby-req-v1` es el de la cadena
canonica y de la cabecera `X-Signature-Scheme`; `instance-ed25519-v1` es su nombre declarativo en el manifest.

La clave Ed25519 es la de la **instancia**: la inyecta el host mediante la interfaz `webhookout.Signer`.
El kernel no guarda, deriva ni registra llaves. Sin `Signer`, o con otro `scheme`, no sale ninguna
peticion (nunca se envia sin firma).

## Mensaje firmado

Ocho campos unidos por `\n` (sin salto final):

```
asteby-req-v1
<METHOD en mayusculas>
<path>
<rawQuery>
<Idempotency-Key>
<X-Actor>
<ts>
<hex(sha256(body))>
```

| Campo | Valor |
|---|---|
| `path` | Ruta **decodificada** tal como la ve el receptor (`r.URL.Path`), sin prefijo de proxy. |
| `rawQuery` | `r.URL.RawQuery` (vacio: las plantillas de `handler.path` no admiten query). |
| `Idempotency-Key` | La enviada en la cabecera (ver abajo). |
| `X-Actor` | Correo del usuario que opera, el enviado en la cabecera. |
| `ts` | Segundos Unix en decimal, el mismo texto de `X-Signature-Timestamp`. |
| `body` | Los bytes exactos enviados (JSON plano; `{}` si no hay campos). |

Como la firma cubre metodo, ruta, query, clave, actor, hora y hash del cuerpo, una captura no sirve contra otra
ruta, otro cuerpo, otro actor ni otra clave.

## Cabeceras

| Cabecera | Valor |
|---|---|
| `X-Signature-Scheme` | `asteby-req-v1` |
| `X-Signature` | `ed25519:` + base64 estandar de los 64 bytes de la firma |
| `X-Signature-Timestamp` | `ts` |
| `X-Instance-Pubkey` | clave publica, 64 caracteres hex en minuscula |
| `Idempotency-Key` | `[A-Za-z0-9._:-]{1,128}` |
| `X-Actor` | correo del staff (obligatorio en las cuatro escrituras) |
| `Content-Type` / `Accept` | `application/json` |

El receptor acepta una deriva de reloj de `MaxSkew` = 5 minutos y exige que la clave publica este en su lista
de instancias autorizadas.

## Idempotency-Key

`DeriveIdempotencyKey(addon, org, model, action, recordID, invocationID)` =
`"mc1-" + hex(sha256)` de las seis cadenas con prefijo de longitud (`<len>:<valor>|`). Es estable para una
misma invocacion logica: `invocationID` es el id de peticion que genero el cliente (o el `delivery_id`
guardado) y se reutiliza en cada reintento y en cada reenvio del mismo envio. Si el host no lo da, el cliente
genera uno aleatorio y lo devuelve en `Result.IdempotencyKey`. Dos acciones distintas sobre el mismo
registro llevan `invocationID` distinto y por tanto claves distintas.

## Peticion

- Metodos: `POST`, `PUT`, `PATCH`, `DELETE` (por omision `POST`).
- URL = base (resuelta por el host desde `url_setting` / `url_ref`; solo `scheme://host[:port]`, sin
  credenciales, ruta, query ni fragmento) + `handler.path`.
- `handler.path` admite `{record.<col>}` y `{payload.<campo>}`. Cada valor debe ser escalar, no vacio, sin `/`,
  `\`, controles ni ser `.` / `..`; se escapa con `url.PathEscape`. Cualquier otra cosa es error y no se envia.
- Cuerpo: el `payload` de la accion como **objeto JSON plano** (sin sobre, sin escape HTML). `confirm_text` se
  retira. Hasta 1 MiB.
- `timeout_ms` (1000..30000, por omision 10000) es el presupuesto total incluyendo reintentos; `retries`
  (0..3, por omision 2) solo ante error de red, 429 y 5xx, con espera de 250 ms y luego 1 s. Cada intento se
  vuelve a firmar con la hora actual y la misma `Idempotency-Key`.

## Resultado (fail-closed)

| Respuesta | Resultado |
|---|---|
| 2xx | `Result` (cuerpo hasta 64 KiB; valores de `license_key`, `license_token`, tokens y secretos sustituidos por `[redacted]`; sin JSON o truncado => `Body == nil`) |
| 4xx, 3xx (nunca se sigue) | `webhook_rejected` (HTTP 422), con el `error.message`/`message` del destino, maximo 300 caracteres |
| 429, 5xx, red | `webhook_unavailable` (502) tras los reintentos |
| presupuesto agotado | `webhook_timeout` (504) |
| configuracion, firma, destino fuera de politica | `webhook_misconfigured` (422); no sale nada |
| `confirm_typed` no satisfecho | `confirm_mismatch` (422); no sale nada |

Nunca hay un falso exito. El paquete no escribe en ningun log; los errores no incluyen cabeceras, claves ni
firmas.

## Politica de salida (SSRF)

`webhookout.Policy`, configurada por el host. Valor cero: solo `https`, solo destinos publicos; se deniegan
loopback, privadas, link-local (metadatos de nube), CGNAT, nombres de uso especial (`localhost`, `*.internal`,
etiquetas sueltas) y puertos distintos de 443. La comprobacion definitiva ocurre al conectar, sobre la IP
resuelta (`netguard.DialControl`), asi que el rebinding de DNS no la evita. Sin proxies; sin redirecciones.

- `AllowInternal`: lista **explicita** de nombres o IPs (con `:puerto` opcional) que el host autoriza, por
  ejemplo la base del nucleo de confianza. Sin comodines ni valores por omision.
- `AllowInsecureHTTP`: permite `http://` solo hacia un destino de `AllowInternal`.
- `AllowHosts`: estrecha los destinos publicos (sintaxis de `netguard`).

## `confirm_typed`

`VerifyConfirmTyped(ct, record, payload)` exige que `payload.confirm_text` (sin espacios en los extremos) sea
igual, con mayusculas, al valor **actual** de `ct.Field` en el registro. Un valor vacio o ausente en el registro
nunca se puede confirmar. El host la aplica en servidor para cualquier tipo de handler; `Client.Do` la aplica
sola cuando recibe `Request.ConfirmTyped`.

## Verificador de referencia y pruebas

`runtime/webhookout/reference_test.go` transcribe de forma independiente el algoritmo del receptor. Las pruebas
comprueban la ida y vuelta y que se rechazan: ruta distinta, id distinto, metodo distinto, query anadida,
cuerpo alterado, `X-Actor` alterado, `Idempotency-Key` alterada, otra clave publica, hora vieja y cabecera de
esquema ausente. `testdata/golden_sign.json` fija el mensaje, la firma (Ed25519 es determinista) y la clave
de idempotencia para una semilla de prueba (`UPDATE_GOLDEN=1` la regenera).
