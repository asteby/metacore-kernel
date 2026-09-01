package v3

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/Masterminds/semver/v3"
	"github.com/asteby/metacore-kernel/manifest/computeexpr"
	"github.com/asteby/metacore-kernel/manifest/rulesexpr"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

// validFns is the rollup aggregate-function allowlist, shared with the legacy
// validator via the schema enum. count ignores from/expr.
var validFns = map[string]struct{}{
	"sum": {}, "count": {}, "avg": {}, "min": {}, "max": {},
}

// validateArithExpr parses a Tier-2 formula / Tier-1 rollup expression under
// the strict arithmetic allowlist and verifies every identifier is a column in
// `cols`. Returns nil on success.
func validateArithExpr(expr string, cols map[string]struct{}) error {
	return computeexpr.Validate(expr, cols)
}

// seqPlaceholderRe matches a {seq} or {seq:0N} folio placeholder. Mirrors
// dynamic.seqPlaceholderRe so validation and rendering agree on the grammar.
var seqPlaceholderRe = regexp.MustCompile(`\{seq(?::0(\d+))?\}`)

// validateSequenceFormat enforces that a folio Format is non-empty and carries
// exactly one {seq}/{seq:0N} placeholder (zero placeholders would make every
// folio identical; more than one is almost certainly an authoring mistake).
func validateSequenceFormat(format string) error {
	if strings.TrimSpace(format) == "" {
		return fmt.Errorf("is empty")
	}
	n := len(seqPlaceholderRe.FindAllString(format, -1))
	if n != 1 {
		return fmt.Errorf("must contain exactly one {seq} or {seq:0N} placeholder (found %d)", n)
	}
	return nil
}

// constraintComparisonOps are the operators a declarative guard predicate may
// use, longest-match first so ">=" is not mis-split as ">". Mirrors
// dynamic.constraintOps (the runtime evaluator) so validation and evaluation
// agree on the grammar.
var constraintComparisonOps = []string{">=", "<=", "!=", "==", ">", "<", "="}

// validateConstraintExpr checks a guard predicate `<arith> <op> <arith>`: it
// must carry exactly one comparison operator, and BOTH sides must pass the
// strict arithmetic allowlist against `cols`. This keeps the manifest planes in
// lock-step with the runtime evaluator (dynamic.evalConstraintExpr).
func validateConstraintExpr(expr string, cols map[string]struct{}) error {
	op, idx := "", -1
	for i := 0; i < len(expr) && op == ""; i++ {
		c := expr[i]
		if c != '>' && c != '<' && c != '=' && c != '!' {
			continue
		}
		for _, o := range constraintComparisonOps {
			if strings.HasPrefix(expr[i:], o) {
				op, idx = o, i
				break
			}
		}
	}
	if op == "" {
		return fmt.Errorf("must contain a comparison operator (>= <= > < == !=)")
	}
	lhs := strings.TrimSpace(expr[:idx])
	rhs := strings.TrimSpace(expr[idx+len(op):])
	if lhs == "" || rhs == "" {
		return fmt.Errorf("comparison is missing an operand")
	}
	if err := validateArithExpr(lhs, cols); err != nil {
		return fmt.Errorf("left side %q: %v", lhs, err)
	}
	if err := validateArithExpr(rhs, cols); err != nil {
		return fmt.Errorf("right side %q: %v", rhs, err)
	}
	return nil
}

// validateConstraintApproval enforces the approval contract of one column
// guard: on_violation is ""|"reject"|"request_approval"; request_approval
// REQUIRES an approval block (the kernel must know who may approve); an
// approval block without request_approval is an authoring mistake (it would
// silently be ignored); and `when` is an action-only knob.
func validateConstraintApproval(where string, con Constraint) []string {
	var errs []string
	switch con.OnViolation {
	case "", "reject":
		if con.Approval != nil {
			errs = append(errs, fmt.Sprintf("%s.approval is set but on_violation is not \"request_approval\"", where))
		}
	case "request_approval":
		if con.Approval == nil {
			errs = append(errs, fmt.Sprintf("%s.on_violation=request_approval requires an approval block (roles)", where))
		}
	default:
		errs = append(errs, fmt.Sprintf("%s.on_violation %q is not one of \"reject\"|\"request_approval\"", where, con.OnViolation))
	}
	if con.Approval != nil {
		errs = append(errs, validateApprovalPolicy(where+".approval", con.Approval)...)
		if strings.TrimSpace(con.Approval.When) != "" {
			errs = append(errs, fmt.Sprintf("%s.approval.when is only valid on contributions.actions[].approval", where))
		}
	}
	return errs
}

// validateApprovalPolicy checks the role list (non-empty, no blank entries)
// and the expiry of an approval block. Shared by column guards and actions.
func validateApprovalPolicy(where string, p *ApprovalPolicy) []string {
	var errs []string
	if p == nil {
		return nil
	}
	if len(p.Roles) == 0 {
		errs = append(errs, fmt.Sprintf("%s.roles is empty (declare at least one approver role)", where))
	}
	for i, r := range p.Roles {
		if strings.TrimSpace(r) == "" {
			errs = append(errs, fmt.Sprintf("%s.roles[%d] is empty", where, i))
		}
	}
	if p.ExpiresHours < 0 {
		errs = append(errs, fmt.Sprintf("%s.expires_hours must be >= 0", where))
	}
	return errs
}

// validateRollupSpec checks one Tier-1 rollup: target on the PARENT (ownCols),
// fn in the enum, from on the CHILD (childCols), exactly one of from/expr
// (count may omit both), expr under the arithmetic allowlist against childCols.
// Returns a slice of human-readable errors prefixed with `where`.
func validateRollupSpec(where string, r Rollup, ownCols, childCols map[string]struct{}) []string {
	var errs []string
	if r.Target == "" {
		errs = append(errs, fmt.Sprintf("%s.target is empty", where))
	} else if _, ok := ownCols[r.Target]; !ok {
		errs = append(errs, fmt.Sprintf("%s.target %q is not a declared column on the parent model", where, r.Target))
	}
	fn := strings.ToLower(strings.TrimSpace(r.Fn))
	if fn == "" {
		fn = "sum"
	}
	if _, ok := validFns[fn]; !ok {
		errs = append(errs, fmt.Sprintf("%s.fn %q is not one of sum|count|avg|min|max", where, r.Fn))
	}
	hasFrom := strings.TrimSpace(r.From) != ""
	hasExpr := strings.TrimSpace(r.Expr) != ""
	if hasFrom && hasExpr {
		errs = append(errs, fmt.Sprintf("%s declares both from and expr (use exactly one)", where))
	}
	if fn != "count" && !hasFrom && !hasExpr {
		errs = append(errs, fmt.Sprintf("%s.fn=%s requires either from or expr", where, fn))
	}
	if hasFrom {
		if !computeexpr.IdentRe.MatchString(r.From) {
			errs = append(errs, fmt.Sprintf("%s.from %q is not a valid column identifier", where, r.From))
		} else if _, ok := childCols[r.From]; !ok {
			errs = append(errs, fmt.Sprintf("%s.from %q is not a declared column on the child model", where, r.From))
		}
	}
	if hasExpr {
		if err := validateArithExpr(r.Expr, childCols); err != nil {
			errs = append(errs, fmt.Sprintf("%s.expr %q: %v", where, r.Expr, err))
		}
	}
	return errs
}

// Dashboard widget enums, shared with the JSON schema so the struct-level
// checks and the schema agree byte-for-byte (the "dual validation" contract).
var (
	dashKinds = map[string]struct{}{
		"stat": {}, "bar": {}, "line": {}, "area": {}, "pie": {},
		"donut": {}, "list": {}, "progress": {}, "custom": {},
	}
	dashAggregates = map[string]struct{}{
		"count": {}, "sum": {}, "avg": {}, "min": {}, "max": {},
	}
	dashSizes   = map[string]struct{}{"sm": {}, "md": {}, "lg": {}, "full": {}}
	dashFormats = map[string]struct{}{
		"number": {}, "currency": {}, "percent": {}, "compact": {},
	}
)

