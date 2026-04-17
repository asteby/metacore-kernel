# Development setup

1. Clone both repos side by side:

   ```
   ~/projects/metacore-sdk
   ~/projects/metacore-kernel
   ```

2. The `go.mod` has `replace github.com/asteby/metacore-sdk => ../metacore-sdk`
   so changes in the SDK are picked up immediately during local development.

3. Before tagging a release, run:

   ```bash
   go mod edit -dropreplace github.com/asteby/metacore-sdk
   go mod tidy
   ```

   to pin to a published SDK version.

## Private module access

To pull the (private) SDK and kernel modules from CI or another machine,
configure `GOPRIVATE` and `~/.netrc` as documented in
`metacore-sdk/docs/internal-setup.md`.
