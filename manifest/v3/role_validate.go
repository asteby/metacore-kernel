package v3

import (
	"fmt"
	"reflect"
	"regexp"
	"sort"
	"strings"

	"github.com/Masterminds/semver/v3"
	"github.com/asteby/metacore-kernel/capability"
)

// Role validation (X-R1): the base + successors pattern. The rules are pure
// functions over RoleFacts so the v3 validator and the legacy host manifest
// (manifest.Manifest) enforce exactly the same thing.
//
// Compatibility window (one release): a manifest WITHOUT metadata.role keeps
// installing. Its role is inferred (InferRole) and every finding that would be
// a hard error for a declared role comes back as a warning instead. Only a
// declared role is enforced.
const (
	// RuleRoleInferred: a role rule is violated by a manifest whose role is
	// only inferred. Warning-only until the role is declared.
	RuleRoleInferred = "role_inferred"
	// RuleRoleProviderForeignWrite: a provider holds db:write outside its own
	// namespace and no catalog was supplied to check the contract's
	// writable_models. Warning-only.
	RuleRoleProviderForeignWrite = "role_provider_foreign_write"
)

// RoleCatalog lets a host that knows other addons (the hub, ops) complete the
// rules that cannot be decided from one manifest. All methods are optional in
// effect: a nil RoleCatalog skips those checks.
type RoleCatalog interface {
	// RoleOf returns the declared (or inferred) role of an addon key.
	RoleOf(addonKey string) (role string, ok bool)
	// WritableTables returns the TABLES a provider of the contract may
	// db:write, and whether the contract exists in the catalog. They are
	// tables, not model keys: a capability's db:write target names a table,
	// while a contract's writable_models name models of its owner. The catalog
	// resolves model -> table from the owner's manifest (ContractWritableTables
	// does exactly that); the comparison is case-insensitive on the table.
	WritableTables(contractKey string) (tables []string, ok bool)
}

// CheckRoles evaluates the role rules. errs are hard errors, warns are
// "[rule] message" warnings. The invalid-role-value case is always an error.
func CheckRoles(f RoleFacts, cat RoleCatalog) (errs, warns []string) {
	if nilCatalog(cat) {
		cat = nil
	}
	declared := f.Role != ""
	if declared && !ValidRole(f.Role) {
		return []string{fmt.Sprintf("metadata.role %q is not one of %s", f.Role, strings.Join(Roles, "|"))}, nil
	}
	role := f.Role
	if !declared {
		// role_missing is reported by RoleMissingWarnings; here the inferred
		// role only decides which rules apply, as warnings.
		role = InferRole(f)
	}
	// report files a violation: error for a declared role, warning otherwise.
	report := func(msg string) {
		if declared {
			errs = append(errs, msg)
			return
		}
		warns = append(warns, fmt.Sprintf("[%s] %s", RuleRoleInferred, msg))
	}
	isSuccessor := role == RoleProvider || role == RoleExtension

	if len(f.Contracts) > 0 && role != RoleContract {
		report("contracts[] requires metadata.role \"contract\"")
	}
	if declared && role == RoleContract && len(f.Contracts) == 0 {
		report("metadata.role \"contract\" requires at least one contracts[] entry")
	}
	if len(f.Implements) > 0 && !isSuccessor {
		report("implements[] requires metadata.role \"provider\" or \"extension\"")
	}
	if declared && role == RoleProvider && len(f.Implements) == 0 {
		report("metadata.role \"provider\" requires at least one implements[] entry")
	}
	if f.ExtendsAddon != nil && role != RoleExtension {
		report("extends_addon requires metadata.role \"extension\"")
	}
	if declared && role == RoleExtension && f.ExtendsAddon == nil {
		report("metadata.role \"extension\" requires extends_addon")
	}
	if f.ExtendsAddon != nil {
		found := false
		for _, r := range f.Requires {
			if r.Key == f.ExtendsAddon.Key && !r.Optional {
				found = true
			}
		}
		if !found {
			report(fmt.Sprintf("extends_addon.key %q must also be a (non-optional) compatibility.requires entry", f.ExtendsAddon.Key))
		}
		if f.ExtendsAddon.Key == f.Key {
			report("extends_addon.key cannot be the addon itself")
		}
		if _, err := semver.NewConstraint(f.ExtendsAddon.Version); err != nil {
			errs = append(errs, fmt.Sprintf("extends_addon.version %q is not a valid semver range: %v", f.ExtendsAddon.Version, err))
		}
	}
	if role == RolePreset && (len(f.OwnModelKeys) > 0 || len(f.Contracts) > 0 || len(f.Implements) > 0 || f.ExtendsAddon != nil) {
		report("a preset carries configuration only: no models, contracts, implements or extends_addon")
	}
	for i, r := range f.Replaces {
		if r == f.Key {
			errs = append(errs, fmt.Sprintf("replaces[%d] %q cannot be the addon itself", i, r))
		}
	}

	errs = append(errs, checkContracts(f)...)
	errs, warns = checkImplements(f, cat, declared, errs, warns)

	// R-D: successors have no navigation of their own.
	if isSuccessor && f.NavigationGroups > 0 {
		report(fmt.Sprintf("R-D: a %s has no navigation of its own (contributions.navigation); it enters through the contract's provider slot", role))
	}
	// R-C: a provider only publishes capabilities of the contracts it implements.
	if role == RoleProvider && len(f.Implements) > 0 {
		for _, k := range f.ProvidedCapabilities {
			ok := false
			for _, im := range f.Implements {
				if k == im.Contract || strings.HasPrefix(k, im.Contract+".") {
					ok = true
				}
			}
			if !ok {
				report(fmt.Sprintf("R-C: provides_capabilities %q is outside the implemented contracts", k))
			}
		}
	}
	// R-B: a provider writes only its own namespace or what its contract opens.
	if role == RoleProvider {
		for _, t := range f.DBWrites {
			if ownWriteTarget(f, t) {
				continue
			}
			if cat != nil {
				if writableByContract(f, cat, t) {
					continue
				}
				report(fmt.Sprintf("R-B: db:write %q is outside the provider's namespace and not in writable_models of its contracts", t))
				continue
			}
			warns = append(warns, fmt.Sprintf("[%s] db:write %q is outside the provider's namespace; the contract's writable_models were not checked", RuleRoleProviderForeignWrite, t))
		}
	}
	// R-A: only an extension may require a provider.
	if cat != nil && role != RoleExtension {
		for _, r := range f.Requires {
			if rr, ok := cat.RoleOf(r.Key); ok && rr == RoleProvider {
				report(fmt.Sprintf("R-A: %q is a provider; only an extension may require a provider (consume it through its contract)", r.Key))
			}
		}
	}
	sort.Strings(warns)
	return errs, warns
}

