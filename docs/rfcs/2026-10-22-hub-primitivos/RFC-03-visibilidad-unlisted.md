# RFC-03: visibilidad `unlisted` y por versión (en el hub, no en el manifest)

- **Prioridad:** P2 (M2, `hub_catalog`). Independiente del resto: puede ir en cualquier momento.
- **Tamaño:** S (solo hub).
- **Depende de:** nada. **Cero bumps de kernel.**

## 1. Problema (evidencia)

- El ADR §5.2 define tres niveles (`public`, `unlisted`, `private`); hoy solo hay dos:
  - Manifest: `metadata.visibility` acepta `public|private` (`manifest/v3/types.go:586-603`) [V].
  - Hub: constantes `VisibilityPublic/VisibilityPrivate` y `ValidVisibility`
    (`backend/internal/store/models.go:420-432`), `CHECK (visibility IN ('public','private'))`
    (`backend/internal/migrations/sql/0015_addon_visibility.sql:29`) [V].
- El spike propuso añadir `unlisted` al manifest (G7, «tres bumps») [S]. Pero el propio contrato del
  kernel ya separa las cosas: *«WHO is granted access to a private bundle is NOT part of the signed
  manifest (it changes without republishing); the marketplace owns that allowlist»*
  (`types.go:586-592`) [V]. Que un addon salga o no en el listado es exactamente igual de mutable: un
  admin lo cambia sin republicar, y debe poder hacerlo por versión (preview, beta) sin firmar otro bundle.
- El hub ya trata la aserción del manifest como **una sola vía** (puede volver `private` una fila, nunca
  abrirla; abrirla exige `POST /v1/admin/addons/{key}/visibility`) (`backend/internal/api/publish.go:149-216`) [V].

## 2. Diseño

### 2.1 Fila de catálogo (hub)

| Valor | Catálogo público / índice firmado | Ficha por URL | Instalar / bundle |
|---|---|---|---|
| `public` | sí | sí | instancia con licencia operable (gate) |
| `unlisted` (nuevo) | **no** | sí, con la clave | igual que `public` |
| `private` | no | solo orgs con *grant* | licencia operable **y** *entitlement* |

- `ValidVisibility` y la `CHECK` admiten `unlisted` (migración nueva `00NN_addon_visibility_unlisted.sql`:
  `DROP CONSTRAINT … ; ADD CONSTRAINT … CHECK (visibility IN ('public','unlisted','private'))`).
- `POST /v1/admin/addons/{key}/visibility` acepta `unlisted`. Transiciones permitidas por admin: todas.
- Publicación: el manifest sigue diciendo `public|private`. Regla nueva: un publish con
  `visibility: public` (o vacío) **no** cambia una fila `unlisted` (igual que no abre una `private`).
  Un `private` del manifest cierra también una fila `unlisted`.
- Listados (`api/catalog.go`, `api/addons_list.go`, `store/gorm_store.go`, `store/memory.go`): filtran
  `visibility = 'public'` para el catálogo público; la ficha `GET /v1/addons/{key}` responde para
  `public` y `unlisted`; `private` sigue con su chequeo de *grant* (`api/private_access.go:55`).
- Índice firmado del catálogo (ADR §5.8): **no** incluye `unlisted` ni `private`.

### 2.2 Por versión

Columna nueva `addon_versions.listed boolean NOT NULL DEFAULT true` (o `channel text` si el ADR de
rollouts lo prefiere; aquí basta el booleano):

- `listed = false`: la versión no aparece como «última» en catálogo ni en `/v1/addons/{key}` para
  instancias que no la pidan explícitamente por número; sí se descarga por versión exacta (canary de un
  addon, versión de prueba para un cliente). Complementa el *yank* (migración 0069), que la retira.
- Endpoint admin `POST /v1/admin/addons/{key}/versions/{v}/listing {listed}` y acción
  correspondiente en `hub_catalog` (RFC-02 `set_fields` + RFC-01 webhook al núcleo cuando exista).

## 3. Cambios por repo

| Repo | Cambio |
|---|---|
| hub | migración + constantes + filtros de listado + endpoint admin + pruebas (patrón de `api/addon_visibility_test.go`). |
| kernel | **ninguno.** Opcional y diferido: si algún día un autor necesita *declarar* `unlisted` en el bundle, se añade al enum `metadata.visibility` con la regla «solo puede cerrar, nunca abrir» y la cadena completa de bumps; hoy no hay caso. |
| ops | ninguno: el marketplace embebido consulta `/v1/addons`, que deja de devolver `unlisted` en el listado y sigue resolviendo la ficha por clave. |

## 4. Compatibilidad

- Instancias viejas que listan `/v1/addons`: dejan de ver los `unlisted` en el listado, que es el
  comportamiento buscado; la instalación por clave sigue funcionando.
- Manifests v3 estrictos: sin cambio de contrato, sin riesgo de 400 en publish.

## 5. Pruebas

`TestVisibility_Listing_AnonDoesNotSeeUnlisted`, `…_UnlistedDetailByKey`, `…_UnlistedInstallable`,
`…_PublishPublicDoesNotRelistUnlisted`, `…_PublishPrivateClosesUnlisted`, `…_IndexExcludesUnlisted`,
`…_UnlistedVersionNotLatest`, `…_ExactUnlistedVersionDownloadable`.

## 6. Seguridad

`unlisted` **no** es control de acceso (la clave del addon no es secreta). Lo que no debe verse se
declara `private`. La descarga sigue detrás del gate de licencia (`HUB_BUNDLE_GATE=enforce`, ADR §4.4).
