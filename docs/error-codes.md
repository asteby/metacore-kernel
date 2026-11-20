# Códigos de error estables y su catálogo es/en

El kernel devuelve un `code` estable (más `params`) en los errores de campo y de
filas; el texto (`message`) es solo un respaldo en inglés. Para mostrar el
mensaje en el idioma del usuario sin reimplementar el texto:

```go
msg, ok := errcodes.Translate("exceeds_remaining", "es", map[string]any{"line": 2, "requested": 3, "remaining": 1, "source": "«Venta»"})
```

`errcodes.Messages()` devuelve el catálogo completo (`lang → code → texto`, con
marcadores `{param}`) para fusionarlo en el bundle del host o del SDK
(`i18n.Compose`, `Catalog.Merge`). Fuente: `errcodes/locales/errors.json`; un
test exige paridad es/en y que cada código emitido por el kernel esté en el catálogo.

Las etiquetas de `ApprovalRequest` van por llaves `models.approval_requests.*`
(`dynamic/locales/approval_requests.json`, vía `DefineTranslations`).

Sin código estable (errores de desarrollador, texto en inglés, no para el usuario final):
`legacybridge`, `query.Aggregate`, y los errores Go de `importer` (topes, URL, números).

| code | en | es |
|---|---|---|
| `source_not_found` | The selected source document {source} was not found. | No se encontró el documento origen elegido {source}. |
| `source_not_eligible` | The selected source document {source} can no longer be used (for example it was already converted, cancelled or changed state). Pick another. | El documento origen elegido {source} ya no se puede usar (por ejemplo, ya se convirtió, se canceló o cambió de estado). Elige otro. |
| `source_line_not_found` | Line {line}: the source line no longer exists in {source}. | Renglón {line}: el renglón de origen ya no existe en {source}. |
| `exceeds_remaining` | Line {line}: quantity {requested} exceeds what is pending on {source} ({remaining}). Adjust the quantity; the rest is already recorded in another document. | Renglón {line}: la cantidad {requested} excede lo pendiente de {source} ({remaining}). Ajusta la cantidad; lo demás ya está registrado en otro documento. |
| `importer.required` | '{column}' is missing. | Falta '{column}'. |
| `importer.generator` | Could not generate the value of '{column}': {error} | No se pudo generar el valor de '{column}': {error} |
| `importer.number` | '{column}' must be a number (got: {value}). | '{column}' debe ser un número (recibido: {value}). |
| `importer.boolean` | '{column}' must be yes or no (got: {value}). | '{column}' debe ser sí o no (recibido: {value}). |
| `importer.date` | '{column}' must be a date formatted YYYY-MM-DD (got: {value}). | '{column}' debe ser una fecha con formato AAAA-MM-DD (recibido: {value}). |
| `importer.email` | '{column}' is not a valid email (got: {value}). | '{column}' no es un correo válido (recibido: {value}). |
| `importer.transform_unknown` | Transform '{transform}' of column '{column}' is not registered. | La transformación '{transform}' de la columna '{column}' no está registrada. |
| `importer.transform` | Transform '{transform}' of column '{column}' failed: {error} | La transformación '{transform}' de la columna '{column}' falló: {error} |
| `unknown_filter` | A list filter could not be applied (unknown column, operator or malformed value). | No se pudo aplicar un filtro de la lista (columna u operador desconocido, o valor mal formado). |
| `automation.invalid` | Invalid automation. | Automatización inválida. |
| `automation.not_found` | Automation not found. | Automatización no encontrada. |
| `automation.forbidden` | You do not have permission for this automation. | No tienes permiso para esta automatización. |
| `automation.limit` | Automation limit reached. | Límite de automatizaciones alcanzado. |
| `automation.conflict` | The automation changed; reload and try again. | La automatización cambió; recarga e intenta de nuevo. |
| `automation.bad_state` | The automation does not allow that action in its current state. | La automatización no admite esa acción en su estado actual. |
| `automation.disabled` | Automations are disabled. | Las automatizaciones están deshabilitadas. |
| `automation.internal` | Internal error. | Error interno. |

Parámetros: `source` (etiqueta del origen, puede venir vacío), `line` (1-based),
`requested` / `remaining` (cantidades), `column` (encabezado), `value` (celda
recibida), `transform`, `error`. En `automation`, el sobre HTTP de error añade
`code` (`automation.*`) junto al `message`.
