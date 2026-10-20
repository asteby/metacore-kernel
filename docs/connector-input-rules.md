# `input_rules`: validación declarativa de credenciales, ajustes y campos

Bloque **opcional** que un manifest v3 puede declarar en:

- `settings[]` (ajustes del addon),
- `connectors[].credentials[]` (y `edge_devices[].pairing_credentials[]`, que usan la misma forma),
- `contributions.actions[].fields[]` (también en `steps[].fields[]` e `item_fields[]`).

Sin `input_rules` todo se comporta como antes. Con él, el host valida el valor
en el servidor al guardar (fuente de verdad) y la UI aplica las mismas reglas en
vivo. Tipo Go: `inputrules.Rules` (alias `v3.InputRules`), runtime en
`manifest/inputrules`.

```json
{
  "key": "access_token", "type": "secret", "required": true,
  "input_rules": {
    "trim": true,
    "pattern": "^(APP_USR|TEST)-[0-9]{8,}-[0-9]{4,}-[0-9a-f]{32}-[0-9]+$",
    "pattern_hint": "mercadopago.connector.hint.access_token",
    "examples": ["APP_USR-1234567890123456-101912-0123456789abcdef0123456789abcdef-123456789"],
    "max_length": 200,
    "autocomplete": "new-password",
    "mask": "full",
    "forbid_patterns": [
      { "pattern": "^(APP_USR|TEST)-[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$",
        "message": "mercadopago.connector.error.public_key_in_access_token" }
    ]
  }
}
```

| Campo | Significado |
|---|---|
| `trim` | Recorta espacios, tabs, saltos de línea, NBSP y caracteres de ancho cero al inicio y al final antes de validar y al guardar. Por omisión `true`. |
| `pattern` | Regex (≤ 512 caracteres) que debe cumplir el valor recortado. Ánclala con `^…$`. |
| `pattern_hint` | Clave i18n del addon (o texto) con el formato esperado. Se muestra bajo el campo y con el error. |
| `examples` | Hasta 5 valores válidos. El validador comprueba que cumplan todas las reglas. |
| `min_length` / `max_length` | Longitud en runas tras el recorte. `max_length` ≤ 4096 (tope duro del host). |
| `format` | Formato predefinido: `uuid`, `url` (http/https), `url_https`, `email`, `hex`, `numeric`, `alnum`. |
| `autocomplete` | `off` o `new-password`. Los campos secretos siempre usan `new-password` (evita que el navegador meta la contraseña de inicio de sesión). |
| `mask` | `none` (texto) o `full` (contraseña con mostrar/ocultar). Por omisión `full` en secretos. |
| `forbid_patterns` | Hasta 10 `{pattern, message}`: si el valor coincide, error `forbidden` con ese mensaje. Sirve para detectar el valor equivocado (p. ej. la Public Key pegada en el Access Token). |

## Subconjunto de regex

`pattern` y `forbid_patterns[].pattern` corren igual en Go (RE2) y en el
navegador (ECMAScript), así que solo se acepta el subconjunto común:

- sin flags en línea ni grupos especiales: solo `(?:…)` (no `(?i)`, `(?P<n>…)`, `(?=…)`);
- sin `\A`, `\z`, `\Q…\E`, `\C`, `\p{…}`, `\P{…}`, `\x{…}` ni clases POSIX `[[:alpha:]]`;
- sin cuantificadores anidados: una repetición (`*`, `+`, `{n,}`, `{n,m}` con m>1)
  dentro de otra **no acotada** (`*`, `+`, `{n,}`) se rechaza (`(a+)+`, `(x*)*`).
  Una repetición externa acotada como `(?:[0-9a-f]{4}-){3}` es válida.

El validador del manifest rechaza el addon si una regla no cumple. Si aun así
llega una regla inválida al host, el runtime la **ignora** (y lo reporta sin el
valor) para no bloquear al usuario.

## Errores (runtime)

`inputrules.Check(field, value, siblings)` devuelve la primera violación, sin el
valor: `required`, `forbidden`, `looks_like_other_field` (el valor no cumple sus
reglas pero sí las de otra credencial del mismo conector; `params.other_field`,
`params.other_label`), `pattern`, `format`, `min_length`, `max_length`.

Orden: recorte → vacío (`required` si es obligatorio) → tope de 4096 →
`forbid_patterns` → longitudes → `format` → `pattern`; si fallan longitudes,
formato o patrón, se prueba antes `looks_like_other_field`.

## Proyección al host

`manifest.CredentialDef.InputRules`, `manifest.SettingDef.InputRules` (y ahora
también `SettingDef.Description`, que antes se perdía), `manifest.FieldDef.InputRules`
y `modelbase.FieldDef.InputRules`, todos con la etiqueta JSON `input_rules`.

## Compatibilidad y orden de publicación

El JSON Schema v3 es **cerrado** (`additionalProperties: false`) y el decode usa
`DisallowUnknownFields`: un hub o un host con un kernel anterior **rechaza** un
manifest que declare `input_rules`. Por eso el orden es:

1. **kernel**: publicar la versión con `input_rules` (v0.223.0).
2. **hub**: subir su kernel a ≥ v0.223.0 (si no, rechaza la publicación del addon).
3. **ops** (y cualquier host): subir el kernel, validar en el servidor y en la UI.
4. **addons**: publicar con `input_rules` declarando el mínimo:

```json
"compatibility": {
  "requires": [
    { "key": "kernel", "version": ">=3.0.0 <4.0.0" },
    { "key": "metacore-kernel", "version": ">=0.223.0" }
  ]
}
```

Un addon sin `input_rules` no necesita nada de esto.

## Lint de traducciones

`go run github.com/asteby/metacore-kernel/cmd/manifest-i18ncheck [-literals] <dir-addon>...`
falla si alguna clave i18n de ajustes, credenciales, secciones del asistente,
acciones o de `input_rules` (`pattern_hint`, `forbid_patterns[].message`) falta
en alguno de los locales declarados (paquete `manifest/i18ncheck`).
