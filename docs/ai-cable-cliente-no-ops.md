# IA para clientes no-OPS: protocolo de cable con el Hub

Guía de referencia para un sistema que **no** corre el kernel (7 Leguas en Laravel, un
script, otro backend) y quiere usar la IA de Asteby cobrada en Sparks (Pulse). Es el
mismo protocolo que usa OPS por dentro (`services/hubai`); el contrato de la capacidad
está en [capabilities-ai.md](capabilities-ai.md) y el diseño en
[RFC 0007](rfc/0007-capacidad-ai-complete.md).

Reglas que no cambian:

- **No hay llave de LLM en el cliente.** Solo una identidad Ed25519 de instancia. El Hub
  elige proveedor y modelo y guarda las llaves.
- **Sin saldo no es una excepción.** `insufficient_pulse`, `budget_exceeded`,
  `rate_limited`… se traducen a un `status` y la pantalla sigue funcionando sin IA.
- **No se manda nada que no haga falta.** Redacta antes (correos, teléfonos, RFC/CURP,
  tarjetas) y manda `redact_pii: true` si el texto trae datos personales. El Hub no guarda
  el texto de las llamadas con `addon_key`.

## 1. Identidad

Una llave Ed25519 por instalación (no por usuario). En PHP: `sodium_crypto_sign_keypair()`
una vez; guarda la secreta (64 bytes) en el almacén de secretos del sistema, nunca en el
repositorio. La pública (32 bytes, en hex) es la identidad que el Hub empareja: o la
registra el operador del Hub, o el Hub la auto-empareja en la primera llamada firmada
válida (`HUB_AI_AUTO_PAIR`, ver `hub/docs/ABY-AI.md`).

El monedero del Hub va por **(producto, org)**: acuerda con el operador del Hub el
`metadata.product` y el `org_id` que usará el cliente; sin monedero la respuesta es
`no_wallet` o el comportamiento heredado sin Sparks.

## 2. Petición

```
POST {HUB}/v1/ai/complete
Content-Type: application/json
X-Instance-Pubkey: <hex de la llave pública>
X-Signature-Timestamp: <segundos unix>
X-Signature: ed25519:<base64(firma)>
```

Firma: `ed25519_sign(secreta, sha256(cuerpo || "\n" || timestamp))`, donde `cuerpo` son
**los bytes exactos** que se envían y `timestamp` la misma cadena de la cabecera. El Hub
acepta ±5 minutos de desfase (fuera de eso: 408 `replay`).

Cuerpo (campos de `ai.Request` del Hub; los de la extensión aditiva son opcionales y un
Hub anterior los ignora):

| Campo | Notas |
|---|---|
| `org_id` | org que paga (uuid del directorio del Hub) |
| `tier` | `fast` \| `balanced` \| `heavy` |
| `messages` | `[{"role":"system"\|"user"\|"assistant","content":[{"type":"text","text":"…"}]}]` |
| `max_tokens`, `temperature` | opcionales |
| `addon_key`, `feature` | atribución del gasto (p. ej. `7l_tickets`, `ticket_triage`) |
| `idempotency_key` | misma clave = mismo resultado sin volver a cobrar; derivarla de lo que se procesa |
| `response_format` | `{"type":"json_object"}` para respuesta JSON |
| `redact_pii` | `true` si el texto trae datos personales |
| `metadata` | `{"product":"<acordado>"}` |

## 3. Respuesta

- **200**: `{"request_id","model","stop_reason","content":[{"type":"text","text":"…"}],"usage":{"input_tokens","output_tokens"},"cost_sparks"}`.
- **Error**: `{"error":{"code","message"}}`. Traducción a `status` (la misma que hace OPS):

| HTTP | `code` | `status` |
|---|---|---|
| 402 | `insufficient_pulse` | `insufficient_pulse` |
| 429 | `budget_exceeded` | `budget_exceeded` |
| 429 | `rate_limited` | `rate_limited` (respeta `Retry-After` si viene) |
| 429 / 403 | `budget_disabled` / `banned` | `disabled` |
| (según el contrato del Hub) | `no_wallet` | `no_wallet` |
| 400 | `invalid_request` | error de programación: corrige la petición |
| 401 / 403 / 408 | `unauthorized` / `not_paired` / `replay` | error de configuración (llave, emparejamiento, reloj) |
| 5xx, red | `unavailable` u otro | `unavailable` |

