# ADR 0006 — «Dueño + enchufes» en el kernel: contratos por dependencia y anchors de navegación

- Estado: aceptado (implementación aditiva, F1 del RFC)
- Fecha: 2026-11-25
- Fuente: RFC SDK-0001 (`metacore-sdk/docs/rfcs/0001-dueno-y-enchufes-contratos-y-anchors.md`, decisiones A1–A6 aprobadas con la recomendación del RFC) y RFC 0004 del kernel (#653, parte A ya en v0.321).

## Qué entra

| Pieza | Dónde | Nota |
|---|---|---|
| `compatibility.requires[].contract` (excluyente con `key`) | `manifest/v3` (tipo, las dos copias del schema con `oneOf`), `manifest.Manifest.ContractRequires`, `v3.Manifest.ContractRequirements()` | `Requirement.key` pasa a `omitempty` para que un `{contract}` serializado no lleve `"key": ""`. |
| `extension_points.nav_anchors[]` (`NavAnchor`) | `manifest/v3`, `manifest.Manifest.NavAnchors` | |
| `NavGroup.{key, section, order}`, `NavItem.{key, attach_to, as, fallback, order}` | `manifest/v3`, `manifest.NavGroup/NavItem` | |
| Registro cerrado de secciones, tabla `target`→`section`, defaults de `fallback` (A4) | `manifest/v3/nav_anchors.go` | Vive en `v3` y no en `navigation` porque el validador de `v3` lo necesita y `navigation` ya importa `manifest`. |
| Validación por manifest | `validateNavAnchors` en `v3.validateDoc` | Ver «Reglas». |
| Resolvedor puro v2 y puerta de instalación | `navigation.Resolve`, `navigation.CheckInstalled` | |
| Registro de contratos alimentado por manifests | ya existía (`capability.Registry`, `Installer.ContractRegistry`); nuevo `Registry.Selectable` / `Owner` y `Declared.Cardinality` | `Selectable` entrega un `capability.Contract` de un contrato DECLARADO para usar el `Select` de siempre (binding de mercado > default de la org > único proveedor; `ambiguous_provider`). |
| Instalación en cascada por contrato (A6) | `installer.PlanContractInstall` + `ContractCatalog` (lo implementa el host sobre el hub) | Instala al dueño y lo que el dueño necesita; nunca un proveedor; `ChooseProvider` lista lo que la org debe elegir. |
| Proyecciones N-1 | `Manifest.ProvidesCapabilitiesForN1`, `manifest.InferImplements` | No corren dentro de `FromV3`. |
| Evento `commerce.payments.refund_created` | `capability/commerce.go` | Pedido del coordinador; ver al final. |

## Reglas de validación (manifest/v3)

Errores (solo pueden darse con campos nuevos): clave/sección de anchor fuera del registro o que no coinciden, anchor duplicado en el manifest, anchor sin exactamente uno de `model|page|url`, `model` que no es del addon, `page` inexistente, `accepts` vacío/repetido/fuera del enum, `key` de grupo que no empieza con `<metadata.key>.`, `section` y `target` que no coinciden, `items[].key` repetida en el addon, `as`/`fallback` sin `attach_to`, `attach_to` sin `as`, `attach_to` en un item anidado, `fallback: section` con `action|panel|setting`, `as: action` sin `actions[]`, `as: mode` sin `page`, `as` no aceptado por un anchor **del mismo manifest**, `contract` mal formado o junto con `key`.

Permitido: `attach_to` a un anchor que nadie publica (aplica el fallback).

Avisos: `nav_target_unknown` (NAV-06) para un `target` legacy que no mapea a sección (va a `operations`); `contract_conformance_missing` para un contrato `>=1.0.0` sin `conformance`.

Entre manifests (instalación): `navigation.CheckInstalled` da error para dos dueños definitivos del mismo anchor y para un `as` que el anchor presente no acepta; aviso para provisional reemplazado por definitivo. El instalador lo corre en `checkRequires` con el mismo modo `EnforceAddonRequires` (aviso por omisión), solo si el candidato publica anchors o usa `attach_to`, y solo reporta los problemas que **agrega** el candidato.

## Decisiones donde el RFC no cerraba (opción conservadora)

1. **`order` es `*int`**: `nil` = 1000; así `order: 0` es explícito y no se confunde con «sin orden».
2. **`target`**: además de `sidebar.<x>` se acepta `<x>` sin prefijo y sin distinguir mayúsculas (como el resolvedor de fase 0 de ops); `sidebar.settings` → `settings`; vacío → `operations` sin aviso.
3. **Recurso para el dedupe**: `page` > `model` + `view_type` (`table` = vacío) + `filter` canónico > `url` normalizada (sin barra final, query ordenada, sin fragmento). `group_by` y `columns` no cuentan (el RFC no los nombra). Los padres con hijos no participan.
4. **Dedupe y legacy**: el alcance es el del RFC (anchor + `as`, o el mismo grupo); grupos legacy de addons distintos **no** se deduplican entre sí, para que la navegación de manifests sin campos nuevos no cambie. Extensión: una hoja propia de grupo cuyo recurso es el recurso de un anchor visible **de la misma sección** se descarta a favor del anchor (`products.list` frente a `catalog.product`); es la lectura de «dueño del anchor gana» y cubre los casos de fase 0 de ops.
5. **Anchor sin permiso** = anchor ausente: sus enchufes aplican su fallback (§5.3 lo dice para el dueño ausente; se extiende al permiso).
6. **First-party** lo decide el host (`navigation.Addon.FirstParty`); el kernel no sabe quién publica.
7. **Resolve nunca falla**: dos dueños definitivos → gana first-party y luego la clave menor, con aviso; el error es de `CheckInstalled`.
8. **v1 (`Build`/`BuildFor`) no cambia para manifests legacy** (golden generado antes del cambio, `navigation/testdata/legacy_buildfor.golden.json`). Con un manifest con anchors, v1 trata cada enchufe como si su anchor faltara: descarta los de fallback `hide` y conserva como entrada los de `section`; un grupo hecho solo de enchufes ocultos no se pinta.
9. **`NavItem.page`** no se proyectaba en el manifest host; ahora se proyecta **solo** en items con `attach_to`, para que la navegación servida de un manifest legacy sea idéntica byte a byte.
10. **Conformidad**: el kernel solo avisa (`contract_conformance_missing`, en ambos modos) y expone `CheckContractConformance` para que el hub lo convierta en error al publicar. «`implements` publica solo con CONF verde» es del hub.
11. **Proyección `implements`→`provides_capabilities`** solo al servir a un host N-1 (`ProvidesCapabilitiesForN1`): convención `{key: "<contrato>.<op>", trigger: {type: "wasm", export: "<op>"}}`, gana la entrada propia, operaciones de `implements[].operations` o, si está vacío, las que dé el host. No se aplica en `FromV3`: un host nuevo enrutaría a exports que el addon quizá no tiene.
12. **Desinstalar un dueño** (§6.4): lo bloquean (en `enforce`) los consumidores no opcionales por `requires[].contract`; los `implements` siguen solo con aviso (`warnDanglingContracts`), como hoy, para no bloquear desinstalaciones que hoy pasan.
13. **Proveedor sin dueño**: `CheckInstallable` no lo bloquea (conducta actual); `PlanContractInstall` sí trae al dueño.
14. **Plan en cascada**: sigue `requires[].contract` no opcionales, `implements[]` y los `requires` de addon no opcionales de los dueños planeados; los opcionales nunca se instalan (sus `enables[]` quedan apagados); profundidad > 3 y ciclos son error. `ChooseProvider` solo lista contratos `cardinality: one` sin implementador instalado ni planeado.
15. **Reglas que siguen fuera del kernel**: NAV-07 (solo first-party publica anchors/contratos: hub), el slot `contract.providers` y los `settings.<familia>` por contrato (host), `nav.dedupe` como telemetría (el host emite a partir de `Tree.Dedupe`), la validación de los campos nuevos en el validador del manifest legacy (`manifest/validate.go`: los recibe ya validados por `v3`).

## Compatibilidad

- Un manifest sin campos nuevos valida igual (solo puede ganar el aviso `nav_target_unknown`) y `BuildFor` produce el mismo árbol (prueba golden). `Resolve` conserva cada entrada legacy una vez, en la sección de su `target`, en el orden de v1.
- Un host N-1 rechaza un manifest con `requires[].contract` (su schema exige `key` y no conoce `contract`): durante la ventana N5 el hub no debe servir esas entradas a hosts N-1 (puede omitirlas o sustituirlas por el dueño del contrato).
- SDK: regenerar con tygo `manifest-v3.ts` y `manifest.ts` (tipos nuevos `NavAnchor`, `ContractRequirement`; campos nuevos de `Requirement`, `NavGroup`, `NavItem`, `ExtensionPoints`, `Manifest`). `Requirement.key` pasa a opcional en TS.

## Evento `commerce.payments.refund_created`

Evento canónico aditivo en el dominio reservado `commerce.payments` (solo un proveedor de `commerce.payments.*` puede emitirlo; lo vigila `security.EmitPolicy`). Payload: `provider`, `refund_id`, `status` (`RefundStatuses`), `amount` (decimal en texto), `currency` (obligatorios); `order_id`, `order_ref`, `payment_id`, `provider_ref`, `occurred_at`, `source`, `refund` (objeto libre) opcionales. Los suscriptores deduplican por emisor + `refund_id` y tratan `manual_required` como «aún no emitido». `order_ref` se agregó porque `storefront_notifications` lo lee; `refund` porque ese addon acepta el detalle anidado o plano.

## Addendum: R-D' (sucesores que se enchufan)

Un `provider`/`extension` no puede tener navegación propia, pero SÍ items que se
enchufan (`attach_to` + `as`) a anchors de un dueño. `RoleFacts.OwnNavItems`
cuenta los items de primer nivel sin `attach_to` y los grupos sin items; la regla
R-D solo falla con `OwnNavItems > 0`. Además `navigation.CheckInstalled` rechaza
que un sucesor (`Addon.Role` provider|extension) se enchufe a un anchor cuyo
dueño no está en sus `requires` (por clave de addon, o por un contrato que el
dueño declara y el sucesor requiere). Sin `Role` el chequeo no corre (legado).
