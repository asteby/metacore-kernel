# Document forms and option filters

Two opt-in UI contracts that the `@asteby/metacore-runtime-react` SDK (47.x)
reads from the metadata the kernel serves. Both are pure UI metadata: the
DDL and write planes ignore them, and a manifest that declares neither serves
exactly the payload it served before (retro-compatible).

| Contract | Declared in the manifest | Served in |
| --- | --- | --- |
| Guided create flow per document type | `models[].document_forms` | `TableMetadata.document_forms` |
| Hide options of a relation picker | `option_filter` on a column or an action field (also inside `document_forms` fields) | `ColumnDef.option_filter`, `FieldDef.option_filter` |
| Extra columns on every option | `options.extra_columns` (object form of `options`) | `FieldOptionsConfig.extra_columns` + extra keys in `GET /options/:model` |

## `document_forms`

Replaces the generic "Crear" modal of a document-like model (invoice, credit
note, payment receipt...) with one card per document type; picking a card shows
that type's own fields and, when the type declares `lines`, a line-items step
(`DocumentLinesGrid`). One type skips the card selector. The SDK writes the
type's `value` into `type_field` and the serialized lines into the lines field.

Manifest (`models[]`, v3) and served metadata share the same shape (the kernel
mirrors the SDK's `DocumentFormsManifest`):

```json
{
  "document_forms": {
    "type_field": "type",
    "lines_field": "lines",
    "types": [
      {
        "key": "invoice",
        "label": "Factura",
        "description": "Ingreso (I)",
        "icon": "receipt",
        "value": "I",
        "fields": [
          { "key": "customer_name", "label": "Cliente", "type": "text", "required": true },
          { "key": "payment_method", "label": "Método de pago", "type": "text" }
        ],
        "defaults": { "currency": "MXN" },
        "lines": { "columns": ["tax", "discount"], "price_source": "sale", "required": true, "title": "Partidas" },
        "endpoint": "/dynamic/fiscal_documents",
        "submit_label": "Emitir"
      },
      {
        "key": "payment_receipt",
        "label": "REP",
        "value": "P",
        "fields": [
          {
            "key": "invoice_id", "label": "Factura pagada", "type": "dynamic_select", "required": true,
            "optionsConfig": { "type": "dynamic", "source": "Invoice", "value": "id", "label": "folio", "extra_columns": ["status"] },
            "option_filter": [ { "field": "status", "not_in": ["cancelada"] } ]
          }
        ],
        "lines": true
      }
    ]
  }
}
```

(The manifest spells the picker source `"options": { "source": ... }`, the
served field spells it `"optionsConfig": { "type": "dynamic", ... }`, exactly
like action fields.)

| Key | Notes |
| --- | --- |
| `type_field` | Model column that discriminates the type; must exist in the model. Empty = the type is not written. |
| `lines_field` | Default payload field for the lines (default `lines`). |
| `types[].key` | `^[a-z][a-z0-9_]*$`, unique. |
| `types[].label` / `description` / `submit_label` | Literal or i18n key (the localized table transformer translates them). |
| `types[].value` | Written into `type_field`; default = `key`. Must be unique across types when `type_field` is set. |
| `types[].fields[]` | Same vocabulary as action fields (`Action.fields`): pickers, `visible_when`, `depends_on`, `validation`, `item_fields`... Keys unique within the type. |
| `types[].defaults` | Fixed values that travel with the type's create payload. |
| `types[].lines` | `true` (all defaults), `false`/absent (no lines step) or `{ field, columns[], price_source: "sale"\|"cost", required, title }`. Served as `true` when option-less, an object otherwise; `false` is not served. The lines payload field must not collide with a header field key. |
| `types[].endpoint` | Create endpoint override for the type. |

Validation (`v3.Validate`): at least one type, key/label rules above,
`type_field` is a model column, `price_source` in `sale|cost`, no duplicate
line columns, plus the `option_filter` / `extra_columns` rules for the fields.
The JSON schema (`manifest/v3/schema/manifest-v3.schema.json`,
`docs/spec/v3/manifest-v3.schema.json`) defines `DocumentForms`,
`DocumentFormType` and `DocumentFormLines`.

### How the kernel serves it

`GET /metadata/table/:model` returns `data.document_forms` when present.
Sources, in order:

1. `TableMetadata.DocumentForms` set by the model's own `DefineTable`. Hosts
   serving addon models call `dynamic.DeriveDocumentForms(def)` (same pattern as
   `DeriveFormLayout`) from their definer; `manifest.FromV3` already carries the
   block onto `ModelDefinition.DocumentForms`.
2. Models implementing `modelbase.HasDocumentForms`
   (`DefineDocumentForms() *DocumentForms`) are projected by
   `metadata.Service`. `DefineTable` wins when both are set.

`metadata.NewLocalizedTableTransformer` translates the i18n keys of type labels,
descriptions, submit labels, line step titles and field labels/options.

## `option_filter`

Hides options of a relation / dynamic picker (for example cancelled invoices in
a payments selector). It is evaluated **by the SDK on the client**, over the
options `GET /options/:model` already returned, so the picker's options must
carry the tested columns (see `extra_columns`). Available on:

- `models[].columns[].option_filter` (a column with `ref` or an object `options`),
- `contributions.actions[].fields[].option_filter` (and its `item_fields`),
- `document_forms.types[].fields[].option_filter`.

One rule or a list (AND). Always served as a list:

```json
"option_filter": [
  { "field": "status", "not_in": ["cancelada", "borrador"] },
  { "field": "folio", "not_equals": "X" }
]
```

| Rule key | Meaning |
| --- | --- |
| `field` | Option property tested: an extra column (`status`) or `id`/`value`/`label`/`name`/`description`/`color`/`icon`. |
| `equals`, `in` | Keep only options whose value matches (the property must exist). |
| `not_equals`, `not_in` | Hide options whose value matches (an option lacking the property is kept). |

Values are strings, numbers or booleans. Comparison is trimmed and
case-insensitive; the current selection is never hidden by the SDK. A rule
needs `field` and at least one operator (`in`/`not_in` non-empty), otherwise
the manifest is rejected.

## `extra_columns` on `/options`

The object form of `options` gains `extra_columns`: scalar columns of `source`
that `GET /options/:model?field=<f>` returns on **every** option, as sibling
keys of `id`/`value`/`label`/`name` (this is where the SDK reads them, into
`option.meta`):

```json
"options": { "source": "Invoice", "value": "id", "label": "folio", "extra_columns": ["status"] }
```

```json
{ "id": "…", "value": "…", "label": "FAC-1", "name": "FAC-1", "status": "vigente" }
```

- Served on the field as `optionsConfig.extra_columns`
  (`modelbase.FieldOptionsConfig.ExtraColumns`); the host's
  `OptionsConfigResolver` must keep it when it builds the entry
  (`Service.Options` reads it).
- Only plain identifiers are honoured; names colliding with `id`, `value`,
  `label`, `name`, `description`, `image`, `color`, `icon` are ignored (the
  manifest validator rejects them); a column not present on the source row, a
  NULL value or a non-scalar value is omitted. Named string/number/bool types
  are unwrapped; types with a `String()` method (uuid, time) are served as
  strings.
- Manifest validation: lowercase snake_case, no duplicates, and each column
  must exist in `source` when `source` is a model of the same manifest (the
  kernel audit columns and `organization_id` are always accepted).
- Without `extra_columns` the `/options` payload is unchanged. In Go,
  `dynamic.Option.Extra` carries the values and `Option.MarshalJSON` flattens
  them; declared keys can never be overwritten.