## 4. Fragmento PHP / Laravel

```php
use Illuminate\Support\Facades\Http;

function aiComplete(string $hub, string $secretKey, array $payload): array
{
    // Firmar EXACTAMENTE lo que se envía.
    $body = json_encode($payload, JSON_UNESCAPED_UNICODE | JSON_UNESCAPED_SLASHES | JSON_THROW_ON_ERROR);
    $ts   = (string) time();
    $sig  = sodium_crypto_sign_detached(hash('sha256', $body . "\n" . $ts, true), $secretKey);
    $pub  = sodium_crypto_sign_publickey_from_secretkey($secretKey);

    try {
        $res = Http::withHeaders([
            'X-Instance-Pubkey'     => bin2hex($pub),
            'X-Signature-Timestamp' => $ts,
            'X-Signature'           => 'ed25519:' . base64_encode($sig),
        ])->withBody($body, 'application/json')->timeout(120)
          ->post(rtrim($hub, '/') . '/v1/ai/complete');
    } catch (\Throwable $e) {
        return ['status' => 'unavailable', 'message' => $e->getMessage()];
    }

    if ($res->successful()) {
        $text = collect($res->json('content'))->where('type', 'text')->pluck('text')->implode('');
        return [
            'status'      => 'ok',
            'content'     => $text,
            'cost_sparks' => (int) $res->json('cost_sparks', 0),
            'request_id'  => $res->json('request_id'),
        ];
    }

    $code = (string) ($res->json('error.code') ?? '');
    $status = match ($code) {
        'insufficient_pulse', 'budget_exceeded', 'rate_limited', 'no_wallet' => $code,
        'budget_disabled', 'banned' => 'disabled',
        'invalid_request', 'unauthorized', 'not_paired', 'replay'
            => throw new \LogicException("ai.complete $code: " . $res->json('error.message')),
        default => 'unavailable',
    };
    return ['status' => $status, 'message' => $res->json('error.message'),
            'retry_after_seconds' => (int) $res->header('Retry-After')];
}
```

`withBody` es necesario: `post($array)` re-serializa el arreglo y la firma deja de coincidir.

## 5. Vector de prueba

Para comprobar la firma sin llamar al Hub (verificado con Go `crypto/ed25519` y con PHP
`sodium`):

| Dato | Valor |
|---|---|
| semilla (hex) | `0102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f20` |
| llave pública (hex) | `79b5562e8fe654f94078b112e8a98ba7901f853ae695bed7e0e3910bad049664` |
| cuerpo | `{"org_id":"00000000-0000-0000-0000-000000000001","tier":"fast","feature":"ticket_triage","messages":[{"role":"user","content":[{"type":"text","text":"hola"}]}]}` |
| timestamp | `1764000000` |
| sha256(cuerpo + "\n" + timestamp) | `70e3a5eb669a48113cd9975469a155e6cae5a82dd5337f160e3c98bed316afaa` |
| `X-Signature` | `ed25519:q0xOznV6B0Lfdj8SAXbNnxcjiWYWvVBBtN4a2LuE0qRqV7XqA3x30ayYcJIU+DPVijvH1toYcuRQXoECrbMGCg==` |

```php
$kp  = sodium_crypto_sign_seed_keypair(hex2bin('0102…1f20'));
$sig = sodium_crypto_sign_detached(hash('sha256', $body . "\n" . '1764000000', true), sodium_crypto_sign_secretkey($kp));
// 'ed25519:' . base64_encode($sig) === el valor de la tabla
```

## 6. Qué falta del lado del Hub

- `cost_sparks`, `idempotency_key` con `replay`, `addon_key`, `feature`, `response_format`
  y `redact_pii` llegan con la extensión aditiva de `/v1/ai/complete` (rama del Hub
  `feat/soporte-escalado`, `docs/AI-COMPLETE-CONTRATO.md`). Antes de que se despliegue, el
  Hub ignora esos campos y la respuesta no trae `cost_sparks`.
- El patrocinio (`sponsor_org_id`) todavía no existe en el Hub.