// validateDashboard enforces the §1 cross-field rules of the dashboard-widget
// contract that the JSON schema cannot express (kind-conditional requirements).
// Each error is prefixed with the offending widget's key so an author can find
// it immediately. Returns a slice (possibly empty) so the caller can append.
func validateDashboard(m *Manifest) []string {
	if m.Contributions == nil || len(m.Contributions.Dashboard) == 0 {
		return nil
	}
	var errs []string
	seen := make(map[string]struct{}, len(m.Contributions.Dashboard))
	for i, w := range m.Contributions.Dashboard {
		id := w.Key
		if id == "" {
			id = fmt.Sprintf("#%d", i)
		}
		where := fmt.Sprintf("contributions.dashboard[%s]", id)

		if w.Key == "" {
			errs = append(errs, fmt.Sprintf("%s.key is required", where))
		} else if _, dup := seen[w.Key]; dup {
			errs = append(errs, fmt.Sprintf("%s.key %q is duplicated within the addon", where, w.Key))
		} else {
			seen[w.Key] = struct{}{}
		}
		if w.Title == "" {
			errs = append(errs, fmt.Sprintf("%s.title is required", where))
		}
		if w.Kind == "" {
			errs = append(errs, fmt.Sprintf("%s.kind is required", where))
			// Without a kind the conditional rules below are meaningless.
			continue
		}
		if _, ok := dashKinds[w.Kind]; !ok {
			errs = append(errs, fmt.Sprintf("%s.kind %q is not one of stat|bar|line|area|pie|donut|list|progress|custom", where, w.Kind))
		}
		if w.Size != "" {
			if _, ok := dashSizes[w.Size]; !ok {
				errs = append(errs, fmt.Sprintf("%s.size %q is not one of sm|md|lg|full", where, w.Size))
			}
		}
		if w.Format != "" {
			if _, ok := dashFormats[w.Format]; !ok {
				errs = append(errs, fmt.Sprintf("%s.format %q is not one of number|currency|percent|compact", where, w.Format))
			}
		}

		if w.Kind == "custom" {
			// Federated escape hatch: needs an expose + a frontend bundle.
			if w.Expose == "" {
				errs = append(errs, fmt.Sprintf("%s is kind=custom and must declare expose", where))
			}
			if m.Frontend == nil {
				errs = append(errs, fmt.Sprintf("%s is kind=custom and requires a top-level frontend block (federation)", where))
			}
			continue
		}

		// Declarative kinds: a query (with a model) is mandatory.
		if w.Query == nil {
			errs = append(errs, fmt.Sprintf("%s.query is required for kind=%s", where, w.Kind))
			continue
		}
		q := w.Query
		if q.Model == "" {
			errs = append(errs, fmt.Sprintf("%s.query.model is required", where))
		}
		agg := q.Aggregate
		if agg == "" {
			agg = "count"
		}
		if _, ok := dashAggregates[agg]; !ok {
			errs = append(errs, fmt.Sprintf("%s.query.aggregate %q is not one of count|sum|avg|min|max", where, q.Aggregate))
		}
		if agg != "count" && q.Field == "" {
			errs = append(errs, fmt.Sprintf("%s.query.field is required when aggregate=%s", where, agg))
		}
		switch w.Kind {
		case "bar", "pie", "donut", "list":
			if q.GroupBy == "" {
				errs = append(errs, fmt.Sprintf("%s.query.group_by is required for kind=%s", where, w.Kind))
			}
		case "line", "area":
			if q.DateField == "" {
				errs = append(errs, fmt.Sprintf("%s.query.date_field is required for kind=%s", where, w.Kind))
			}
			if q.Interval == "" {
				errs = append(errs, fmt.Sprintf("%s.query.interval is required for kind=%s", where, w.Kind))
			}
		}
		if w.Compare != nil && (q.DateField == "" || q.Range == "") {
			errs = append(errs, fmt.Sprintf("%s.compare requires query.date_field and query.range", where))
		}
	}
	return errs
}

// docPapers is the page-geometry enum for printable documents, shared with the
// JSON schema so the struct-level checks and the schema agree byte-for-byte
// (the "dual validation" contract).
var docPapers = map[string]struct{}{"A4": {}, "letter": {}, "ticket80": {}}

// validateDocuments enforces the cross-field rules of the printable-document
// contract (contributions.documents[]) the JSON schema cannot express: unique
// keys, a model that exists among the addon's own models or its model
// extensions, a paper size in the enum, and a non-empty .html template path.
// colsByModel is the manifest's own-model index (keyed by model key); model
// extensions target models owned by OTHER addons, so their target names are
// added to the resolvable set. Returns a slice (possibly empty) so the caller
// can append.
func validateDocuments(m *Manifest, colsByModel map[string]map[string]struct{}) []string {
	if m.Contributions == nil || len(m.Contributions.Documents) == 0 {
		return nil
	}
	// Models the addon may bind a document to: its own models plus the models
	// it extends (columns attached to another addon's model).
	known := make(map[string]struct{}, len(colsByModel))
	for k := range colsByModel {
		known[k] = struct{}{}
	}
	for _, mod := range m.Models {
		for _, ext := range mod.Extensions {
			if ext.TargetModel != "" {
				known[ext.TargetModel] = struct{}{}
			}
		}
	}
	var errs []string
	seen := make(map[string]struct{}, len(m.Contributions.Documents))
	for i, d := range m.Contributions.Documents {
		id := d.Key
		if id == "" {
			id = fmt.Sprintf("#%d", i)
		}
		where := fmt.Sprintf("contributions.documents[%s]", id)

		if d.Key == "" {
			errs = append(errs, fmt.Sprintf("%s.key is required", where))
		} else if _, dup := seen[d.Key]; dup {
			errs = append(errs, fmt.Sprintf("%s.key %q is duplicated within the addon", where, d.Key))
		} else {
			seen[d.Key] = struct{}{}
		}
		if d.Model == "" {
			errs = append(errs, fmt.Sprintf("%s.model is required", where))
		} else if _, ok := known[d.Model]; !ok {
			errs = append(errs, fmt.Sprintf("%s.model %q is not a model of this addon (nor a model it extends)", where, d.Model))
		}
		if d.Template == "" {
			errs = append(errs, fmt.Sprintf("%s.template is required", where))
		} else if !strings.HasSuffix(d.Template, ".html") {
			errs = append(errs, fmt.Sprintf("%s.template %q must be a bundle-relative .html path", where, d.Template))
		}
		if d.Paper == "" {
			errs = append(errs, fmt.Sprintf("%s.paper is required", where))
		} else if _, ok := docPapers[d.Paper]; !ok {
			errs = append(errs, fmt.Sprintf("%s.paper %q is not one of A4|letter|ticket80", where, d.Paper))
		}
		if d.Condition != nil && strings.TrimSpace(d.Condition.Field) == "" {
			errs = append(errs, fmt.Sprintf("%s.condition.field is required", where))
		}
	}
	return errs
}

// publicRouteKinds is the rendering enum for public routes, shared with the
// JSON schema (dual validation).
var publicRouteKinds = map[string]struct{}{"document": {}, "json": {}, "html": {}}

// isTextColumnType reports whether a v3 column type can hold a public-route
// token (text or a bounded varchar).
func isTextColumnType(t string) bool {
	t = strings.ToLower(strings.TrimSpace(t))
	return t == "text" || strings.HasPrefix(t, "varchar(")
}

// isTemporalColumnType reports whether a v3 column type can carry a
// public-route expiry (date / timestamp / timestamptz).
func isTemporalColumnType(t string) bool {
	switch strings.ToLower(strings.TrimSpace(t)) {
	case "date", "timestamp", "timestamptz":
		return true
	}
	return false
}

var ruleThenKinds = map[string]struct{}{"flag": {}, "notify": {}}
var ruleSeverities = map[string]struct{}{"info": {}, "warning": {}, "error": {}}
var ruleEventTypes = map[string]struct{}{"created": {}, "updated": {}, "transitioned": {}}

