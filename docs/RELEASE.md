# Release process

`metacore-kernel` is distributed as a **Go module**, not as a binary. There
are no executable artifacts; releases exist as Git tags indexed by
`proxy.golang.org`, with GitHub Releases providing a categorized changelog
on top.

This document is the source of truth for cutting a release and for
recovering from a botched one.

---

## Table of contents

1. [Choose the version (SemVer)](#1-choose-the-version-semver)
2. [Cut and publish the tag](#2-cut-and-publish-the-tag)
3. [What runs automatically](#3-what-runs-automatically)
4. [Verify the release](#4-verify-the-release)
5. [Consume from a host application](#5-consume-from-a-host-application)
6. [Renovate in consumer repositories](#6-renovate-in-consumer-repositories)
7. [Pre-releases](#7-pre-releases)
8. [Rollback / retract](#8-rollback--retract)
9. [Troubleshooting](#9-troubleshooting)
10. [References](#10-references)

---

## 1. Choose the version (SemVer)

The kernel follows [SemVer 2.0](https://semver.org/) strictly because the Go
module graph requires it.

| Bump  | Triggered by                                                      |
| ----- | ----------------------------------------------------------------- |
| Patch | `fix:` commits, internal refactors, optimisations, doc-only       |
| Minor | `feat:` commits, new exported symbols, deprecations marked `// Deprecated:` |
| Major | `feat!:` / `BREAKING CHANGE:` body, removed/renamed exports, interface changes |

Major bumps require the `/v2` (or higher) module-path suffix:
`github.com/asteby/metacore-kernel/v2`. Plan for the import-path migration
in consumers ahead of time — Renovate cannot rewrite the suffix on its own.

While the kernel is in `v0.x`, minor bumps may technically include breaking
changes. We still mark them as such in the changelog so consumers can tell
the difference.

### How to decide

```bash
# What landed since the last tag?
git log $(git describe --tags --abbrev=0)..HEAD --oneline
```

- Any `feat!:` / `BREAKING CHANGE:` lines  → major.
- Any `feat:` lines                        → minor.
- Only `fix:` / `chore:` / `docs:` / `test:` → patch.

The first commit message that introduces a breaking change wins, regardless
of how many fixes ship alongside it.

## 2. Cut and publish the tag

```bash
# From a clean main branch:
git checkout main
git pull --ff-only
git status                          # must be clean
go test -race ./...                 # match CI

# Annotated tag — required for GoReleaser to pick up the message:
git tag -a v0.2.0 -m "Release v0.2.0"
git push origin v0.2.0
```

To push every pending tag at once:

```bash
git push --tags
```

## 3. What runs automatically

`.github/workflows/release.yml` triggers on every `v*` tag and runs:

1. **Checkout + Go 1.25** with module cache.
2. **Tests with the race detector** (`go test -race ./...`). Failure aborts
   the release.
3. **Go proxy ping** — `curl https://proxy.golang.org/.../@v/<tag>.info` to
   force immediate indexing. Without it, `go get` may report
   `unknown revision` for several minutes.
4. **GoReleaser** (`release --clean`) creates the GitHub Release with:
   - Categorized changelog (features / fixes / other).
   - Source archive (`.tar.gz`).
   - Checksums.
   - Automatic `prerelease: true` when the tag carries a SemVer suffix
     (`-alpha`, `-beta`, `-rc`).
Consumers are not notified by the kernel. They discover new versions on
their own through Renovate / Dependabot polling the Go proxy — see
section 6.

## 4. Verify the release

```bash
# 1. GitHub Release exists
gh release view v0.2.0 --repo asteby/metacore-kernel

# 2. Go proxy indexed the tag
curl -s https://proxy.golang.org/github.com/asteby/metacore-kernel/@v/list
curl -s https://proxy.golang.org/github.com/asteby/metacore-kernel/@v/v0.2.0.info | jq

# 3. pkg.go.dev (5–30 minutes after release)
open https://pkg.go.dev/github.com/asteby/metacore-kernel@v0.2.0
```

## 5. Consume from a host application

In any consumer repository:

```bash
go env -w GOPRIVATE="github.com/asteby/*"   # one time per machine
go get github.com/asteby/metacore-kernel@v0.2.0
go mod tidy
```

Renovate, configured per [`CONSUMER_GUIDE.md`](./CONSUMER_GUIDE.md#8-renovate-template),
opens the PR automatically on the next schedule tick.

## 6. Renovate in consumer repositories

Every consumer ships a `renovate.json` derived from
[`docs/consumer-renovate-template.json`](./consumer-renovate-template.json).
Default policy:

- **patch + minor** → auto-merge (GitHub `platformAutomerge`).
- **major** → PR opened with `breaking` and `review-required` labels.

Auto-merge proceeds only when the consumer's CI is green. A failing test
suite leaves the PR open for human intervention.

## 7. Pre-releases

Pre-releases let consumers exercise upcoming changes before a stable tag.

```bash
git tag -a v0.3.0-alpha.1 -m "Pre-release v0.3.0-alpha.1"
git push origin v0.3.0-alpha.1
```

GoReleaser flags the GitHub Release as `prerelease: true` automatically.
Renovate ignores prereleases by default — to opt a consumer into testing
one, run `go get github.com/asteby/metacore-kernel@v0.3.0-alpha.1` manually
in that repository.

## 8. Rollback / retract

Go modules are **immutable** — once a version is on `proxy.golang.org`, it
stays there forever. To mark a release as defective, use `retract` in
`go.mod`:

```bash
# 1. Add a retract directive with a rationale comment.
go mod edit -retract=v0.2.0
```

Resulting `go.mod`:

```go
module github.com/asteby/metacore-kernel

go 1.25

retract (
    v0.2.0 // leaked credentials in logs; use v0.2.1+
)
```

Steps:

1. Land the retract directive (and the actual fix) on `main`.
2. Tag a new patch version that includes both (`v0.2.1`).
3. Push the tag — the release workflow indexes it on the Go proxy.
4. Consumers running `go get -u` will see a warning and resolve to the next
   non-retracted version.

To retract a contiguous range:

```go
retract [v0.2.0, v0.2.4]
```

## 9. Troubleshooting

| Symptom                                   | Likely cause                          | Fix                                                                  |
| ----------------------------------------- | ------------------------------------- | -------------------------------------------------------------------- |
| `go get` reports `unknown revision`       | Proxy not indexed yet                 | `GOPROXY=direct go get …` or wait 5 minutes                          |
| Release workflow fails on tests           | Recent race condition                 | Fix on `main`, re-tag with the next patch version                    |
| `pkg.go.dev` does not show the new version | Index lag                             | Open `https://pkg.go.dev/github.com/asteby/metacore-kernel@vX.Y.Z` to force the fetch |
| Consumer never receives a Renovate PR     | Renovate disabled or `GOPRIVATE` mis-configured | Inspect `renovate.json` and `hostRules.token`                       |

## 10. References

- [SemVer 2.0](https://semver.org/)
- [Go module reference — `retract`](https://go.dev/ref/mod#go-mod-file-retract)
- [GoReleaser for libraries](https://goreleaser.com/customization/builds/#skipping-builds)
- [Renovate `gomod` manager](https://docs.renovatebot.com/modules/manager/gomod/)
- [Conventional Commits](https://www.conventionalcommits.org/en/v1.0.0/)
