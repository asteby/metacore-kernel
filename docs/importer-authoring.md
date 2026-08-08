# Authoring spreadsheet imports

MetaCore already has a **dynamic** import path — there is no separate
`defineImporter` API. One `ImportSpec` drives both the downloadable template
and the parser.

## How to author

### Compiled Go model (host app)

```go
func (m *MyModel) DefineImport() modelbase.ImportSpec {
    return modelbase.ImportSpec{
        SheetName: "Datos",
        Columns: []modelbase.ImportColumn{
            {Key: "name", Header: "Nombre", Required: true},
            {Key: "email", Header: "Email", Type: "email", Required: true, Aliases: []string{"Correo"}},
            {Key: "password", Header: "Contraseña", Generator: "random_secret"},
            {
                Key:       "avatar",
                Header:    "URL Foto de Perfil",
                Transform: "media_url", // fetch → host MediaStore
                Example:   "https://cdn.example.com/photo.jpg",
            },
        },
        Instructions: []string{"Una fila por registro."},
    }
}
```

Implement `modelbase.HasImportSpec` via `DefineImport()`.

### Addon manifest (v3)

```json
{
  "import": {
    "sheet_name": "Datos",
    "columns": [
      { "key": "name", "header": "Nombre", "required": true },
      { "key": "avatar", "header": "URL Foto", "transform": "media_url" }
    ]
  }
}
```

Omitting `import` derives a usable spec from the model's form fields.

## Pipeline

1. Host serves `GET /dynamic/:model/export/template` → `importer.BuildTemplate(spec)`.
2. UI (`ImportDialog` in `@asteby/metacore-runtime-react`) uploads the file.
3. Host calls `importer.PrepareWithDeps(spec, rows, deps)` then creates each record.

## Generators vs transforms

| | When | Examples |
|---|---|---|
| **Generator** | Cell empty | `random_secret` |
| **Transform** | Cell present (after coerce) | `media_url`, `media_url_list` |

Register custom ones at host bootstrap:

```go
importer.RegisterGenerator("my_gen", func() (any, error) { ... })
importer.RegisterTransform("my_xf", func(ctx importer.TransformContext, raw string) (any, error) { ... })
```

## Media transforms

Builtins `media_url` / `media_url_list` need request-scoped deps:

```go
deps := &importer.TransformDeps{
    Store: myMediaStore, // Put(ctx, name, reader, contentType) → storedName
    // HTTPClient optional (default 10s timeout)
}
prepared, err := importer.PrepareWithDeps(spec, rows, deps)
```

- Timeout: 10s per URL  
- Max size: 5 MiB  
- `media_url_list` splits on `|`, `,`, `;` and joins stored filenames with `|`

Without `Store`, those transforms fail the row with a clear message (validate
and import stay in sync).

## Host hooks (optional)

Hosts may normalize composed columns (prefix + first + last name → `user.name`)
or attach M2M relations after create. Keep that in the host — the kernel stays
model-agnostic.