// validateRules enforces contributions.rules[]: unique keys; a model this
// addon owns or extends; a `when` that parses under rulesexpr AND whose
// identifiers all resolve to columns of Model (column checks skipped for
// EXTENDED models, same reasoning as validatePublicRoutes — the owning
// addon's manifest is not in scope here); then.kind in the enum; a message;
// notify_roles required (non-empty) when kind is notify; event_types values
// in the enum. Returns a slice (possibly empty) so the caller can append.
func validateRules(m *Manifest, colsByModel map[string]map[string]struct{}) []string {
	if m.Contributions == nil || len(m.Contributions.Rules) == 0 {
		return nil
	}
	extended := map[string]struct{}{}
	for _, mod := range m.Models {
		for _, ext := range mod.Extensions {
			if ext.TargetModel != "" {
				extended[ext.TargetModel] = struct{}{}
			}
		}
	}

	var errs []string
	seen := make(map[string]struct{}, len(m.Contributions.Rules))
	for i, r := range m.Contributions.Rules {
		id := r.Key
		if id == "" {
			id = fmt.Sprintf("#%d", i)
		}
		where := fmt.Sprintf("contributions.rules[%s]", id)

		if r.Key == "" {
			errs = append(errs, fmt.Sprintf("%s.key is required", where))
		} else if _, dup := seen[r.Key]; dup {
			errs = append(errs, fmt.Sprintf("%s.key %q is duplicated within the addon", where, r.Key))
		} else {
			seen[r.Key] = struct{}{}
		}

		cols, isOwn := colsByModel[r.Model]
		_, isExt := extended[r.Model]
		switch {
		case r.Model == "":
			errs = append(errs, fmt.Sprintf("%s.model is required", where))
		case !isOwn && !isExt:
			errs = append(errs, fmt.Sprintf("%s.model %q is not a model of this addon (nor a model it extends)", where, r.Model))
		}

		if r.When == "" {
			errs = append(errs, fmt.Sprintf("%s.when is required", where))
		} else if parsed, err := rulesexpr.Parse(r.When); err != nil {
			errs = append(errs, fmt.Sprintf("%s.when %q: %v", where, r.When, err))
		} else if isOwn {
			for _, f := range parsed.Fields() {
				if _, ok := cols[f]; !ok {
					errs = append(errs, fmt.Sprintf("%s.when references unknown column %q of model %q", where, f, r.Model))
				}
			}
		}

		for _, et := range r.EventTypes {
			if _, ok := ruleEventTypes[et]; !ok {
				errs = append(errs, fmt.Sprintf("%s.event_types contains %q, want one of created|updated|transitioned", where, et))
			}
		}

		if r.Then.Kind == "" {
			errs = append(errs, fmt.Sprintf("%s.then.kind is required", where))
		} else if _, ok := ruleThenKinds[r.Then.Kind]; !ok {
			errs = append(errs, fmt.Sprintf("%s.then.kind %q is not one of flag|notify", where, r.Then.Kind))
		} else if r.Then.Kind == "notify" && len(r.Then.NotifyRoles) == 0 {
			errs = append(errs, fmt.Sprintf("%s.then.notify_roles is required when then.kind is notify", where))
		}
		if r.Then.Severity != "" {
			if _, ok := ruleSeverities[r.Then.Severity]; !ok {
				errs = append(errs, fmt.Sprintf("%s.then.severity %q is not one of info|warning|error", where, r.Then.Severity))
			}
		}
		if strings.TrimSpace(r.Then.Message) == "" {
			errs = append(errs, fmt.Sprintf("%s.then.message is required", where))
		}
	}
	return errs
}

// validatePublicRoutes enforces the cross-field rules of the public-route
// contract (contributions.public_routes[]) the JSON schema cannot express:
// unique keys; a model the addon owns or extends; a token column that exists
// on an OWN model and is text-typed; a kind in the enum; a document that
// exists in contributions.documents[] and binds the same model (required for
// kind document); columns/relations/expires_column/enabled_when fields that
// resolve on the model; the token column never listed in columns. Column-level
// checks are skipped for EXTENDED models (their columns live in another
// addon's manifest) — the host re-checks at serve time. Returns a slice
// (possibly empty) so the caller can append.
func validatePublicRoutes(m *Manifest) []string {
	if m.Contributions == nil || len(m.Contributions.PublicRoutes) == 0 {
		return nil
	}
	own := make(map[string]map[string]string, len(m.Models)) // model → column → type
	rels := make(map[string]map[string]struct{}, len(m.Models))
	extended := map[string]struct{}{}
	for _, mod := range m.Models {
		cols := make(map[string]string, len(mod.Columns))
		for _, c := range mod.Columns {
			cols[c.Name] = c.Type
		}
		own[mod.Key] = cols
		rs := make(map[string]struct{}, len(mod.Relations))
		for _, r := range mod.Relations {
			rs[r.Name] = struct{}{}
		}
		rels[mod.Key] = rs
		for _, ext := range mod.Extensions {
			if ext.TargetModel != "" {
				extended[ext.TargetModel] = struct{}{}
			}
		}
	}
	docs := make(map[string]string, len(m.Contributions.Documents)) // key → model
	for _, d := range m.Contributions.Documents {
		docs[d.Key] = d.Model
	}

	var errs []string
	seen := make(map[string]struct{}, len(m.Contributions.PublicRoutes))
	for i, r := range m.Contributions.PublicRoutes {
		id := r.Key
		if id == "" {
			id = fmt.Sprintf("#%d", i)
		}
		where := fmt.Sprintf("contributions.public_routes[%s]", id)

		if r.Key == "" {
			errs = append(errs, fmt.Sprintf("%s.key is required", where))
		} else if _, dup := seen[r.Key]; dup {
			errs = append(errs, fmt.Sprintf("%s.key %q is duplicated within the addon", where, r.Key))
		} else {
			seen[r.Key] = struct{}{}
		}

		cols, isOwn := own[r.Model]
		_, isExt := extended[r.Model]
		switch {
		case r.Model == "":
			errs = append(errs, fmt.Sprintf("%s.model is required", where))
		case !isOwn && !isExt:
			errs = append(errs, fmt.Sprintf("%s.model %q is not a model of this addon (nor a model it extends)", where, r.Model))
		}

		if r.TokenColumn == "" {
			errs = append(errs, fmt.Sprintf("%s.token_column is required", where))
		} else if isOwn {
			if typ, ok := cols[r.TokenColumn]; !ok {
				errs = append(errs, fmt.Sprintf("%s.token_column %q is not a column of model %q", where, r.TokenColumn, r.Model))
			} else if !isTextColumnType(typ) {
				errs = append(errs, fmt.Sprintf("%s.token_column %q must be a text column (got %q)", where, r.TokenColumn, typ))
			}
		}

		if r.Kind == "" {
			errs = append(errs, fmt.Sprintf("%s.kind is required", where))
		} else if _, ok := publicRouteKinds[r.Kind]; !ok {
			errs = append(errs, fmt.Sprintf("%s.kind %q is not one of document|json|html", where, r.Kind))
		}

		if r.Document == "" {
			if r.Kind == "document" {
				errs = append(errs, fmt.Sprintf("%s.document is required when kind is document", where))
			}
		} else if docModel, ok := docs[r.Document]; !ok {
			errs = append(errs, fmt.Sprintf("%s.document %q is not a contributions.documents[] key of this addon", where, r.Document))
		} else if docModel != r.Model {
			errs = append(errs, fmt.Sprintf("%s.document %q binds model %q, not %q", where, r.Document, docModel, r.Model))
		}

		switch r.Kind {
		case "json":
			if len(r.Columns) == 0 {
				errs = append(errs, fmt.Sprintf("%s.columns must list at least one column for kind json", where))
			}
		case "html":
			if len(r.Columns) == 0 && r.Document == "" {
				errs = append(errs, fmt.Sprintf("%s.columns must list at least one column for kind html (or set document)", where))
			}
		}
		seenCols := make(map[string]struct{}, len(r.Columns))
		for _, c := range r.Columns {
			if c == "" {
				errs = append(errs, fmt.Sprintf("%s.columns contains an empty name", where))
				continue
			}
			if _, dup := seenCols[c]; dup {
				errs = append(errs, fmt.Sprintf("%s.columns lists %q twice", where, c))
				continue
			}
			seenCols[c] = struct{}{}
			if r.TokenColumn != "" && c == r.TokenColumn {
				errs = append(errs, fmt.Sprintf("%s.columns must not expose the token column %q", where, c))
				continue
			}
			if isOwn {
				if _, ok := cols[c]; !ok {
					errs = append(errs, fmt.Sprintf("%s.columns[%s] is not a column of model %q", where, c, r.Model))
				}
			}
		}
		if isOwn {
			for _, rel := range r.Relations {
				if rel == "" {
					errs = append(errs, fmt.Sprintf("%s.relations contains an empty name", where))
					continue
				}
				if _, ok := rels[r.Model][rel]; ok {
					continue
				}
				// A ref column (customer_id) resolves to a sibling named after
				// its stem (customer); accept either spelling.
				if _, ok := cols[rel]; ok {
					continue
				}
				if _, ok := cols[rel+"_id"]; ok {
					continue
				}
				errs = append(errs, fmt.Sprintf("%s.relations[%s] is neither a relation nor a ref column of model %q", where, rel, r.Model))
			}
		}
		if r.ExpiresColumn != "" && isOwn {
			if typ, ok := cols[r.ExpiresColumn]; !ok {
				errs = append(errs, fmt.Sprintf("%s.expires_column %q is not a column of model %q", where, r.ExpiresColumn, r.Model))
			} else if !isTemporalColumnType(typ) {
				errs = append(errs, fmt.Sprintf("%s.expires_column %q must be a date/timestamp column (got %q)", where, r.ExpiresColumn, typ))
			}
		}
		if strings.TrimSpace(r.EnabledWhen) != "" {
			expr, err := ParseRecordExpr(r.EnabledWhen)
			if err != nil {
				errs = append(errs, fmt.Sprintf("%s.enabled_when: %v", where, err))
			} else if isOwn {
				for _, f := range expr.Fields() {
					if _, ok := cols[f]; !ok {
						errs = append(errs, fmt.Sprintf("%s.enabled_when references %q, not a column of model %q", where, f, r.Model))
					}
				}
			}
		}
	}
	return errs
}

