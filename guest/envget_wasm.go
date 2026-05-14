//go:build wasm || wasip1

package guest

// hostEnvGet is the raw host import.
//
//go:wasmimport metacore_host env_get
func hostEnvGet(keyPtr, keyLen uint32) uint64

// EnvGet reads an installation setting (or secret) by key. The host
// returns the value bytes directly — there is no JSON envelope to
// decode.
//
// Returns:
//   - `(value, true, nil)` when the key is present and non-empty.
//   - `("", false, nil)` when the key is missing or maps to an empty
//     value. The host folds both into a single "0" return (see
//     runtime/wasm/capabilities.go line 134-137); we surface the same
//     distinction the host exposes.
//   - `(value, found, err)` is reserved for a future host that adds a
//     typed envelope. Today `err` is always nil; keep the slot in the
//     signature so consumers don't need to chase a breaking API change
//     when env_get gains a forbidden / not_provisioned code path.
//
// Memory contract: the response bytes live in guest memory until the
// next host call allocates over them. The helper copies the bytes
// into a Go string before returning, so the result is safe to retain.
func EnvGet(key string) (value string, found bool, err error) {
	keyPtr, keyLen := stringRef(key)
	packed := hostEnvGet(keyPtr, keyLen)
	buf := readPacked(packed)
	v, ok := decodeEnvValue(buf)
	return v, ok, nil
}
