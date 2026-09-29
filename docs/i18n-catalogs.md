# Catálogos i18n para metadata

Cómo traducir las etiquetas de `TableMetadata` / `ModalMetadata` sin escribir
un traductor propio: catálogos en memoria (`i18n.Catalog`), mensajes declarados
junto al modelo (`DefineTranslations` / `host.WithTranslations`), composición
con el bundle del host (`i18n.Compose`), carga desde archivos embebidos
(`i18n.LoadCatalogFS`) y etiquetas generadas para llaves sin traducción
(`AppConfig.I18nHumanizeMissing`).

---

## Contenido

- [El problema](#el-problema)
- [Mensajes junto al modelo](#mensajes-junto-al-modelo)
- [Cadena de idiomas (fallback)](#cadena-de-idiomas-fallback)
- [Orden de las capas](#orden-de-las-capas)
- [`i18n.Catalog` e interpolación](#i18ncatalog-e-interpolación)
- [`i18n.Compose`](#i18ncompose)
- [Catálogos desde archivos: `LoadCatalogFS`](#catálogos-desde-archivos-loadcatalogfs)
- [Etiquetas humanizadas: `I18nHumanizeMissing`](#etiquetas-humanizadas-i18nhumanizemissing)
- [Compatibilidad](#compatibilidad)

---

## El problema

Las etiquetas de la metadata de los modelos son llaves i18n
(`"models.branches.modal.fields.name"`). `host.AppConfig` sólo aceptaba un
`Translator`, así que una app sin bundle completo servía las llaves crudas, y
cada app terminaba escribiendo un `TranslatorFunc` ad hoc con un mapa y
registrando los transformers a mano.

## Mensajes junto al modelo

Un modelo declara su catálogo (`idioma → llave → texto`) implementando
`modelbase.HasTranslations`:

```go
func (Branch) DefineTable() modelbase.TableMetadata {
    return modelbase.TableMetadata{
        Title:   "models.branches.table.title",
        Columns: []modelbase.ColumnDef{{Key: "name", Label: "models.branches.table.columns.name"}},
    }
}

func (Branch) DefineTranslations() map[string]map[string]string {
    return map[string]map[string]string{
        "es": {"table.title": "Sucursales", "table.columns.name": "Nombre"},
        "en": {"table.title": "Branches", "table.columns.name": "Name"},
    }
}
```

o se registra con la opción `host.WithTranslations` (útil cuando el modelo vive
en otro paquete o los textos son de la app):

```go
app.RegisterModel("branches", newBranch, host.WithTranslations(map[string]map[string]string{
    "es": {"modal.create_title": "Nueva sucursal"},
}))
```

Las llaves pueden ser **relativas al modelo** (`table.title`,
`modal.fields.name`) y se expanden a `models.<llave de registro>.<...>`; las
que ya empiezan con el prefijo (`models.…`, o `AppConfig.I18nKeyPrefix`) se
dejan tal cual. `RegisterModel` las mezcla en el traductor de la app:
`WithTranslations` pisa a `DefineTranslations` en la misma llave.

Para mensajes de toda la app (no de un modelo) está
`app.AddTranslations(msgs)`, con llaves completas.

No hace falta configurar nada más: al registrar el primer catálogo el kernel
instala los transformers de metadata. El middleware `Accept-Language` se monta
siempre en `app.Mount`, así que el orden entre `Mount` y `RegisterModel` da
igual.

## Cadena de idiomas (fallback)

Para una petición `Accept-Language: es-MX` la búsqueda es:

```
es-MX → es → AppConfig.I18nDefaultLanguage → en
```

Es decir: la etiqueta exacta, sus prefijos más cortos, el idioma por defecto
de la app (default `"en"`) y finalmente inglés. Una petición en `fr` en una app
con `I18nDefaultLanguage: "es"` recibe español. Las etiquetas se comparan sin
distinguir mayúsculas y `_` equivale a `-` (`es_MX` = `es-mx`).

## Orden de las capas

El traductor efectivo (`app.Translator()`) es:

1. `AppConfig.Translator` (el bundle del host), si existe;
2. los catálogos registrados (`DefineTranslations`, `WithTranslations`,
   `AddTranslations`);
3. con `I18nHumanizeMissing`, una etiqueta generada a partir de la llave.

La primera capa que traduce gana. Así el host puede corregir cualquier texto
que un modelo o addon trae por defecto. Una app que quiera el orden inverso
compone su propio `Translator` (ver [`i18n.Compose`](#i18ncompose)).

`app.Translator()` sirve también para textos propios del servidor (correos,
PDF): pasa un `context` con `i18n.WithLanguage`.

## `i18n.Catalog` e interpolación

```go
cat := i18n.NewCatalog(map[string]map[string]string{
    "es": {"greeting": "Hola {name}"},
    "en": {"greeting": "Hi {name}"},
}).WithFallback("es", "en")

cat.Translate(ctx, "greeting", map[string]any{"name": "Ana"}) // "Hola Ana"
cat.Translate(ctx, "greeting", "name", "Ana")                  // pares nombre/valor
```

- `NewCatalog` copia el mapa; `Add(lang, msgs)` / `Merge(msgs)` agregan
  mensajes (seguro en concurrencia).
- `WithFallback(langs...)` reemplaza los idiomas de respaldo (default `"en"`);
  sin argumentos desactiva el respaldo.
- `Lookup(lang, key)` devuelve el texto crudo y si existe.
- Interpolación simple `{nombre}`; los placeholders sin argumento quedan como
  están. No hay reglas de plural.
- Una llave ausente devuelve la llave (contrato de `Translator`).

## `i18n.Compose`

```go
tr := i18n.Compose(appCatalog, hostBundle) // la primera capa que acierta gana
```

Una capa "falla" cuando devuelve la misma llave o `""`. Las capas `nil` se
ignoran. Útil para pasar como `AppConfig.Translator` un bundle compuesto.

## Catálogos desde archivos: `LoadCatalogFS`

```go
//go:embed locales/*.json locales/*.yaml
var locales embed.FS

cat, err := i18n.LoadCatalogFS(locales, "locales/*.json", "locales/*.yaml")
if err != nil { log.Fatal(err) }
app.AddTranslations(cat.Messages())      // o: AppConfig{Translator: i18n.Compose(cat, bundle)}
```

- Formatos `.json`, `.yaml` y `.yml`.
- El idioma sale del nombre: `es-MX.json` → `es-MX`,
  `branches.es.yaml` → `es` (el último segmento antes de la extensión).
- Un archivo cuyo nombre no es un idioma (`messages.json`) se lee como
  documento multi-idioma: `{"es": {...}, "en": {...}}`.
- Los objetos anidados se aplanan con puntos
  (`{"models": {"branches": {"table": {"title": "…"}}}}` =
  `"models.branches.table.title"`); los arreglos dan `.0`, `.1`, …
- Error si ningún archivo coincide (típicamente un path de `embed` mal
  escrito) o si alguno no parsea.

## Etiquetas humanizadas: `I18nHumanizeMissing`

```go
host.NewApp(host.AppConfig{DB: db, JWTSecret: s, I18nHumanizeMissing: true, I18nDefaultLanguage: "es"})
```

Con la bandera, una llave que ninguna capa traduce se convierte en una
etiqueta legible (`i18n.Humanize`) en lugar de mostrarse cruda:

| Llave | `es` | otro idioma |
|---|---|---|
| `models.branches.modal.fields.postal_code` | Código postal | Postal code |
| `models.orders.table.columns.created_at` | Fecha de creación | Created at |
| `models.orders.table.columns.customer_id` | Cliente | Customer |
| `models.pickup_points.table.title` | Pickup points | Pickup points |

Reglas: el sujeto es el segmento que sigue al último contenedor (`fields`,
`columns`, `filters`, `actions`, `options`, `placeholders`, …); para llaves del
modelo (`table.title`, `modal.create_title`) es el nombre del modelo. Se separa
por `_`, `-` y camelCase, se descarta un `_id` final y un `is_`/`has_`
inicial. En español se usa un diccionario de nombres de campo comunes; lo que
no está ahí, y cualquier otro idioma, queda en inglés en *sentence case*.
Acrónimos conocidos (ID, URL, RFC, SKU, CLABE…) van en mayúsculas.

La bandera activa los transformers de metadata aunque no haya `Translator` ni
catálogos. `i18n.HumanizeMissing(tr)` ofrece lo mismo para un traductor
cualquiera.

## Compatibilidad

- Sin `Translator`, sin catálogos y sin `I18nHumanizeMissing` la metadata se
  sirve igual que antes (los transformers no se instalan).
- `AppConfig.Translator` sigue funcionando igual y gana sobre los catálogos.
  Única diferencia: si devuelve `""` para una llave, ahora se prueba la
  siguiente capa y, al final, se sirve la llave.
- El middleware `Accept-Language` ahora se monta siempre y guarda el idioma
  tanto en el `fiber.Ctx` como en `c.Context()`. Antes sólo quedaba en
  `c.Context()`, y los handlers de metadata (que pasan `c` al servicio) no lo
  veían: la metadata se traducía siempre con el idioma vacío.
- Apps que registraban sus propios transformers con un `TranslatorFunc`
  siguen funcionando; pueden migrar a `WithTranslations` y borrar ese código.