// validHookPrefixes is the allowlist of TransitionHook.Do dispatch targets,
// shared with the JSON schema pattern so the struct-level checks and the schema
// agree (the "dual validation" contract).
var validHookPrefixes = map[string]struct{}{
	"wasm": {}, "webhook": {}, "compiled": {},
}

// validateStageMachine enforces the cross-field invariants of a model's stage
// machine that the JSON schema cannot express: stage_field names a real column,
// stage keys are unique, transitions reference declared stages, and each
// on_transition hook matches declared stages (or "*") and carries a valid
// wasm:/webhook:/compiled: `do`. Returns a slice (possibly empty) prefixed with
// the offending model's index so the caller can append. Empty stages is a
// no-op (the model has no stage machine).
func validateStageMachine(mi int, mod Model, ownCols map[string]struct{}) []string {
	if len(mod.Stages) == 0 && mod.StageField == "" && len(mod.Transitions) == 0 && len(mod.OnTransition) == 0 {
		return nil
	}
	var errs []string
	where := fmt.Sprintf("models[%d]", mi)

	// stage_field is mandatory once any stage-machine field is declared, and
	// must name a declared column.
	if mod.StageField == "" {
		errs = append(errs, fmt.Sprintf("%s declares stages/transitions/on_transition but no stage_field", where))
	} else if _, ok := ownCols[mod.StageField]; !ok {
		errs = append(errs, fmt.Sprintf("%s.stage_field %q is not a declared column on the model", where, mod.StageField))
	}

	// Stage keys: non-empty and unique.
	stageKeys := make(map[string]struct{}, len(mod.Stages))
	for si, st := range mod.Stages {
		if st.Key == "" {
			errs = append(errs, fmt.Sprintf("%s.stages[%d].key is empty", where, si))
			continue
		}
		if _, dup := stageKeys[st.Key]; dup {
			errs = append(errs, fmt.Sprintf("%s.stages[%d].key %q is duplicated", where, si, st.Key))
		}
		stageKeys[st.Key] = struct{}{}
	}
	if len(mod.Stages) == 0 && mod.StageField != "" {
		errs = append(errs, fmt.Sprintf("%s declares stage_field %q but no stages", where, mod.StageField))
	}

	// Transitions: from/to must reference declared stage keys.
	for ti, t := range mod.Transitions {
		if _, ok := stageKeys[t.From]; !ok {
			errs = append(errs, fmt.Sprintf("%s.transitions[%d].from %q is not a declared stage key", where, ti, t.From))
		}
		if _, ok := stageKeys[t.To]; !ok {
			errs = append(errs, fmt.Sprintf("%s.transitions[%d].to %q is not a declared stage key", where, ti, t.To))
		}
	}

	// Hooks: from/to must be "*" or a declared stage key. A hook must carry `set`,
	// `do`, or both. When `do` is present it must carry a known wasm:/webhook:/
	// compiled: prefix (the schema pattern enforces the shape, this enforces the
	// set). When `set` is present every key must name a declared column.
	for hi, h := range mod.OnTransition {
		if h.From != "*" && h.From != "" {
			if _, ok := stageKeys[h.From]; !ok {
				errs = append(errs, fmt.Sprintf("%s.on_transition[%d].from %q is not a declared stage key (or \"*\")", where, hi, h.From))
			}
		}
		if h.To != "*" {
			if _, ok := stageKeys[h.To]; !ok {
				errs = append(errs, fmt.Sprintf("%s.on_transition[%d].to %q is not a declared stage key (or \"*\")", where, hi, h.To))
			}
		}
		if h.Do == "" && len(h.Set) == 0 {
			errs = append(errs, fmt.Sprintf("%s.on_transition[%d] must declare `set`, `do`, or both", where, hi))
		}
		for col := range h.Set {
			if _, ok := ownCols[col]; !ok {
				errs = append(errs, fmt.Sprintf("%s.on_transition[%d].set key %q is not a declared column on the model", where, hi, col))
			}
		}
		if h.Do != "" {
			prefix, _, found := strings.Cut(h.Do, ":")
			if !found {
				errs = append(errs, fmt.Sprintf("%s.on_transition[%d].do %q must be wasm:<export> | webhook:<key> | compiled:<fn>", where, hi, h.Do))
			} else if _, ok := validHookPrefixes[prefix]; !ok {
				errs = append(errs, fmt.Sprintf("%s.on_transition[%d].do %q has an unknown prefix (want wasm|webhook|compiled)", where, hi, prefix))
			}
		}
	}
	return errs
}

// validateNavViewTypes enforces that a kanban nav entry declares the group_by
// column it groups its board by. view_type is otherwise free (hosts fall back
// to a table for an unknown value). Walks the nested NavItem tree.
func validateNavViewTypes(m *Manifest) []string {
	if m.Contributions == nil {
		return nil
	}
	var errs []string
	var walk func(items []NavItem, path string)
	walk = func(items []NavItem, path string) {
		for i, it := range items {
			where := fmt.Sprintf("%s[%d]", path, i)
			if it.ViewType == "kanban" && it.GroupBy == "" {
				errs = append(errs, fmt.Sprintf("%s declares view_type=kanban but no group_by", where))
			}
			if len(it.Items) > 0 {
				walk(it.Items, where+".items")
			}
		}
	}
	for gi, g := range m.Contributions.Navigation {
		walk(g.Items, fmt.Sprintf("contributions.navigation[%d].items", gi))
	}
	return errs
}

// validateDoRef checks a Schedule/InboundWebhook `do` carries a known
// wasm:/webhook:/compiled: dispatch prefix (the schema pattern enforces the
// shape; this enforces the set, mirroring validateStageMachine). Returns an
// error string suffix or "" when valid.
func validateDoRef(do string) string {
	prefix, _, found := strings.Cut(do, ":")
	if !found {
		return fmt.Sprintf("%q must be wasm:<export> | webhook:<key> | compiled:<fn>", do)
	}
	if _, ok := validHookPrefixes[prefix]; !ok {
		return fmt.Sprintf("%q has an unknown prefix (want wasm|webhook|compiled)", do)
	}
	return ""
}

