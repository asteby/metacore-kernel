# Dominios de la organización (`domains`) y capability `domain:manage`

Primitivo de plataforma que generaliza el flujo de dominios propios de la tienda
(`ops/backend/handlers/storefront_domains.go`) para cualquier addon (correo,
tienda, portal, SSO). Código: paquete `domains/`. La capability
`domain:manage` y el import wasm `dns_resolve` lo exponen a los addons.

## Modelo

```go
type OrgDomain struct {
    ID, OrganizationID uuid.UUID
    Host        string    // minúsculas, sin esquema/puerto/punto final
    Purposes    []string  // "storefront", "mail", ... (≤8, ^[a-z][a-z0-9_]{0,31}$)
    VerifyToken string    // "asteby-verify-" + 128 bits
    Status      Status    // pending | verified | disabled
    DNSProvider string    // "manual" (default) | "cloudflare" | ...
    VerifiedAt  *time.Time
}
```

`NewOrgDomain`, `NormalizeHost`, `ValidateHost` (mismos textos que el flujo de
la tienda: `host is required`, `host must be a fully qualified domain name`, …),
`NewVerifyToken`, `VerifyRecordName` (`_asteby-verify.<host>`) y
`ContainsToken` (igualdad exacta tras quitar comillas, **no** subcadena) son
equivalentes byte a byte a los helpers privados de ops: ops puede reemplazarlos
sin cambiar comportamiento.

## Propiedad (TXT `_asteby-verify.`)

`CheckOwnership(ctx, resolver, host, token) → {Verified, Status, Record, Observed, Diagnostic}`

| Status | Significa |
|---|---|
| `ok` | el token está publicado |
| `missing` | el nombre no tiene TXT |
| `wrong_value` | hay TXT pero ninguno con el token |
| `unknown` | **falló la consulta** (timeout, SERVFAIL): reintentar; jamás decir "falta" |

Solo `Verified == true` puede pasar un `OrgDomain` a `verified`.

## RecordSet: requerido vs observado

```go
set := domains.RecordSet{Domain: "example.com", Records: []domains.Record{
    {Type: domains.TypeMX,  Name: "example.com", Value: "mx.mail.test", Priority: 10, Purpose: "mx"},
    {Type: domains.TypeTXT, Name: "example.com", Value: "v=spf1 include:_spf.mail.test ~all",
        Purpose: "spf", Unique: true, Match: domains.MatchContains, Needle: "include:_spf.mail.test"},
    {Type: domains.TypeTXT, Name: "_dmarc.example.com", Value: "v=DMARC1; p=none; rua=mailto:d@example.com",
        Purpose: "dmarc", Unique: true, Match: domains.MatchPrefix, Needle: "v=DMARC1"},
}}
check, err := domains.Check(ctx, []domains.DNSResolver{sys, cf1111, g8888}, set, domains.CheckOptions{
    EvalOptions: domains.EvalOptions{AppliedAt: lastApplied}})
```

`Value` es lo que se **escribe**; `Needle` (con `Match` contains/prefix) es lo que
se **exige**, para que los mecanismos extra del SPF o la política DMARC afinada
del cliente sigan verificando.

Veredicto por registro (`RecordCheck.Status`):

| Estado | Cuándo |
|---|---|
| `ok` | satisface el requisito y ningún resolvedor lo contradice |
| `missing` | ausencia autoritativa (NXDOMAIN / sin datos) |
| `wrong_value` | existen registros pero ninguno sirve (valor distinto, **dos SPF**, **MX con menor preferencia ajeno**) |
| `propagating` | algunos resolvedores lo ven y otros no, o se escribió hace menos de `Window` (1h) y aún no se ve |
| `unknown` | la consulta misma falló (no es una afirmación sobre la zona) |

`Diagnostics[]` (`code`, `message_key` = `domains.diag.<code>`, `severity`,
`params`, `detail`): `not_found`, `value_mismatch`, `multiple_spf`,
`mx_shadowed`, `partial_propagation`, `recently_applied`, `lookup_failed`,
`unique_has_extra_values`, `ownership_token_missing|mismatch`.
`RecordSetCheck.AllOK` ignora los registros `Optional`. `Evaluate` es la versión
pura (sin E/S) para hosts que ya tienen las respuestas.

`DNSResolver` es una interfaz de una sola función (`Lookup(ctx,name,type)`);
`NewNetResolver(label, "1.1.1.1:53")` la implementa con la stdlib (varios
resolvedores públicos = observar propagación real).

## Aplicar (Cloudflare detrás de interfaz)

```go
ap := domains.NewProviderApplier(domains.NewCloudflare(token))   // DNSApplier
plan, _ := ap.Plan(ctx, set)          // solo lectura: zona + diff
// UI muestra plan.Entries (create/update/noop/blocked + warnings) y plan.Fingerprint()
res, plan, err := domains.ApplyConfirmed(ctx, ap, set, confirmedFingerprint)
```

- Token mínimo: **Zone:Read + DNS:Edit** sobre la zona del cliente. Sin clave
  global, sin llamada de borrado en el tipo.
- **Upsert idempotente, nunca borra.** `Unique` (SPF, DMARC, DKIM, CNAME)
  *actualiza* el valor existente con warning `replaces_existing`; un MX junto a
  otro ajeno se *crea* con `mx_other_target` (los MX del cliente se quedan);
  un CNAME que choca con otro tipo queda `blocked`. Reaplicar un plan viejo no
  duplica (cada creación re-lista el nombre antes de escribir).
- `Fingerprint()` ata la confirmación al diff exacto: si la zona cambió mientras
  el diálogo estaba abierto, `ApplyConfirmed` devuelve `ErrPlanChanged` y el
  plan nuevo, sin escribir.
- Registros A/AAAA/CNAME se crean `proxied=false`. El token no sale en `String()`,
  `%v`/`%#v` ni en errores. Errores tipados: `ErrProviderAuth`, `ErrZoneNotFound`,
  `ErrRateLimited`.
- Otros proveedores (Route53, DigitalOcean…) = implementar `ZoneProvider`
  (`FindZone`, `ListRecords`, `CreateRecord`, `UpdateRecord`).
- Domain Connect y "copiar/pegar" no necesitan aplicador: copiar/pegar es
  `Check` en bucle sobre el `RecordSet`.

## Capability `domain:manage`

`{"kind":"domain:manage","target":"mail"}`: el `target` es el **propósito**
(`mail`, glob o `*`). `security.Capabilities.CanManageDomain(purpose)` es la
guarda para el host al exponer register/verify/apply; el host además filtra por
`OrgDomain.Purposes`.

## Import wasm `dns_resolve` (ABI 1.17, aditivo)

```
dns_resolve(reqPtr i32, reqLen i32) -> i64      [v1.17]
req:  {"name":"_asteby-verify.acme.mx","type":"TXT"}     // A|AAAA|CNAME|MX|TXT
resp: {"success":true,"data":{"name","type","values":[…],"no_records":bool},"meta":{…}}
```

- Exige `domain:manage` **y** la política `DNSAuthorizeFunc` del host
  (`Host.WithDNS(resolver, authorize)`): el nombre debe ser (o colgar de) un
  dominio de la organización invocante. Sin autorizador → `forbidden` (cerrado
  por defecto, no es un oráculo DNS abierto). Sin resolver → `dns_unavailable`.
- Respuesta vacía autoritativa = éxito con `no_records:true`. Fallo de consulta =
  error `dns_lookup_failed` (nunca "no existe").
- 64 consultas por invocación (`rate_limited`), 5 s por consulta.
- Guests compilados contra 1.0–1.16 siguen funcionando: es un import nuevo.