// bareTable reduces a db:write target to its table name: real targets come
// schema-qualified ("public.sales_orders", "addon_<owner>.fiscal_documents").
// Only the qualifier is dropped; "*" and wildcards are left alone.
func bareTable(t string) string {
	t = strings.TrimPrefix(strings.TrimSpace(t), "public.")
	if strings.HasPrefix(t, "addon_") {
		if i := strings.Index(t, "."); i > 0 {
			t = t[i+1:]
		}
	}
	return strings.ToLower(t)
}

func ownWriteTarget(f RoleFacts, t string) bool {
	t = strings.TrimPrefix(strings.TrimSpace(t), "public.")
	// Only the dot form: "addon_<key>_" would let "payments_x" write the
	// namespace of an unrelated addon "payments_x_point".
	if t == "addon_"+f.Key+".*" || strings.HasPrefix(t, "addon_"+f.Key+".") {
		return true
	}
	for _, tb := range f.OwnTables {
		if strings.EqualFold(t, tb) {
			return true
		}
	}
	return false
}

// ContractWritableTables resolves, for every contract a manifest owns, the
// tables of the models named in writable_models (what RoleCatalog.WritableTables
// serves). A name that is not a model of the manifest is skipped (CheckRoles
// reports it as an error on the owner).
func ContractWritableTables(m *Manifest) map[string][]string {
	tableOf := map[string]string{}
	for _, md := range m.Models {
		tableOf[md.Key] = md.Table
	}
	out := map[string][]string{}
	for _, c := range m.Contracts {
		tables := []string{}
		for _, wm := range c.WritableModels {
			if t, ok := tableOf[wm]; ok {
				tables = append(tables, t)
			}
		}
		out[c.Key] = tables
	}
	return out
}

// nilCatalog reports whether cat is nil or an interface holding a nil pointer
// (a typed nil), which a host may pass by accident; both mean "no catalog".
func nilCatalog(cat RoleCatalog) bool { return isNilValue(cat) }

// isNilValue reports whether v is nil or an interface holding a nil pointer.
func isNilValue(v interface{}) bool {
	if v == nil {
		return true
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Ptr, reflect.Map, reflect.Slice, reflect.Func, reflect.Interface, reflect.Chan:
		return rv.IsNil()
	}
	return false
}

func writableByContract(f RoleFacts, cat RoleCatalog, target string) bool {
	for _, im := range f.Implements {
		ms, ok := cat.WritableTables(im.Contract)
		if !ok {
			continue
		}
		// Both sides are compared as bare table names: real db:write targets and
		// catalog entries may be schema-qualified ("public.x", "addon_<owner>.x").
		bt := bareTable(target)
		for _, m := range ms {
			if bareTable(m) == bt {
				return true
			}
		}
	}
	return false
}