// validatePipelineRuntime enforces the cross-field invariants of the addon-level
// pipeline-runtime primitives (connectors / schedules / webhooks / edge
// devices) that the JSON schema cannot express: connector keys are unique; a
// schedule's `every` parses as a Go duration and its `do` carries a known
// prefix; a webhook's `do` carries a known prefix and, when it declares a
// `verify`, it supplies a `secret_ref` that resolves to a declared connector
// credential; an edge device's `kind`/`transport` come from the closed v1
// sets and its events/commands carry non-empty, unique types (an event's `do`
// is validated exactly like a webhook's). Empty blocks are a no-op (the addon
// has no runtime primitives — the back-compat default).
func validatePipelineRuntime(m *Manifest) []string {
	if len(m.Connectors) == 0 && len(m.Schedules) == 0 && len(m.Webhooks) == 0 && len(m.EdgeDevices) == 0 {
		return nil
	}
	var errs []string

	// Connectors: unique keys; index their credential keys for secret_ref checks.
	connectorCreds := make(map[string]map[string]struct{}, len(m.Connectors))
	seenConn := make(map[string]struct{}, len(m.Connectors))
	for ci, c := range m.Connectors {
		if c.Key == "" {
			errs = append(errs, fmt.Sprintf("connectors[%d].key is empty", ci))
			continue
		}
		if _, dup := seenConn[c.Key]; dup {
			errs = append(errs, fmt.Sprintf("connectors[%d].key %q is duplicated", ci, c.Key))
		}
		seenConn[c.Key] = struct{}{}
		creds := make(map[string]struct{}, len(c.Credentials))
		for _, cr := range c.Credentials {
			creds[cr.Key] = struct{}{}
		}
		connectorCreds[c.Key] = creds
	}

	// Schedules: unique keys; every parses; do prefix known.
	seenSched := make(map[string]struct{}, len(m.Schedules))
	for si, s := range m.Schedules {
		if s.Key == "" {
			errs = append(errs, fmt.Sprintf("schedules[%d].key is empty", si))
		} else {
			if _, dup := seenSched[s.Key]; dup {
				errs = append(errs, fmt.Sprintf("schedules[%d].key %q is duplicated", si, s.Key))
			}
			seenSched[s.Key] = struct{}{}
		}
		if d, err := time.ParseDuration(s.Every); err != nil || d <= 0 {
			errs = append(errs, fmt.Sprintf("schedules[%d].every %q is not a positive Go duration (e.g. \"30s\", \"5m\")", si, s.Every))
		}
		if msg := validateDoRef(s.Do); msg != "" {
			errs = append(errs, fmt.Sprintf("schedules[%d].do %s", si, msg))
		}
	}

	// Webhooks: unique keys + paths; do prefix known; verify ⇒ secret_ref → a
	// declared connector credential.
	seenHook := make(map[string]struct{}, len(m.Webhooks))
	seenPath := make(map[string]struct{}, len(m.Webhooks))
	for wi, w := range m.Webhooks {
		if w.Key == "" {
			errs = append(errs, fmt.Sprintf("webhooks[%d].key is empty", wi))
		} else {
			if _, dup := seenHook[w.Key]; dup {
				errs = append(errs, fmt.Sprintf("webhooks[%d].key %q is duplicated", wi, w.Key))
			}
			seenHook[w.Key] = struct{}{}
		}
		if w.Path == "" {
			errs = append(errs, fmt.Sprintf("webhooks[%d].path is empty", wi))
		} else {
			if _, dup := seenPath[w.Path]; dup {
				errs = append(errs, fmt.Sprintf("webhooks[%d].path %q is duplicated", wi, w.Path))
			}
			seenPath[w.Path] = struct{}{}
		}
		if msg := validateDoRef(w.Do); msg != "" {
			errs = append(errs, fmt.Sprintf("webhooks[%d].do %s", wi, msg))
		}
		if w.Verify != "" {
			if w.SecretRef == "" {
				errs = append(errs, fmt.Sprintf("webhooks[%d] declares verify=%q but no secret_ref", wi, w.Verify))
			} else {
				conn, cred, ok := strings.Cut(w.SecretRef, ".")
				if !ok {
					errs = append(errs, fmt.Sprintf("webhooks[%d].secret_ref %q must be \"<connector>.<credential>\"", wi, w.SecretRef))
				} else if creds, ok := connectorCreds[conn]; !ok {
					errs = append(errs, fmt.Sprintf("webhooks[%d].secret_ref %q references undeclared connector %q", wi, w.SecretRef, conn))
				} else if _, ok := creds[cred]; !ok {
					errs = append(errs, fmt.Sprintf("webhooks[%d].secret_ref %q references undeclared credential %q on connector %q", wi, w.SecretRef, cred, conn))
				}
			}
		}
	}

	// EdgeDevices: unique keys; kind and transport from the closed v1 sets;
	// events/commands need a non-empty type, events need a valid `do` ref.
	seenDevice := make(map[string]struct{}, len(m.EdgeDevices))
	for di, d := range m.EdgeDevices {
		if d.Key == "" {
			errs = append(errs, fmt.Sprintf("edge_devices[%d].key is empty", di))
		} else {
			if _, dup := seenDevice[d.Key]; dup {
				errs = append(errs, fmt.Sprintf("edge_devices[%d].key %q is duplicated", di, d.Key))
			}
			seenDevice[d.Key] = struct{}{}
		}
		if _, ok := edgeDeviceKinds[d.Kind]; !ok {
			errs = append(errs, fmt.Sprintf("edge_devices[%d].kind %q is not one of cash_recycler|card_terminal|scale|fiscal_printer", di, d.Kind))
		}
		if _, ok := edgeDeviceTransports[d.Transport]; !ok {
			errs = append(errs, fmt.Sprintf("edge_devices[%d].transport %q is not one of: ws", di, d.Transport))
		}
		seenEvent := make(map[string]struct{}, len(d.Events))
		for ei, ev := range d.Events {
			if ev.Type == "" {
				errs = append(errs, fmt.Sprintf("edge_devices[%d].events[%d].type is empty", di, ei))
			} else {
				if _, dup := seenEvent[ev.Type]; dup {
					errs = append(errs, fmt.Sprintf("edge_devices[%d].events[%d].type %q is duplicated", di, ei, ev.Type))
				}
				seenEvent[ev.Type] = struct{}{}
			}
			if ev.Do == "" {
				errs = append(errs, fmt.Sprintf("edge_devices[%d].events[%d].do is empty", di, ei))
			} else if msg := validateDoRef(ev.Do); msg != "" {
				errs = append(errs, fmt.Sprintf("edge_devices[%d].events[%d].do %s", di, ei, msg))
			}
		}
		seenCommand := make(map[string]struct{}, len(d.Commands))
		for ci, cmd := range d.Commands {
			if cmd.Type == "" {
				errs = append(errs, fmt.Sprintf("edge_devices[%d].commands[%d].type is empty", di, ci))
			} else {
				if _, dup := seenCommand[cmd.Type]; dup {
					errs = append(errs, fmt.Sprintf("edge_devices[%d].commands[%d].type %q is duplicated", di, ci, cmd.Type))
				}
				seenCommand[cmd.Type] = struct{}{}
			}
		}
	}
	return errs
}

//go:embed schema/manifest-v3.schema.json
var schemaBytes []byte

var compiledSchema *jsonschema.Schema

