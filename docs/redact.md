# `redact`: redaccion de PII y secretos

Paquete `github.com/asteby/metacore-kernel/redact`. Solo stdlib, aditivo.
Reemplaza las listas de regex duplicadas que cada capa llevaba por su cuenta.

```go
clean := redact.String(msg)                                  // reglas por defecto
r := redact.New(redact.WithIP(), redact.WithMaxRunes(500))   // motor reutilizable
agent.WithHandoffRedactor(redact.AsFunc(r))                  // sin importar `agent`
```

## API

| Simbolo | Para que |
|---|---|
| `type Redactor interface{ Redact(string) string }`, `Func` | Contrato minimo; `*Engine` lo implementa |
| `New(opts...) *Engine`, `Default()`, `String(s, opts...)` | Motor inmutable y seguro para uso concurrente; `String` sin opciones reusa el motor por defecto |
| `AsFunc(r) func(string) string` | Adaptador para `agent.WithHandoffRedactor` y similares. `nil` cae al motor por defecto, nunca deja pasar texto sin limpiar |
| `Legacy()` | Reproduce byte a byte la lista original (ver abajo) |
| `Only`, `Without`, `With`, `WithIP` | Seleccion de reglas (`ip` es opcional y viene apagada) |
| `WithRule(Rule)` | Regla propia; corre antes de las integradas |
| `WithMarker(rule, texto)` | Reemplazo literal de una regla |
| `WithStripControls`, `WithTrim`, `WithMaxInput`, `WithMaxBytes`, `WithMaxRunes` | Normalizacion y limites. El truncado corre despues de redactar, asi un secreto cortado a medias nunca se expone |
| `TruncateRunes`, `TruncateBytes`, `StripControls`, `LuhnValid` | Utilidades puras |
| `Spec()` | Descripcion de las reglas para portar a otros lenguajes |

Marcador por defecto: `[redacted:<regla>]` (`[redacted:email]`). Las reglas que
conservan contexto lo mantienen: `password=[redacted:secret]`,
`Bearer [redacted:bearer]`, `https://[redacted:url_credentials]@host`.

### Reglas (orden de aplicacion)

`private_key`, `url_credentials`, `url_signature`, `bearer`, `basic_auth`
(solo si el base64 decodifica a `usuario:clave`), `jwt`, `conn_string`,
`api_key`, `provider_key` (AWS, Google, GitHub, GitLab, `sk-`, npm, Slack app,
SendGrid, tiendas), `secret` (clave=valor), `secret_ext` (cookie, passphrase,
id_token, ...), `email`, `id_person`, `id_entity`, `card` (13-19 digitos con
Luhn), `number` (cualquier otra corrida de 13-19 digitos), `ip` (opcional;
valida con `netip`, por eso `10.1.2.3`-como-version o `12:30:45` no se tocan),
`phone`, `hex` (40+), `blob` (base64 de 48+), `blob_urlsafe` (48+ con letras y
digitos).

Las reglas marcadas `Extra` en `Spec()` no pertenecen a `Legacy()`.

## Paridad con las implementaciones anteriores

`Legacy()` es identica a `fleetevents.Redact` (OPS) y `fleet.Redact` (Hub); las
dos listas eran la misma (12 regex). Las dos reglas de identificadores
nacionales (`id_person`, `id_entity`) no llevan marcador legado en el kernel
porque esos nombres son vocabulario de nicho; el host que deba conservar el
suyo lo pasa:

```go
redact.New(redact.Legacy(),
    redact.WithMarker(redact.RuleIDPerson, "[CURP]"),
    redact.WithMarker(redact.RuleIDEntity, "[RFC]"))
```

`redact_test.go` compara la salida con una copia textual de la lista original
(`legacy_oracle_test.go`) sobre el corpus, todos sus prefijos/sufijos y la
union de todo; y `TestDefaultSupersetOfLegacy` exige que el motor por defecto
nunca deje intacto lo que la lista original redactaba. Mejoras sin perder
cobertura: tarjetas con Luhn, URLs con credenciales o firma, PEM, claves de
proveedores, cookies/passphrase, IP opcional y blobs base64url.

## Seguridad y rendimiento

