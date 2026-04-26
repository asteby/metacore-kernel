# Dynamic CRUD API Reference

HTTP reference for every endpoint the dynamic CRUD framework mounts. The
shapes documented here are the wire contract — JSON tags on the underlying
Go types are load-bearing and any change is a MAJOR version bump.

For the conceptual walkthrough see [`dynamic-system.md`](dynamic-system.md).

---

## Table of contents

- [Conventions](#conventions)
- [Authentication](#authentication)
- [Error envelope](#error-envelope)
- [CRUD endpoints](#crud-endpoints)
  - [List](#list)
  - [Get](#get)
  - [Create](#create)
  - [Update](#update)
  - [Delete](#delete)
- [Lookup endpoints](#lookup-endpoints)
  - [Options](#options)
  - [Search](#search)
- [Metadata endpoints](#metadata-endpoints)
  - [Table metadata](#table-metadata)
  - [Modal metadata](#modal-metadata)
  - [All metadata](#all-metadata)
- [Filter operators](#filter-operators)
- [Status code reference](#status-code-reference)

---

## Conventions

- Base path is whatever the host passes to `app.Mount()`. Examples below
  use `/api`.
- IDs are UUIDs (RFC 4122 v4). Non-UUID `:id` segments return `400`.
- `:model` is the registry key passed to
  [`App.RegisterModel(key, factory)`](../host/app.go) — usually the
  snake-case table name.
- Every successful response is `{ "success": true, "data": ... }`.
  List responses additionally carry `meta` (pagination).
- Every error response is `{ "success": false, "message": "<reason>" }`.
- Timestamps are ISO 8601 in UTC. `null` is allowed where the underlying
  column is nullable.
- Response shapes are produced by `dynamic.Handler`
  ([`dynamic/handler.go`](../dynamic/handler.go)) and
  `metadata.Handler` ([`metadata/handler.go`](../metadata/handler.go)).

## Authentication

Every CRUD and metadata endpoint sits behind the auth middleware mounted by
`host.App`. Send the JWT in the standard header:

```
Authorization: Bearer <jwt>
```

When the resolver returns no user, requests are rejected with `401`:

```json
{ "success": false, "message": "not authenticated" }
```

## Error envelope

| Status | When                                          | Sample message                                         |
| ------ | --------------------------------------------- | ------------------------------------------------------ |
| 400    | Bad UUID, malformed body, validation failure  | `"invalid id"`, `"invalid body"`, `"invalid input: ..."` |
| 401    | No or invalid JWT                             | `"not authenticated"`                                  |
| 403    | Permission denied                             | `"permission denied: missing capability \"tickets.create\""` |
| 404    | Model unregistered, or row missing            | `"model not found in registry"`, `"record not found"`  |
| 422    | Metadata request with invalid metadata        | `"metadata invalid: ..."`                              |
| 501    | Options/Search called without resolver wired  | `"options config not available"`                       |
| 500    | Anything else (DB error, etc.)                | `"dynamic: list: ..."`                                 |

Error mapping lives in
[`dynamic/handler.go:handleError`](../dynamic/handler.go) and
[`metadata/handler.go:respondServiceError`](../metadata/handler.go).

## CRUD endpoints

All five routes are mounted by `dynamic.Handler.Mount`
([`dynamic/handler.go`](../dynamic/handler.go)):

```
GET    /dynamic/:model
POST   /dynamic/:model
GET    /dynamic/:model/:id
PUT    /dynamic/:model/:id
DELETE /dynamic/:model/:id
```

### List

Paginated, filtered, sorted, free-text-searched.

`GET /api/dynamic/:model`

Query string parameters (parsed by
[`query/params.go`](../query/params.go)):

| Param        | Type    | Default | Notes                                                          |
| ------------ | ------- | ------- | -------------------------------------------------------------- |
| `page`       | int ≥1  | `1`     | 1-indexed                                                      |
| `per_page`   | int     | `20`    | Clamped to `[1, MaxPerPage]` (see `query.MaxPerPage`)          |
| `sortBy`     | string  | unset   | Must match a `TableMetadata.Columns[].key` — unknown is dropped |
| `order`      | enum    | `desc`  | `asc` or `desc`                                                |
| `search`     | string  | unset   | Free-text; truncated at `MaxSearchTermLength`                  |
| `f_<col>`    | string  | unset   | Filter — see [Filter operators](#filter-operators)             |

```bash
curl -G \
  -H "Authorization: Bearer $JWT" \
  --data-urlencode "page=2" \
  --data-urlencode "per_page=25" \
  --data-urlencode "sortBy=due_at" \
  --data-urlencode "order=asc" \
  --data-urlencode "search=invoice" \
  --data-urlencode "f_status=in:open,pending" \
  https://api.example.com/api/dynamic/tickets
```

Response `200 OK`:

```json
{
  "success": true,
  "data": [
    {
      "id": "9b1c08f1-3c4a-4f9c-bd4e-9d0b3e5a1234",
      "organization_id": "11111111-1111-1111-1111-111111111111",
      "subject": "Invoice #2042 missing PDF",
      "status": "open",
      "priority": "high",
      "body": "Customer reports the PDF link 404s.",
      "due_at": "2026-04-30T17:00:00Z",
      "created_at": "2026-04-26T12:01:09Z",
      "updated_at": "2026-04-26T12:01:09Z"
    }
  ],
  "meta": {
    "total": 137,
    "page": 2,
    "per_page": 25,
    "last_page": 6
  }
}
```

`meta` shape is `query.PageMeta`
([`query/builder.go`](../query/builder.go)) — its JSON tags are part of
the public API.

### Get

`GET /api/dynamic/:model/:id`

```bash
curl -H "Authorization: Bearer $JWT" \
  https://api.example.com/api/dynamic/tickets/9b1c08f1-3c4a-4f9c-bd4e-9d0b3e5a1234
```

Response `200 OK`:

```json
{
  "success": true,
  "data": {
    "id": "9b1c08f1-3c4a-4f9c-bd4e-9d0b3e5a1234",
    "subject": "Invoice #2042 missing PDF",
    "status": "open",
    "priority": "high",
    "body": "Customer reports the PDF link 404s.",
    "due_at": "2026-04-30T17:00:00Z",
    "created_at": "2026-04-26T12:01:09Z",
    "updated_at": "2026-04-26T12:01:09Z"
  }
}
```

`404 Not Found` when the row does not exist or is filtered out by the
tenant scope (the org doesn't own this id).

### Create

`POST /api/dynamic/:model`

Body is a JSON object keyed by column name. The kernel:

1. Injects `organization_id` from the JWT (when the model is `org_scoped`).
2. Sets `created_by_id` to the authenticated user.
3. Runs `BeforeCreate` hooks.
4. Inserts via GORM (`id`, `created_at`, `updated_at` are set by Postgres).
5. Runs `AfterCreate` hooks.

```bash
curl -X POST \
  -H "Authorization: Bearer $JWT" \
  -H "Content-Type: application/json" \
  -d '{
    "subject":  "Invoice #2042 missing PDF",
    "status":   "open",
    "priority": "high",
    "body":     "Customer reports the PDF link 404s."
  }' \
  https://api.example.com/api/dynamic/tickets
```

Response `201 Created`:

```json
{
  "success": true,
  "data": {
    "id": "9b1c08f1-3c4a-4f9c-bd4e-9d0b3e5a1234",
    "organization_id": "11111111-1111-1111-1111-111111111111",
    "subject": "Invoice #2042 missing PDF",
    "status": "open",
    "priority": "high",
    "body": "Customer reports the PDF link 404s.",
    "due_at": null,
    "created_at": "2026-04-26T12:01:09Z",
    "updated_at": "2026-04-26T12:01:09Z"
  }
}
```

Validation: column types are coerced via JSON unmarshalling onto the
runtime struct produced by `dynamic.BuildStructType`
([`dynamic/model.go`](../dynamic/model.go)). Type mismatches surface as
`400 invalid input`. Cross-field validation is the addon's job — register
a hook on `dynamic.Hooks` ([`dynamic/hooks.go`](../dynamic/hooks.go)).

### Update

`PUT /api/dynamic/:model/:id`

Behaviour is **load-merge-save**, not a partial PATCH:

1. The kernel loads the row by id (org-scoped).
2. JSON-unmarshals the request body onto the loaded struct — keys not in
   the body keep their previous value.
3. Runs `BeforeUpdate` hooks.
4. `gorm.Save` writes the full row back (so omitted columns retain their
   existing value, not get nulled out).
5. Runs `AfterUpdate` hooks.

```bash
curl -X PUT \
  -H "Authorization: Bearer $JWT" \
  -H "Content-Type: application/json" \
  -d '{ "status": "resolved" }' \
  https://api.example.com/api/dynamic/tickets/9b1c08f1-3c4a-4f9c-bd4e-9d0b3e5a1234
```

Response `200 OK`:

```json
{
  "success": true,
  "data": {
    "id": "9b1c08f1-3c4a-4f9c-bd4e-9d0b3e5a1234",
    "subject": "Invoice #2042 missing PDF",
    "status": "resolved",
    "priority": "high",
    "body": "Customer reports the PDF link 404s.",
    "due_at": null,
    "created_at": "2026-04-26T12:01:09Z",
    "updated_at": "2026-04-26T13:42:55Z"
  }
}
```

`404 Not Found` when the id is unknown to this org.

### Delete

`DELETE /api/dynamic/:model/:id`

```bash
curl -X DELETE \
  -H "Authorization: Bearer $JWT" \
  https://api.example.com/api/dynamic/tickets/9b1c08f1-3c4a-4f9c-bd4e-9d0b3e5a1234
```

Response `200 OK`:

```json
{ "success": true }
```

When `soft_delete: true` is set on the model definition, GORM updates
`deleted_at` and the row vanishes from subsequent list/get calls without
losing data. Otherwise the delete is unconditional (`DELETE FROM ...`).

## Lookup endpoints

The options and search endpoints are mounted by
`dynamic.Handler.MountOptions` — outside the `/dynamic` prefix, to preserve
the historical paths consumers rely on.

```
GET /options/:model
GET /search/:model
```

`host.App.Mount` does **not** call `MountOptions` automatically — hosts
that need lookup endpoints construct a `dynamic.Handler` directly and
attach it themselves:

```go
dynHandler := dynamic.NewHandler(app.Dynamic, userResolver)
dynHandler.MountOptions(api, authMiddleware)
```

Both require the host to wire a resolver in `dynamic.Config`
(`OptionsConfigResolver`, `SearchConfigResolver`) — without one, the
endpoints return `501 Not Implemented`. See
[`dynamic/service.go`](../dynamic/service.go).

### Options

Render values for a `<select>` field. Used heavily by the runtime-react
form generator.

`GET /api/options/:model?field=<col>[&q=...&filter_value=...&limit=...&offset=...]`

| Param          | Notes                                                       |
| -------------- | ----------------------------------------------------------- |
| `field`        | Required. The form field whose options are being requested. |
| `q`            | Optional label-column filter (LIKE / ILIKE).                |
| `filter_value` | Optional, scoped through `FieldOptionsConfig.FilterBy`.     |
| `limit`        | Default 50, clamped to `MaxOptionsLimit` (200).             |
| `offset`       | Default 0.                                                  |

```bash
curl -G \
  -H "Authorization: Bearer $JWT" \
  --data-urlencode "field=assignee_id" \
  --data-urlencode "q=alice" \
  https://api.example.com/api/options/tickets
```

Response `200 OK`:

```json
{
  "success": true,
  "type": "dynamic",
  "data": [
    { "id": "...", "value": "...", "label": "Alice Hopper", "name": "Alice Hopper" }
  ]
}
```

`type` is `"static"` when the field declares a hardcoded list and
`"dynamic"` when it queries a related model. Static options never hit
the database.

### Search

Free-text search over the columns listed in `SearchConfig.SearchIn`. The
resolver also drives nested-relation joins (dotted paths like
`patient.user.name`).

`GET /api/search/:model?q=<text>[&limit=...]`

```bash
curl -G \
  -H "Authorization: Bearer $JWT" \
  --data-urlencode "q=invoice 2042" \
  https://api.example.com/api/search/tickets
```

Response `200 OK`:

```json
{
  "success": true,
  "data": [
    { "id": "...", "value": "...", "label": "Invoice #2042 missing PDF", "name": "Invoice #2042 missing PDF" }
  ]
}
```

The dialect-specific match clause is configurable: pass
`Config.SearchMatchClause` to use unaccent/ILIKE on Postgres. Default is
portable `<col> LIKE ?` with `%q%`.

## Metadata endpoints

Mounted by `metadata.Handler.Mount` — see
[`metadata/handler.go`](../metadata/handler.go). The host wires them under
`/metadata`:

```
GET /metadata/table/:model
GET /metadata/modal/:model
GET /metadata/all
```

Cached for `MetadataCacheTTL` (default 5 min). Hosts call
`metaSvc.InvalidateModel(key)` after a per-org transformer changes.

### Table metadata

`GET /api/metadata/table/:model`

```bash
curl -H "Authorization: Bearer $JWT" \
  https://api.example.com/api/metadata/table/tickets
```

Response `200 OK`:

```json
{
  "success": true,
  "data": {
    "title": "Tickets",
    "columns": [
      {
        "key": "subject",
        "label": "Subject",
        "type": "text",
        "sortable": true,
        "filterable": false
      },
      {
        "key": "status",
        "label": "Status",
        "type": "badge",
        "filterable": true,
        "options": [
          { "value": "open", "label": "Open", "color": "blue" }
        ]
      }
    ],
    "searchColumns": ["subject"],
    "filters": [
      { "key": "status", "label": "Status", "type": "select", "column": "status" }
    ],
    "actions": [
      { "key": "escalate", "name": "escalate", "label": "Escalate", "icon": "AlertTriangle" }
    ],
    "enableCRUDActions": true,
    "perPageOptions": [10, 25, 50],
    "defaultPerPage": 25,
    "searchPlaceholder": "Search tickets..."
  }
}
```

The Go shape is `modelbase.TableMetadata`
([`modelbase/metadata.go`](../modelbase/metadata.go)). Every JSON tag is
part of the wire contract.

### Modal metadata

`GET /api/metadata/modal/:model`

```bash
curl -H "Authorization: Bearer $JWT" \
  https://api.example.com/api/metadata/modal/tickets
```

Response `200 OK`:

```json
{
  "success": true,
  "data": {
    "title": "Ticket",
    "createTitle": "Create ticket",
    "editTitle":   "Edit ticket",
    "deleteTitle": "Delete ticket",
    "fields": [
      {
        "key": "subject",
        "label": "Subject",
        "type": "text",
        "required": true,
        "validation": "min:3|max:200",
        "placeholder": "Briefly describe the issue"
      },
      {
        "key": "status",
        "label": "Status",
        "type": "select",
        "required": true,
        "defaultValue": "open",
        "options": [
          { "value": "open",     "label": "Open" },
          { "value": "pending",  "label": "Pending" },
          { "value": "resolved", "label": "Resolved" }
        ]
      },
      {
        "key": "due_at",
        "label": "Due",
        "type": "date"
      }
    ],
    "messages": {
      "createSuccess": "Ticket created",
      "updateSuccess": "Ticket updated",
      "deleteConfirm": "Delete this ticket?"
    }
  }
}
```

The Go shape is `modelbase.ModalMetadata`. `FieldDef.Type` values consumed
by the runtime-react form generator: `text`, `textarea`, `select`,
`search`, `number`, `date`, `email`, `url`, `boolean`, `image`.

### All metadata

`GET /api/metadata/all`

Returns every registered model's table+modal metadata in one payload —
frontends call it once at startup to warm a local cache.

```bash
curl -H "Authorization: Bearer $JWT" \
  https://api.example.com/api/metadata/all
```

Response `200 OK`:

```json
{
  "success": true,
  "data": {
    "version": "lqp9i00.0",
    "tables": {
      "tickets":  { "title": "Tickets",  "columns": [/* … */] },
      "products": { "title": "Products", "columns": [/* … */] }
    },
    "modals": {
      "tickets":  { "title": "Ticket",  "fields": [/* … */] },
      "products": { "title": "Product", "fields": [/* … */] }
    }
  }
}
```

`version` is a monotonic token that changes on every cache invalidation —
use it as an ETag.

## Filter operators

The list endpoint accepts filters as `f_<col>=<op>:<value>`. The parser is
in [`query/filter.go`](../query/filter.go). When `<op>:` is omitted, the
value is treated as `eq`.

| Op       | Wire form                       | Meaning                                          |
| -------- | ------------------------------- | ------------------------------------------------ |
| `eq`     | `f_status=eq:open`              | Equality                                         |
| `ilike`  | `f_subject=ilike:invoice%25`    | Case-insensitive LIKE (Postgres)                 |
| `in`     | `f_status=in:open,pending`      | IN list                                          |
| `gte`    | `f_due_at=gte:2026-04-01`       | `>=`                                             |
| `lte`    | `f_due_at=lte:2026-04-30`       | `<=`                                             |
| `range`  | `f_due_at=range:2026-04-01\|2026-04-30` | `BETWEEN min AND max` (either side may be empty) |

Filters target columns in `TableMetadata.Columns[].key`. Keys not present
in metadata are silently dropped — the query is allow-listed before
hitting GORM.

## Status code reference

| Code | When                                                              |
| ---- | ----------------------------------------------------------------- |
| 200  | List, Get, Update, Delete, Options, Search, all metadata          |
| 201  | Create                                                            |
| 400  | Bad UUID; malformed JSON body; field required                     |
| 401  | UserResolver returned nil                                         |
| 403  | Permission denied                                                 |
| 404  | Model not registered; record not found; options field missing     |
| 422  | Metadata invalid (transformer error)                              |
| 501  | Options or Search called without resolver wired                   |
| 500  | DB / unexpected error                                             |

The mapping is defined in
[`dynamic/handler.go:handleError`](../dynamic/handler.go) and
[`metadata/handler.go:respondServiceError`](../metadata/handler.go).

---

See also: [`dynamic-system.md`](dynamic-system.md),
[`permissions.md`](permissions.md),
[`embedding-quickstart.md`](embedding-quickstart.md).