func init() {
	c := jsonschema.NewCompiler()
	c.DefaultDraft(jsonschema.Draft2020)
	// jsonschema/v6 takes a PARSED document (any) for AddResource, not an
	// io.Reader — and its own UnmarshalJSON decodes numbers as json.Number so
	// integer/number keywords validate correctly.
	doc, err := jsonschema.UnmarshalJSON(strings.NewReader(string(schemaBytes)))
	if err != nil {
		panic(fmt.Errorf("v3: parse embedded schema: %w", err))
	}
	if err := c.AddResource("manifest-v3.schema.json", doc); err != nil {
		panic(fmt.Errorf("v3: embed schema add resource: %w", err))
	}
	s, err := c.Compile("manifest-v3.schema.json")
	if err != nil {
		panic(fmt.Errorf("v3: compile embedded schema: %w", err))
	}
	compiledSchema = s
}

// SchemaJSON returns the embedded JSON schema bytes. Useful for tools that
// want to surface the schema (CLI, docs site, IDE plugins).
func SchemaJSON() []byte {
	out := make([]byte, len(schemaBytes))
	copy(out, schemaBytes)
	return out
}

// Validate parses raw as a v3 manifest, runs it through the JSON schema and
// then enforces the cross-field invariants the schema cannot express:
//
//   - apiVersion must equal APIVersion
//   - every compatibility.requires[].version must be a valid semver range
//   - every lifecycle.upgrade[].from must be a valid semver range
//   - kind=Preset must not declare models or own lifecycle
//   - kind=Addon must not declare a preset block
//
// On failure the returned error wraps a list of all violations so authors
// get the full picture in a single round trip.
// validateOptionWhen enforces the static-option cascade guard contract: an
// option's `when` block must resolve a governing sibling field (its own `field`
// or the container column/field's `depends_on`) and must scope the value with a
// non-empty `in` or `not_in`. `when` only exists on the static array form, so no
// coexistence check against the dynamic object form is structurally possible.
func validateOptionWhen(where, containerDependsOn string, opts []FieldOption) []string {
	var errs []string
	for oi, o := range opts {
		if o.When == nil {
			continue
		}
		ow := fmt.Sprintf("%s.options[%d].when", where, oi)
		if o.When.Field == "" && containerDependsOn == "" {
			errs = append(errs, fmt.Sprintf("%s requires `field`, or the container's `depends_on`, to name the governing sibling field", ow))
		}
		if len(o.When.In) == 0 && len(o.When.NotIn) == 0 {
			errs = append(errs, fmt.Sprintf("%s requires a non-empty `in` or `not_in`", ow))
		}
	}
	return errs
}