- Todo es RE2 (`regexp` de Go): tiempo lineal, sin backtracking catastrofico.
  `TestNoCatastrophicBacktracking` corre entradas de 1 MiB patologicas con
  limite de tiempo y `TestLinearScaling` verifica el crecimiento lineal.
- `WithMaxInput(n)` acota lo escaneado y descarta el resto (nunca lo deja pasar
  sin escanear). Recomendado en rutas que reciben texto de terceros.
- El motor es inmutable; `TestConcurrent` lo ejerce con 16 goroutines.
  La maquina de desarrollo sin CGO no puede usar `-race`; corre
  `go test -race ./redact/...` en CI.

## Fixtures portables (PHP, TypeScript)

`redact/testdata/` es la fuente de verdad compartida:

- `vectors.json`: `profiles` (opciones por nombre: `legacy`, `ip`, `markers`,
  `strip_controls`, `trim`, `max_runes`) y `cases` (`profile`, `input`,
  `expected`). Un port implementa el perfil y debe producir `expected` exacto.
- `rules.json`: salida de `Spec()` (patron, flags, prefijo/sufijo, checksum,
  marcador). Un port puede cargarlo en vez de copiar las regex.

Se regeneran con `go test ./redact -run TestFixtures -update`; sin `-update` el
test falla si estan desactualizados. Los cases `legacy/*` ademas se comparan
con la implementacion original, no solo con el codigo nuevo.

Notas para portar:

- Los patrones son RE2. `\b` y `\d` son ASCII (en PHP/PCRE no uses `/u` con
  `\d`; en JS no uses el flag `u` con `\w`). `flags: "i"` y `"s"` son
  insensible a mayusculas y punto-con-salto-de-linea. `\x{00D1}` es `Ñ`.
- Los reemplazos de `prefix`/`suffix` usan `$1`; en PHP `$1`/`\1`, en JS `$1`.
- `check`: `luhn` (13-19 digitos, ignora espacios y guiones), `ip`
  (parseo estricto IPv4/IPv6), `basic_userpass` (el base64 contiene `:`),
  `mixed_alnum` (al menos una letra y un digito). Si el check falla, el texto
  se deja como esta.
- Truncado por runas (puntos de codigo), no por bytes ni UTF-16.
- PHP (`SupportRedactor`): cargar `vectors.json` en PHPUnit, construir el perfil
  y comparar. TS: igual con vitest. Cualquier diferencia es un bug del port.

## Guia de migracion (no hecha todavia)

No se migra OPS ni el Hub hasta publicar el kernel.

| Hoy | Despues |
|---|---|
| `fleetevents.Redact(s)` (OPS, `services/fleetevents/redact.go`) | `redact.New(redact.Legacy(), WithMarker(RuleIDPerson, "[CURP]"), WithMarker(RuleIDEntity, "[RFC]")).Redact(s)` para no cambiar lo almacenado; o el motor por defecto cuando el Hub acepte los marcadores nuevos |
| `fleet.Redact(s)` (Hub, `internal/fleet/fleet.go`) | el mismo motor; Hub y OPS deben usar la misma configuracion (la ingesta re-redacta) |
| `fleetevents.Truncate` / `fleet.Truncate` | `redact.TruncateBytes` |
| `supportClean(s, n)` + `stripSupportControls` + `truncateRunes` (Hub, `internal/api/support.go`, `ops.go`) | `redact.New(WithStripControls(), WithTrim(), WithMaxRunes(n))` (dar el mismo `Legacy()` si hay que conservar marcadores) |
| `supportSanitize` (sin redactar) | `redact.StripControls` + `TruncateRunes` |
| `sanitizeValue` / `SanitizeAttrs` (usan `Redact`+`Truncate`) | siguen locales (politica de atributos), llamando al motor |
| `SupportRedactor` (PHP, 7 Leguas) | portar contra `vectors.json` / `rules.json` |
| `agent.WithHandoffRedactor(fn)` | `agent.WithHandoffRedactor(redact.AsFunc(engine))` |

Las listas `sensitiveKey` / `safeKeys` (nombres de atributos) no forman parte
de este paquete: son politica de cada emisor.
