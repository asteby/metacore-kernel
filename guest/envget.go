package guest

// EnvGet helpers wrap the `metacore_host.env_get` import documented in
// docs/wasm-abi.md § 3. The host import returns a packed (ptr<<32)|len
// pointing at the raw setting value bytes, or 0 when the key is
// missing. Unlike db_query / event_emit, env_get does NOT return a
// JSON envelope — the response is the value verbatim. The helper
// therefore distinguishes "missing key" from "empty value":
//
//   - Missing key → `("", false, nil)`
//   - Empty value → `("", false, nil)` (host treats empty as missing,
//     see runtime/wasm/capabilities.go line 134-137)
//   - Present value → `(value, true, nil)`
//
// There is no error path on the wire today — every plausible failure
// (host missing, key out of bounds, alloc failure) collapses into
// "missing" on the host side. The helper preserves the error return
// for forward-compat: a future host that introduces a typed envelope
// for env_get can populate it without breaking the helper signature.

// decodeEnvValue translates the raw bytes the host wrote into guest
// memory into the (value, found) tuple the helper exposes. Exposed for
// host-side tests; production consumers go through EnvGet.
//
// `buf == nil` (or empty) means the host returned 0 — the key was
// missing or the value was empty. The helper folds both into the
// `found == false` case, mirroring the host's existing contract.
func decodeEnvValue(buf []byte) (value string, found bool) {
	if len(buf) == 0 {
		return "", false
	}
	return string(buf), true
}