func Validate(raw []byte) error {
	if len(raw) == 0 {
		return errors.New("v3: manifest is empty")
	}

	// Schema check first; if the doc is structurally broken the rest is
	// pointless.
	inst, err := jsonschema.UnmarshalJSON(strings.NewReader(string(raw)))
	if err != nil {
		return fmt.Errorf("v3: invalid JSON: %w", err)
	}
	if err := compiledSchema.Validate(inst); err != nil {
		return fmt.Errorf("v3: schema validation failed: %w", err)
	}

	// Decode into the typed shape for cross-field checks.
	var m Manifest
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return fmt.Errorf("v3: decode into typed manifest: %w", err)
	}

	var errs []string

	if m.APIVersion != APIVersion {
		errs = append(errs, fmt.Sprintf("apiVersion must be %q, got %q", APIVersion, m.APIVersion))
	}

	switch m.Kind {
	case KindAddon, KindPreset, KindTheme, KindConnectorPack:
		// known
	default:
		errs = append(errs, fmt.Sprintf("kind %q is not one of Addon|Preset|Theme|ConnectorPack", m.Kind))
	}

	for i, r := range m.Compatibility.Requires {
		if _, err := semver.NewConstraint(r.Version); err != nil {
			errs = append(errs, fmt.Sprintf("compatibility.requires[%d].version %q is not a valid semver range: %v", i, r.Version, err))
		}
		if r.Key == "" {
			errs = append(errs, fmt.Sprintf("compatibility.requires[%d].key is empty", i))
		}
	}

	if m.Lifecycle != nil {
		for i, step := range m.Lifecycle.Upgrade {
			if _, err := semver.NewConstraint(step.From); err != nil {
				errs = append(errs, fmt.Sprintf("lifecycle.upgrade[%d].from %q is not a valid semver range: %v", i, step.From, err))
			}
		}
	}

	// When metadata.icon.type is "svg", the slug is a path RELATIVE to an
	// asset bundled inside the addon. The kernel never sees the bundle, so it
	// cannot check the file exists or its size (the hub does that on publish);
	// here we only guarantee the path is clean and cannot escape the bundle.
	if m.Metadata.Icon != nil && m.Metadata.Icon.Type == "svg" {
		slug := m.Metadata.Icon.Slug
		switch {
		case strings.TrimSpace(slug) == "":
			errs = append(errs, "metadata.icon.slug is empty (type=svg requires a relative path to a bundled asset)")
		case len(slug) > 256:
			errs = append(errs, fmt.Sprintf("metadata.icon.slug is too long (%d chars, max 256)", len(slug)))
		case strings.HasPrefix(slug, "/"):
			errs = append(errs, fmt.Sprintf("metadata.icon.slug %q must be a relative path, not absolute", slug))
		case strings.Contains(slug, ".."), strings.Contains(path.Clean(slug), ".."), path.Clean(slug) != strings.TrimPrefix(slug, "./"):
			errs = append(errs, fmt.Sprintf("metadata.icon.slug %q must not contain path traversal (\"..\") and must be a clean relative path", slug))
		case !strings.HasSuffix(strings.ToLower(slug), ".svg"):
			errs = append(errs, fmt.Sprintf("metadata.icon.slug %q must reference an .svg asset", slug))
		}
	}

	switch m.Kind {
	case KindAddon:
		if m.Preset != nil {
			errs = append(errs, "kind=Addon must not declare a preset block")
		}
	case KindPreset:
		if m.Preset == nil {
			errs = append(errs, "kind=Preset requires a preset block")
		}
		if len(m.Models) > 0 {
			errs = append(errs, "kind=Preset must not declare models[]")
		}
		if m.Lifecycle != nil {
			errs = append(errs, "kind=Preset must not declare a lifecycle block")
		}
	case KindTheme:
		if m.Theme == nil {
			errs = append(errs, "kind=Theme requires a theme block")
		}
	case KindConnectorPack:
		if m.ConnectorPack == nil {
			errs = append(errs, "kind=ConnectorPack requires a connector_pack block")
		}
	}

	// Index every model's declared columns by model KEY so rollup/formula
	// cross-field checks can resolve targets/identifiers on the PARENT (the
	// model owning the relation) and on the CHILD (relation.Through). Built
	// once for the whole manifest.
	colsByModel := make(map[string]map[string]struct{}, len(m.Models))
	for _, mod := range m.Models {
		set := make(map[string]struct{}, len(mod.Columns))
		for _, c := range mod.Columns {
			set[c.Name] = struct{}{}
		}
		colsByModel[mod.Key] = set
	}

	// Shared tenancy (default): every owned model MUST declare the RLS column
	// (default organization_id). The schema already documents this; enforcing it
	// at Validate blocks hub publish of addons that would otherwise install with
	// OrgScoped=false and unscoped /api/data lists (cross-org leak).
	if m.Kind == KindAddon {
		iso := "shared"
		rls := "organization_id"
		if m.Tenancy != nil {
			if m.Tenancy.Isolation != "" {
				iso = m.Tenancy.Isolation
			}
			if m.Tenancy.RLSColumn != "" {
				rls = m.Tenancy.RLSColumn
			}
		}
		if iso == "shared" {
			for mi, mod := range m.Models {
				if _, ok := colsByModel[mod.Key][rls]; !ok {
					errs = append(errs, fmt.Sprintf(
						"models[%d] (%s): tenancy.isolation=shared requires column %q on every model (host scopes list/read/write by it; omitting it unscopes /api/data across organizations)",
						mi, mod.Key, rls))
				}
			}
		}
	}

	for mi, mod := range m.Models {
		ownCols := colsByModel[mod.Key]
		// Formulas: target must be a column on THIS model. Tier-2 (default)
		// exprs pass the strict arithmetic allowlist; Tier-3 swaps the expr for
		// a "wasm:<export>" handler.
		for fi, f := range mod.Formulas {
			where := fmt.Sprintf("models[%d].formulas[%d]", mi, fi)
			if f.Target == "" {
				errs = append(errs, fmt.Sprintf("%s.target is empty", where))
			} else if _, ok := ownCols[f.Target]; !ok {
				errs = append(errs, fmt.Sprintf("%s.target %q is not a declared column on the model", where, f.Target))
			}
			switch f.Tier {
			case 0, 2: // arithmetic Tier-2 (the default)
				if f.Handler != "" {
					errs = append(errs, fmt.Sprintf("%s.handler is only allowed when tier is 3", where))
				}
				if strings.TrimSpace(f.Expr) == "" {
					errs = append(errs, fmt.Sprintf("%s.expr is empty", where))
				} else if err := validateArithExpr(f.Expr, ownCols); err != nil {
					errs = append(errs, fmt.Sprintf("%s.expr %q: %v", where, f.Expr, err))
				}
			case 3: // wasm-backed Tier-3
				if strings.TrimSpace(f.Expr) != "" {
					errs = append(errs, fmt.Sprintf("%s.expr must be empty when tier is 3 (the handler replaces it)", where))
				}
				if !strings.HasPrefix(f.Handler, "wasm:") || len(f.Handler) <= len("wasm:") {
					errs = append(errs, fmt.Sprintf("%s.handler %q must be \"wasm:<export>\" when tier is 3", where, f.Handler))
				}
			default:
				errs = append(errs, fmt.Sprintf("%s.tier %d is not one of 2|3", where, f.Tier))
			}
		}
		if mod.Seed != nil {
			where := fmt.Sprintf("models[%d].seed", mi)
			if mod.Seed.Key == "" {
				errs = append(errs, fmt.Sprintf("%s.key is empty", where))
			} else {
				known := false
				for _, c := range mod.Columns {
					if c.Name == mod.Seed.Key {
						known = true
						break
					}
				}
				if !known {
					errs = append(errs, fmt.Sprintf("%s.key %q is not a declared column on the model", where, mod.Seed.Key))
				}
			}
			if len(mod.Seed.Rows) == 0 {
				errs = append(errs, fmt.Sprintf("%s.rows is empty", where))
			}
			for rri, row := range mod.Seed.Rows {
				if len(row) == 0 {
					errs = append(errs, fmt.Sprintf("%s.rows[%d] is an empty object", where, rri))
				}
			}
		}
		// Stage machine: stage_field must name a declared column; stage keys
		// unique; transitions/hooks must reference declared stage keys (or "*"
		// for hooks); a hook's `do` carries a wasm:/webhook:/compiled: prefix
		// (the schema pattern already enforces the shape). Mirrors the legacy
		// validator so a manifest fails identically on both surfaces.
		errs = append(errs, validateStageMachine(mi, mod, ownCols)...)
		// Row-locking strategy (guards): only ""/"row" are understood.
		if mod.Locking != "" && mod.Locking != "row" {
			errs = append(errs, fmt.Sprintf("models[%d].locking %q is not one of \"\"|\"row\"", mi, mod.Locking))
		}
		// Folio sequences: unique keys, scope enum, a well-formed format with
		// exactly one {seq}/{seq:0N} placeholder.
		seqKeys := make(map[string]struct{}, len(mod.Sequences))
		for si, sq := range mod.Sequences {
			where := fmt.Sprintf("models[%d].sequences[%d]", mi, si)
			if strings.TrimSpace(sq.Key) == "" {
				errs = append(errs, fmt.Sprintf("%s.key is empty", where))
			} else if _, dup := seqKeys[sq.Key]; dup {
				errs = append(errs, fmt.Sprintf("%s.key %q is duplicated on the model", where, sq.Key))
			} else {
				seqKeys[sq.Key] = struct{}{}
			}
			if sq.Scope != "" && sq.Scope != "org" && sq.Scope != "branch" {
				errs = append(errs, fmt.Sprintf("%s.scope %q is not one of \"\"|\"org\"|\"branch\"", where, sq.Scope))
			}
			if err := validateSequenceFormat(sq.Format); err != nil {
				errs = append(errs, fmt.Sprintf("%s.format %q: %v", where, sq.Format, err))
			}
		}
		// Static-option cascade guards + declarative Constraints on model columns.
		for ci, c := range mod.Columns {
			if c.Sequence != "" {
				if _, ok := seqKeys[c.Sequence]; !ok {
					errs = append(errs, fmt.Sprintf("models[%d].columns[%d].sequence %q is not a declared sequence key on the model", mi, ci, c.Sequence))
				}
			}
			for cci, con := range c.Constraints {
				cw := fmt.Sprintf("models[%d].columns[%d].constraints[%d]", mi, ci, cci)
				if strings.TrimSpace(con.Expr) == "" {
					errs = append(errs, fmt.Sprintf("%s.expr is empty", cw))
				} else if err := validateConstraintExpr(con.Expr, ownCols); err != nil {
					errs = append(errs, fmt.Sprintf("%s.expr %q: %v", cw, con.Expr, err))
				}
				if strings.TrimSpace(con.ErrorKey) == "" {
					errs = append(errs, fmt.Sprintf("%s.error_key is empty", cw))
				}
				errs = append(errs, validateConstraintApproval(cw, con)...)
			}
			if c.Options.Len() == 0 {
				continue
			}
			where := fmt.Sprintf("models[%d].columns[%d]", mi, ci)
			errs = append(errs, validateOptionWhen(where, c.DependsOn, c.Options.Static)...)
		}
		for ri, rel := range mod.Relations {
			where := fmt.Sprintf("models[%d].relations[%d]", mi, ri)
			switch rel.Kind {
			case "one_to_many", "many_to_many":
				// known
			default:
				errs = append(errs, fmt.Sprintf("%s.kind %q is not one of one_to_many|many_to_many", where, rel.Kind))
			}
			if rel.Name == "" {
				errs = append(errs, fmt.Sprintf("%s.name is empty", where))
			}
			if rel.Through == "" {
				errs = append(errs, fmt.Sprintf("%s.through is empty", where))
			}
			if rel.ForeignKey == "" {
				errs = append(errs, fmt.Sprintf("%s.foreign_key is empty", where))
			}
			// Tier-1 rollups: target must be a column on the PARENT (this
			// model); from (if present) must be a column on the CHILD
			// (relation.Through); fn must be in the enum; exactly one of
			// from/expr (count may omit both); expr must pass the strict
			// arithmetic allowlist against the CHILD's columns.
			childCols := colsByModel[rel.Through]
			for ki, rl := range rel.Rollups {
				rw := fmt.Sprintf("%s.rollups[%d]", where, ki)
				errs = append(errs, validateRollupSpec(rw, rl, ownCols, childCols)...)
			}
		}
	}

	if m.Contributions != nil {
		errs = append(errs, validateDashboard(&m)...)
		errs = append(errs, validateNavViewTypes(&m)...)
		errs = append(errs, validateDocuments(&m, colsByModel)...)
		errs = append(errs, validatePublicRoutes(&m)...)
		errs = append(errs, validateConditions(&m)...)
		errs = append(errs, validateRoutes(&m)...)
		errs = append(errs, validateRules(&m, colsByModel)...)
		// contributions.config: exactly one target (model XOR url); a model
		// target must reference one of the addon's own models.
		if cfg := m.Contributions.Config; cfg != nil {
			switch {
			case cfg.Model == "" && cfg.URL == "":
				errs = append(errs, "contributions.config: one of model or url is required")
			case cfg.Model != "" && cfg.URL != "":
				errs = append(errs, "contributions.config: model and url are mutually exclusive")
			case cfg.Model != "":
				if _, ok := colsByModel[cfg.Model]; !ok {
					errs = append(errs, fmt.Sprintf("contributions.config.model %q is not a model of this addon", cfg.Model))
				}
			}
		}
	}

	// Addon-level pipeline-runtime primitives (connectors / schedules / webhooks).
	errs = append(errs, validatePipelineRuntime(&m)...)

	if m.Contributions != nil {
		for ai, a := range m.Contributions.Actions {
			if a.Idempotency != nil && strings.TrimSpace(a.Idempotency.KeyField) == "" {
				errs = append(errs, fmt.Sprintf("contributions.actions[%d].idempotency requires a non-empty key_field", ai))
			}
			// Supervised actions: roles non-empty; `when` (optional) is a guard
			// predicate over the target model's columns ∪ the action's own
			// field keys (the merged record ∪ payload environment it is
			// evaluated against at dispatch).
			if a.Approval != nil {
				aw := fmt.Sprintf("contributions.actions[%d].approval", ai)
				errs = append(errs, validateApprovalPolicy(aw, a.Approval)...)
				if strings.TrimSpace(a.Approval.When) != "" {
					env := map[string]struct{}{}
					for k := range colsByModel[a.TargetModel] {
						env[k] = struct{}{}
					}
					for _, f := range a.Fields {
						env[f.Key] = struct{}{}
					}
					for _, st := range a.Steps {
						for _, f := range st.Fields {
							env[f.Key] = struct{}{}
						}
					}
					// A target model this manifest does not own (an extension or a
					// cross-addon action) has no column index here: fall back to a
					// syntax-only check so a legitimate foreign column is not refused.
					if _, own := colsByModel[a.TargetModel]; !own {
						env = nil
					}
					if err := validateConstraintExpr(a.Approval.When, env); err != nil {
						errs = append(errs, fmt.Sprintf("%s.when %q: %v", aw, a.Approval.When, err))
					}
				}
			}
			// Wizard steps: a step needs a title and at least one field, and a
			// wizard replaces the flat form (steps ⊕ fields is ambiguous).
			if len(a.Steps) > 0 && len(a.Fields) > 0 {
				errs = append(errs, fmt.Sprintf("contributions.actions[%d] declares both steps and fields — a wizard replaces the flat form, declare one or the other", ai))
			}
			for si, st := range a.Steps {
				sw := fmt.Sprintf("contributions.actions[%d].steps[%d]", ai, si)
				if strings.TrimSpace(st.Title) == "" {
					errs = append(errs, fmt.Sprintf("%s.title is empty", sw))
				}
				if len(st.Fields) == 0 {
					errs = append(errs, fmt.Sprintf("%s.fields is empty (a wizard page must render something)", sw))
				}
			}
			for fi, f := range a.Fields {
				// Static-option cascade guards on action fields and their
				// nested item_fields (line-items cells).
				fw := fmt.Sprintf("contributions.actions[%d].fields[%d]", ai, fi)
				if f.Options.Len() > 0 {
					errs = append(errs, validateOptionWhen(fw, f.DependsOn, f.Options.Static)...)
				}
				for ii, it := range f.ItemFields {
					if it.Options.Len() == 0 {
						continue
					}
					iw := fmt.Sprintf("%s.item_fields[%d]", fw, ii)
					errs = append(errs, validateOptionWhen(iw, it.DependsOn, it.Options.Static)...)
				}
				if f.Balance == nil {
					continue
				}
				where := fmt.Sprintf("contributions.actions[%d].fields[%d]", ai, fi)
				if len(f.ItemFields) == 0 {
					errs = append(errs, fmt.Sprintf("%s declares a balance rule but has no item_fields (balance only applies to a line-items array field)", where))
					continue
				}
				cols := map[string]struct{}{}
				for _, it := range f.ItemFields {
					cols[it.Key] = struct{}{}
				}
				if f.Balance.DebitColumn == "" || f.Balance.CreditColumn == "" {
					errs = append(errs, fmt.Sprintf("%s.balance requires both debit_column and credit_column", where))
				}
				if f.Balance.DebitColumn != "" {
					if _, ok := cols[f.Balance.DebitColumn]; !ok {
						errs = append(errs, fmt.Sprintf("%s.balance.debit_column %q is not one of the field's item_fields", where, f.Balance.DebitColumn))
					}
				}
				if f.Balance.CreditColumn != "" {
					if _, ok := cols[f.Balance.CreditColumn]; !ok {
						errs = append(errs, fmt.Sprintf("%s.balance.credit_column %q is not one of the field's item_fields", where, f.Balance.CreditColumn))
					}
				}
			}
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("v3: manifest validation failed:\n  - %s", strings.Join(errs, "\n  - "))
	}
	return nil
}

