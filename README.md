# metacore-kernel

Private runtime kernel for the Metacore platform. Hosts the WASM sandbox,
security enforcement, lifecycle, installer, navigation and event bus.
It consumes the public SDK (`metacore-sdk`) for manifest/bundle/dynamic
types and exposes no public API surface of its own.

## Module

```
github.com/asteby/metacore-kernel
```

## GOPRIVATE

This repository is private. Configure Go to skip the public module proxy:

```bash
go env -w GOPRIVATE=github.com/asteby/metacore-kernel
```

If you also pull `metacore-sdk` from a private source during early dev:

```bash
go env -w GOPRIVATE=github.com/asteby/metacore-kernel,github.com/asteby/metacore-sdk
```

## Layout

```
runtime/wasm/   WASM runtime (wazero)
security/      capability enforcer, HMAC, webhook dispatch
installer/     addon installer
host/          host-side bindings for guest modules
lifecycle/     addon lifecycle hooks
navigation/    nav tree
events/        event bus
```

## SDK consumption

During development, the SDK is wired via a local `replace` directive:

```go
replace github.com/asteby/metacore-sdk => ../metacore-sdk
```

For production builds the `replace` is dropped and a pinned tag is used:

```
require github.com/asteby/metacore-sdk vX.Y.Z
```

## Tests

```bash
go test ./...
```
