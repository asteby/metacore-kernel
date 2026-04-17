# Release Process — metacore-kernel

Este documento describe el flujo completo de release del kernel Go. El kernel es una **library**, no un binario; no se distribuyen artefactos ejecutables. La distribución se hace vía el Go module proxy (`proxy.golang.org`) y GitHub Releases con changelog auto-generado.

---

## 1. Decidir la versión (SemVer)

El kernel sigue [Semantic Versioning 2.0](https://semver.org/) de manera estricta porque el Go module graph lo exige.

- **PATCH** (`v0.1.0` -> `v0.1.1`): bug fixes, optimizaciones internas, refactor sin cambio de API pública.
- **MINOR** (`v0.1.0` -> `v0.2.0`): nuevas features, nuevos símbolos exportados, deprecaciones marcadas con `// Deprecated:`. Backward-compatible.
- **MAJOR** (`v0.x` -> `v1.0`, `v1.x` -> `v2.x`): breaking changes en API exportada, firmas de funciones, tipos públicos, comportamiento documentado. En Go, `v2+` requiere path suffix: `github.com/asteby/metacore-kernel/v2`.

Mientras el kernel esté en `v0.x`, las minor pueden incluir breaking changes, pero seguimos la convención estricta desde el principio para facilitar la transición a `v1.0`.

### ¿Cómo decidir?

1. Revisa commits desde el último tag: `git log $(git describe --tags --abbrev=0)..HEAD --oneline`.
2. ¿Hay `feat:`? -> minor.
3. ¿Hay `fix:` o sólo `chore:`/`docs:`/`test:`? -> patch.
4. ¿Hay `BREAKING CHANGE:` en el body o `!` después del tipo (`feat!:`)? -> major.

---

## 2. Crear y publicar el tag

```bash
# Desde main, con el árbol limpio:
git checkout main
git pull --ff-only
git status   # debe estar limpio

# Verificar tests locales antes de tagear:
go test -race ./...

# Tag anotado (recomendado) — incluye mensaje y autor:
git tag -a v0.1.0 -m "Release v0.1.0"

# Push del tag dispara el workflow:
git push origin v0.1.0
```

Alternativa (push de todos los tags pendientes):

```bash
git push --tags
```

---

## 3. Qué pasa automáticamente

Cuando el tag llega a GitHub, el workflow `.github/workflows/release.yml` ejecuta:

1. **Checkout + setup Go 1.25** con cache de modules.
2. **Tests con race detector** — `go test -race ./...`. Si fallan, el release aborta.
3. **Ping al Go proxy** — `curl https://proxy.golang.org/github.com/asteby/metacore-kernel/@v/v0.1.0.info` fuerza la indexación inmediata. Sin este paso el proxy tarda unos minutos en descubrir el tag.
4. **GoReleaser** (`release --clean`) — crea el GitHub Release con:
   - Changelog categorizado por tipo de commit (features, fixes, other).
   - Archive `source` (tar.gz del repo en ese tag).
   - Checksums.
   - Marca `prerelease` automáticamente si el tag contiene `-alpha`, `-beta`, `-rc`, etc.
5. **Dispatch a consumidoras** — hace `POST /repos/{owner}/{repo}/dispatches` a cada app consumidora (`ops`, `link`, `pilot`, `doctores.lat`, `p2p`, `visor`, `operador360`) con `event_type=metacore-kernel-released`. Eso permite que cada consumidora defina un workflow `on: repository_dispatch` que dispare Renovate on-demand en lugar de esperar al cron.

> El token `CROSSREPO_DISPATCH_TOKEN` necesita scope `repo` y acceso a todas las orgs/repos consumidores. Si falla algún dispatch, el step usa `continue-on-error: true` para no romper el release.

---

## 4. Verificar que el release funcionó

```bash
# 1. GitHub Release
gh release view v0.1.0 --repo asteby/metacore-kernel

# 2. Go proxy indexado
curl -s https://proxy.golang.org/github.com/asteby/metacore-kernel/@v/list
curl -s https://proxy.golang.org/github.com/asteby/metacore-kernel/@v/v0.1.0.info | jq

# 3. pkg.go.dev (tarda 5-30 min en mostrar la nueva versión)
open https://pkg.go.dev/github.com/asteby/metacore-kernel@v0.1.0
```

---

## 5. Consumir desde una app

En cualquier repo consumidor (`ops`, `link`, ...):

```bash
# Si el módulo es privado, configurar GOPRIVATE una sola vez:
go env -w GOPRIVATE=github.com/asteby/*

# Traer la versión publicada:
go get github.com/asteby/metacore-kernel@v0.1.0
go mod tidy
```

Renovate detectará el nuevo tag y abrirá un PR automático en cada consumidora dentro de su siguiente schedule (o inmediatamente si reciben el `repository_dispatch`).

---

## 6. Renovate en apps consumidoras

Cada app consumidora debe tener un `renovate.json` (ver `docs/consumer-renovate-template.json` en este repo).

Regla por defecto:

- **patch + minor** de `github.com/asteby/metacore-kernel` -> **auto-merge** (platformAutomerge en GitHub).
- **major** -> PR abierto con label `breaking`, requiere review humana.

El auto-merge de Renovate sólo procede si el CI de la consumidora pasa verde. Si los tests rompen, el PR queda abierto para intervención manual.

---

## 7. Pre-releases (alpha / beta / rc)

Los pre-releases permiten probar cambios en consumidoras antes del release estable.

```bash
# Alpha/beta/rc siguen SemVer:
git tag -a v0.2.0-alpha.1 -m "Pre-release v0.2.0-alpha.1"
git push origin v0.2.0-alpha.1
```

GoReleaser marca el GitHub Release como `prerelease: true` automáticamente al detectar el sufijo SemVer. Renovate, por defecto, ignora prereleases — las consumidoras no reciben auto-PR. Para probar un prerelease en una app:

```bash
go get github.com/asteby/metacore-kernel@v0.2.0-alpha.1
```

---

## 8. Rollback / Retract

**Importante**: no se puede "borrar" una versión del Go module proxy. Una vez indexada, está disponible para siempre (inmutabilidad es una garantía fundamental del ecosistema Go).

Para marcar una versión como defectuosa usamos `retract` en `go.mod`:

```bash
# 1. Añadir directiva retract en go.mod:
go mod edit -retract=v0.1.0

# 2. Añadir rationale en un comentario:
#    // v0.1.0 filtraba credenciales en logs; usar v0.1.1+
```

Resultado en `go.mod`:

```go
module github.com/asteby/metacore-kernel

go 1.25

retract (
    v0.1.0 // filtraba credenciales en logs; usar v0.1.1+
)
```

3. Hacer commit, tag nuevo (`v0.1.1` con el fix real + el retract), push.
4. Las consumidoras que hagan `go get -u` verán un warning y serán redirigidas a la siguiente versión válida.

Para retractar un rango:

```go
retract [v0.1.0, v0.1.5]
```

---

## 9. Troubleshooting

| Síntoma | Causa probable | Fix |
|---|---|---|
| `go get` dice "unknown revision" | proxy no indexó aún | `GOPROXY=direct go get ...` o esperar 5 min |
| Workflow `Release` falla en tests | race condition reciente | fixear en `main`, retag con versión bumpeada |
| pkg.go.dev no muestra la nueva versión | índice desactualizado | abrir `https://pkg.go.dev/github.com/asteby/metacore-kernel@vX.Y.Z` una vez para forzar fetch |
| Dispatch a consumidoras falla | token sin scope | regenerar `CROSSREPO_DISPATCH_TOKEN` con `repo` scope |
| Consumidora no recibe auto-PR | Renovate deshabilitado o `GOPRIVATE` mal config | revisar `renovate.json` + `hostRules.token` |

---

## 10. Referencias

- [SemVer 2.0](https://semver.org/)
- [Go module reference — retract](https://go.dev/ref/mod#go-mod-file-retract)
- [GoReleaser for libraries](https://goreleaser.com/customization/builds/#skipping-builds)
- [Renovate gomod manager](https://docs.renovatebot.com/modules/manager/gomod/)