// contractKeyRe mirrors the schema pattern of Contract.key, so the Go validator
// (and the legacy one, which never sees the schema) enforce the same shape.
var contractKeyRe = regexp.MustCompile(`^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)+$`)

func checkContracts(f RoleFacts) []string {
	var errs []string
	own := map[string]bool{}
	for _, k := range f.OwnModelKeys {
		own[k] = true
	}
	seen := map[string]bool{}
	for i, c := range f.Contracts {
		w := fmt.Sprintf("contracts[%d] (%s)", i, c.Key)
		if seen[c.Key] {
			errs = append(errs, w+": duplicate contract key")
		}
		seen[c.Key] = true
		if !contractKeyRe.MatchString(c.Key) {
			errs = append(errs, fmt.Sprintf("%s.key must be \"<family>.<name>\" in lower snake case", w))
		}
		if _, err := semver.NewVersion(c.Version); err != nil {
			errs = append(errs, fmt.Sprintf("%s.version %q is not semver: %v", w, c.Version, err))
		}
		ops := map[string]bool{}
		for j, op := range c.Operations {
			if ops[op.Key] {
				errs = append(errs, fmt.Sprintf("%s.operations[%d]: duplicate operation %q", w, j, op.Key))
			}
			ops[op.Key] = true
			if op.Kind == "query" && op.Idempotent {
				errs = append(errs, fmt.Sprintf("%s.operations[%d] (%s): a query is idempotent by nature; drop idempotent", w, j, op.Key))
			}
		}
		evs := map[string]bool{}
		for j, e := range c.Events {
			k := fmt.Sprintf("%s/%d", e.Name, e.Version)
			if evs[k] {
				errs = append(errs, fmt.Sprintf("%s.events[%d]: duplicate event %s", w, j, k))
			}
			evs[k] = true
		}
		if c.States != nil && !own[c.States.Model] {
			errs = append(errs, fmt.Sprintf("%s.states.model %q is not a model of this addon", w, c.States.Model))
		}
		for j, m := range c.WritableModels {
			if !own[m] {
				errs = append(errs, fmt.Sprintf("%s.writable_models[%d] %q is not a model of this addon", w, j, m))
			}
		}
		if c.Health != nil && !ops[c.Health.Operation] {
			errs = append(errs, fmt.Sprintf("%s.health.operation %q is not an operation of the contract", w, c.Health.Operation))
		}
		for j, s := range c.ProviderSettings {
			if s.Type == "enum" && len(s.Options) == 0 {
				errs = append(errs, fmt.Sprintf("%s.provider_settings[%d] (%s): an enum needs options", w, j, s.Key))
			}
		}
	}
	return errs
}

func checkImplements(f RoleFacts, cat RoleCatalog, declared bool, errs, warns []string) ([]string, []string) {
	seen := map[string]bool{}
	for i, im := range f.Implements {
		w := fmt.Sprintf("implements[%d] (%s)", i, im.Contract)
		if seen[im.Contract] {
			errs = append(errs, w+": contract implemented twice")
		}
		seen[im.Contract] = true
		if _, err := semver.NewConstraint(im.Version); err != nil {
			errs = append(errs, fmt.Sprintf("%s.version %q is not a valid semver range: %v", w, im.Version, err))
		}
		if cat != nil {
			if _, ok := cat.WritableTables(im.Contract); !ok {
				errs = append(errs, fmt.Sprintf("%s: unknown contract (not in the catalog)", w))
			}
		}
		// Version compatibility (X-R2): the implemented contract's version must
		// satisfy the range. The version comes from the catalog (optional
		// ContractVersioned) or the host's registry of declared contracts; when
		// neither knows the contract nothing is checked.
		if v, ok := contractVersion(im.Contract, cat); ok {
			if sat, err := capability.Satisfies(v, im.Version); err == nil && !sat {
				msg := fmt.Sprintf("%s.version %q is not satisfied by contract version %s", w, im.Version, v)
				if declared {
					errs = append(errs, msg)
				} else {
					warns = append(warns, fmt.Sprintf("[%s] %s", RuleRoleInferred, msg))
				}
			}
		}
	}
	return errs, warns
}

// contractVersion finds the version of an implemented contract: the catalog
// first (optional ContractVersioned), then the process registry of contracts
// declared by installed addons.
func contractVersion(key string, cat RoleCatalog) (string, bool) {
	if cv, ok := cat.(ContractVersioned); ok && !isNilValue(cat) {
		if v, found := cv.ContractVersion(key); found {
			return v, true
		}
	}
	if d, _, ok := capability.LookupDeclared(key); ok {
		return d.Version, true
	}
	return "", false
}
