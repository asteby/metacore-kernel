package validate

import "sync"

// CustomFunc is a named extra check (e.g. "rfc.tax_id"). Return a non-empty
// code to fail; empty code = pass. Params are interpolated by the SDK.
type CustomFunc func(value any) (code string, params map[string]any)

// Resolver looks up a custom slug. Returning nil means "unknown — skip"
// so an unresolved `$org.*` reference never crashes a write (matches the
// SDK's pass-through for missing org config).
type Resolver func(slug string) CustomFunc

var (
	builtinMu sync.RWMutex
	builtins  = map[string]CustomFunc{
		CodeEmail:   builtinEmail,
		CodeUUID:    builtinUUID,
		CodeURL:     builtinURL,
		CodeNumeric: builtinNumeric,
		CodeInteger: builtinInteger,
		"int":       builtinInteger,
	}
)

// Register installs a process-wide custom validator by slug. Hosts and
// addons call this at boot (e.g. Register("rfc.tax_id", ...)). Re-registering
// the same slug replaces the previous func. Builtin names (email/uuid/url/
// numeric/integer) may be overridden the same way.
func Register(slug string, fn CustomFunc) {
	if slug == "" || fn == nil {
		return
	}
	builtinMu.Lock()
	builtins[slug] = fn
	builtinMu.Unlock()
}

func lookupCustom(slug string, resolver Resolver) CustomFunc {
	if slug == "" {
		return nil
	}
	if resolver != nil {
		if fn := resolver(slug); fn != nil {
			return fn
		}
	}
	builtinMu.RLock()
	fn := builtins[slug]
	builtinMu.RUnlock()
	return fn
}
