// Package importpolicy is the single source of truth for which imports a
// metacore wasm guest may declare: the metacore_host ABI functions the kernel
// registers, plus the closed set of WASI functions the Go wasip1 runtime needs
// for its own plumbing.
//
// Why it lives here. The kernel is the side that actually instantiates guests,
// so it owns the answer. The hub scanner (publish gate) and the addons CI
// preflight (PR gate) both enforced a COPY of these lists; every copy drifted
// at least once — the hub rejected real kernel exports (http_request,
// data_batch) as "forbidden", and the addons PR gate did not check imports at
// all, so fiscal_mexico@0.32.0 (a guest importing wasi path_open through
// time.LoadLocation) passed CI, merged, and died in the Release with
// scan_failed. Import this package instead of copying it.
//
// Pure: no wazero, no host, safe to import from any tool.
package importpolicy

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"sort"
)

// HostModule is the module name of the metacore ABI imports.
const HostModule = "metacore_host"

// WASIModule is the snapshot module every GOOS=wasip1 Go build imports.
const WASIModule = "wasi_snapshot_preview1"

// HostFunctions is every function the kernel registers on HostModule
// (runtime/wasm/capabilities.go). TestHostFunctionsMatchRegisteredModule in
// runtime/wasm fails if this set and the registered module ever differ.
var HostFunctions = map[string]struct{}{
	"log":              {},
	"env_get":          {},
	"http_fetch":       {},
	"http_request":     {},
	"connector_get":    {},
	"event_emit":       {},
	"db_query":         {},
	"db_exec":          {},
	"data_mutate":      {},
	"data_batch":       {},
	"data_query":       {},
	"approval_request": {},
	"sequence_next":    {},
	"ctx_get":          {},
	"routing_resolve":  {},
}

// WASIFunctions is the closed set of WASI imports a guest may declare:
// exactly the Go runtime's plumbing (clock, random, stdio, scheduler, args,
// env, the preopen probe). Filesystem walks (path_open and friends) and
// sockets stay forbidden — a guest has no disk; in Go they usually sneak in
// through os.Open/os.ReadFile or time.LoadLocation.
var WASIFunctions = map[string]struct{}{
	"args_get":            {},
	"args_sizes_get":      {},
	"clock_time_get":      {},
	"environ_get":         {},
	"environ_sizes_get":   {},
	"fd_close":            {},
	"fd_fdstat_get":       {},
	"fd_fdstat_set_flags": {},
	"fd_prestat_get":      {},
	"fd_prestat_dir_name": {},
	"fd_read":             {},
	"fd_seek":             {},
	"fd_write":            {},
	"poll_oneoff":         {},
	"proc_exit":           {},
	"random_get":          {},
	"sched_yield":         {},
}

// Allowed reports whether a guest may import module.name, and why not.
func Allowed(module, name string) error {
	switch module {
	case HostModule:
		if _, ok := HostFunctions[name]; !ok {
			return fmt.Errorf("forbidden host function %q.%q: the kernel does not register it", module, name)
		}
	case WASIModule:
		if _, ok := WASIFunctions[name]; !ok {
			return fmt.Errorf("forbidden WASI import %q.%q — outside the Go-runtime plumbing whitelist", module, name)
		}
	default:
		return fmt.Errorf("forbidden import module %q (%q): only %s and %s are provided", module, name, HostModule, WASIModule)
	}
	return nil
}

// Import is one function import declared by a module.
type Import struct {
	Module string
	Name   string
}

// FunctionImports lists the function imports of a wasm binary by reading its
// import section (id 2) — no runtime, no compilation.
func FunctionImports(wasm []byte) ([]Import, error) {
	if len(wasm) < 8 || !bytes.Equal(wasm[:4], []byte{0, 'a', 's', 'm'}) {
		return nil, fmt.Errorf("not a wasm module: bad magic bytes")
	}
	r := bytes.NewReader(wasm[8:])
	for r.Len() > 0 {
		id, _ := r.ReadByte()
		size, err := binary.ReadUvarint(r)
		if err != nil || int64(size) > int64(r.Len()) {
			return nil, fmt.Errorf("truncated section")
		}
		body := make([]byte, size)
		_, _ = io.ReadFull(r, body)
		if id != 2 {
			continue
		}
		return parseImportSection(body)
	}
	return nil, nil
}

func parseImportSection(body []byte) ([]Import, error) {
	br := bytes.NewReader(body)
	name := func() (string, error) {
		l, err := binary.ReadUvarint(br)
		if err != nil || int64(l) > int64(br.Len()) {
			return "", fmt.Errorf("bad import name")
		}
		b := make([]byte, l)
		_, _ = io.ReadFull(br, b)
		return string(b), nil
	}
	uv := func() error { _, err := binary.ReadUvarint(br); return err }
	limits := func() error {
		flags, err := br.ReadByte()
		if err != nil {
			return err
		}
		if err := uv(); err != nil {
			return err
		}
		if flags&1 != 0 {
			return uv()
		}
		return nil
	}
	n, err := binary.ReadUvarint(br)
	if err != nil {
		return nil, fmt.Errorf("bad import section")
	}
	var out []Import
	for i := uint64(0); i < n; i++ {
		mod, err := name()
		if err != nil {
			return nil, err
		}
		fn, err := name()
		if err != nil {
			return nil, err
		}
		kind, err := br.ReadByte()
		if err != nil {
			return nil, fmt.Errorf("bad import kind")
		}
		switch kind {
		case 0: // func: type index
			err = uv()
			if err == nil {
				out = append(out, Import{Module: mod, Name: fn})
			}
		case 1: // table: reftype + limits
			if _, err = br.ReadByte(); err == nil {
				err = limits()
			}
		case 2: // memory: limits
			err = limits()
		case 3: // global: valtype + mut
			_, err = br.Seek(2, io.SeekCurrent)
		default:
			err = fmt.Errorf("unknown import kind %d", kind)
		}
		if err != nil {
			return nil, fmt.Errorf("bad import %s.%s: %w", mod, fn, err)
		}
	}
	return out, nil
}

// Check returns one violation per function import the module may not declare,
// sorted, or the parse error.
func Check(wasm []byte) ([]string, error) {
	imports, err := FunctionImports(wasm)
	if err != nil {
		return nil, err
	}
	var bad []string
	for _, im := range imports {
		if err := Allowed(im.Module, im.Name); err != nil {
			bad = append(bad, err.Error())
		}
	}
	sort.Strings(bad)
	return bad, nil
}