// Parse runs Validate and, on success, returns the typed manifest.
func Parse(raw []byte) (*Manifest, error) {
	if err := Validate(raw); err != nil {
		return nil, err
	}
	var m Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("v3: decode: %w", err)
	}
	return &m, nil
}

// addonKeyRe is the shape an addon key must have to be referenced by a
// contribution condition — the same lowercase snake/kebab vocabulary the hub
// enforces on publish.
var addonKeyRe = regexp.MustCompile(`^[a-z][a-z0-9_-]*$`)

// validateConditions checks every contribution-level `condition` block: a
// declared condition must carry a predicate (an empty object is an authoring
// mistake, not a no-op) and its addon_installed key must look like an addon
// key. A condition may reference an addon this manifest does NOT declare in
// compatibility.requires — that is the whole point: it is a SOFT dependency
// resolved per organization at serve time.
func validateConditions(m *Manifest) []string {
	var errs []string
	check := func(where string, c *Condition) {
		if c == nil {
			return
		}
		key := strings.TrimSpace(c.AddonInstalled)
		field := strings.TrimSpace(c.Field)
		if key == "" && field == "" {
			errs = append(errs, fmt.Sprintf("%s.condition declares no predicate (set addon_installed or field)", where))
			return
		}
		if key != "" && !addonKeyRe.MatchString(key) {
			errs = append(errs, fmt.Sprintf("%s.condition.addon_installed %q is not a valid addon key", where, key))
		}
	}
	var walkNav func(items []NavItem, where string)
	walkNav = func(items []NavItem, where string) {
		for i, it := range items {
			w := fmt.Sprintf("%s[%d]", where, i)
			check(w, it.Condition)
			if len(it.Items) > 0 {
				walkNav(it.Items, w+".items")
			}
		}
	}
	for gi, g := range m.Contributions.Navigation {
		w := fmt.Sprintf("contributions.navigation[%d]", gi)
		check(w, g.Condition)
		walkNav(g.Items, w+".items")
	}
	for i, a := range m.Contributions.Actions {
		check(fmt.Sprintf("contributions.actions[%d]", i), a.Condition)
	}
	for i, s := range m.Contributions.Slots {
		check(fmt.Sprintf("contributions.slots[%d]", i), s.Condition)
	}
	for i, w := range m.Contributions.Dashboard {
		check(fmt.Sprintf("contributions.dashboard[%d]", i), w.Condition)
	}
	return errs
}

// validateRoutes enforces the invariants of contributions.routes[] the JSON
// schema cannot express: domain and handler are present, and no two routes of
// the SAME domain declare the same match at the same priority — that pair would
// make the winner depend on declaration order within one manifest, which is
// exactly the ambiguity the priority field exists to remove.
func validateRoutes(m *Manifest) []string {
	var errs []string
	seen := make(map[string]int, len(m.Contributions.Routes))
	for i, r := range m.Contributions.Routes {
		where := fmt.Sprintf("contributions.routes[%d]", i)
		if strings.TrimSpace(r.Domain) == "" {
			errs = append(errs, where+".domain is empty")
		}
		if strings.TrimSpace(r.Handler) == "" {
			errs = append(errs, where+".handler is empty")
		}
		keys := make([]string, 0, len(r.Match))
		for k, v := range r.Match {
			if strings.TrimSpace(k) == "" {
				errs = append(errs, where+".match has an empty attribute name")
				continue
			}
			keys = append(keys, k+"="+v)
		}
		sort.Strings(keys)
		sig := fmt.Sprintf("%s|%d|%s", r.Domain, r.Priority, strings.Join(keys, ","))
		if prev, dup := seen[sig]; dup {
			errs = append(errs, fmt.Sprintf("%s duplicates the match of contributions.routes[%d] at the same priority (%d) — give one of them a higher priority", where, prev, r.Priority))
			continue
		}
		seen[sig] = i
	}
	return errs
}
