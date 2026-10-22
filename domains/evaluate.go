package domains

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// RecordStatus is the verdict on one required record.
type RecordStatus string

const (
	// RecordOK: the observed DNS satisfies the record (and no resolver
	// contradicts it).
	RecordOK RecordStatus = "ok"
	// RecordMissing: authoritatively absent (NXDOMAIN / no data) everywhere.
	RecordMissing RecordStatus = "missing"
	// RecordWrongValue: records exist at that name/type but none satisfies the
	// requirement (typo, stale value, a second SPF, a shadowing MX).
	RecordWrongValue RecordStatus = "wrong_value"
	// RecordPropagating: it will probably be fine soon: some resolvers already
	// see it and others do not, or we wrote it recently and caches have not
	// expired.
	RecordPropagating RecordStatus = "propagating"
	// RecordUnknown: the lookup itself failed (timeout, SERVFAIL). NOT a
	// statement about the zone; retry. Never rendered as "missing".
	RecordUnknown RecordStatus = "unknown"
)

// Valid reports whether s is a known verdict.
func (s RecordStatus) Valid() bool {
	switch s {
	case RecordOK, RecordMissing, RecordWrongValue, RecordPropagating, RecordUnknown:
		return true
	}
	return false
}

// Severity of a diagnostic.
type Severity string

const (
	SeverityInfo  Severity = "info"
	SeverityWarn  Severity = "warn"
	SeverityError Severity = "error"
)

// Diagnostic codes (message_key is "domains.diag.<code>").
const (
	DiagNotFound          = "not_found"
	DiagValueMismatch     = "value_mismatch"
	DiagMultipleSPF       = "multiple_spf"
	DiagMXShadowed        = "mx_shadowed"
	DiagPartialPropag     = "partial_propagation"
	DiagRecentlyApplied   = "recently_applied"
	DiagLookupFailed      = "lookup_failed"
	DiagUniqueHasExtra    = "unique_has_extra_values"
	DiagTokenNotPublished = "ownership_token_missing"
	DiagTokenMismatch     = "ownership_token_mismatch"
	DiagTokenMissingInput = "ownership_no_token"
)

// Diagnostic explains a verdict in a form the UI can localize.
type Diagnostic struct {
	Code       string         `json:"code"`
	MessageKey string         `json:"message_key"`
	Severity   Severity       `json:"severity"`
	Params     map[string]any `json:"params,omitempty"`
	// Detail is raw technical text (resolver error...), never the headline.
	Detail string `json:"detail,omitempty"`
}

func diag(code string, sev Severity, params map[string]any, detail string) Diagnostic {
	return Diagnostic{Code: code, MessageKey: "domains.diag." + code, Severity: sev, Params: params, Detail: detail}
}

// Answer is what ONE resolver saw for ONE slot (type + name).
type Answer struct {
	Resolver string
	// Values are the canonical observed values (see DNSResolver).
	Values []string
	// Err is a lookup failure; ErrNoRecords (or empty Values with nil Err) is
	// the authoritative empty answer.
	Err error
}

func (a Answer) empty() bool {
	return errors.Is(a.Err, ErrNoRecords) || (a.Err == nil && len(a.Values) == 0)
}
func (a Answer) failed() bool { return a.Err != nil && !errors.Is(a.Err, ErrNoRecords) }

// Observations maps a slot key (Record.Key()) to the answers of each resolver.
type Observations map[string][]Answer

// RecordCheck is the verdict on one required record.
type RecordCheck struct {
	Record Record       `json:"record"`
	Status RecordStatus `json:"status"`
	// Observed is the union of values any resolver returned (sorted).
	Observed []string `json:"observed,omitempty"`
	// PerResolver is the verdict at each vantage point.
	PerResolver map[string]RecordStatus `json:"per_resolver,omitempty"`
	Diagnostics []Diagnostic            `json:"diagnostics,omitempty"`
}

// RecordSetCheck is the verdict on a whole RecordSet.
type RecordSetCheck struct {
	Domain string               `json:"domain"`
	Checks []RecordCheck        `json:"checks"`
	Counts map[RecordStatus]int `json:"counts"`
	// AllOK is true when every non-Optional record is ok.
	AllOK     bool      `json:"all_ok"`
	CheckedAt time.Time `json:"checked_at"`
}

// EvalOptions tune Evaluate.
type EvalOptions struct {
	// AppliedAt is when the platform last wrote the records (zero = never).
	// A not-yet-visible record written inside Window is propagating, not
	// missing.
	AppliedAt time.Time
	// Window is how long after AppliedAt a stale view counts as propagation
	// (default 1h, the common max TTL of a freshly changed record).
	Window time.Duration
	// Now overrides time.Now (tests).
	Now time.Time
}

const defaultPropagationWindow = time.Hour

// Evaluate compares the required records with what the resolvers saw. It is a
// pure function: all I/O happened in Check (or in the host).
func Evaluate(set RecordSet, obs Observations, opt EvalOptions) RecordSetCheck {
	now := opt.Now
	if now.IsZero() {
		now = time.Now()
	}
	window := opt.Window
	if window <= 0 {
		window = defaultPropagationWindow
	}
	recent := !opt.AppliedAt.IsZero() && now.Sub(opt.AppliedAt) < window && !opt.AppliedAt.After(now.Add(time.Minute))

	slots := set.Slots()
	res := RecordSetCheck{Domain: set.Domain, Counts: map[RecordStatus]int{}, CheckedAt: now, AllOK: true}
	for _, rec := range set.Records {
		c := evalRecord(rec, slots[rec.Key()], obs[rec.Key()], recent)
		res.Counts[c.Status]++
		if c.Status != RecordOK && !rec.Optional {
			res.AllOK = false
		}
		res.Checks = append(res.Checks, c)
	}
	return res
}

func evalRecord(rec Record, siblings []Record, answers []Answer, recent bool) RecordCheck {
	c := RecordCheck{Record: rec, PerResolver: map[string]RecordStatus{}}
	seen := map[string]struct{}{}
	var ok, missing, wrong, unknown int
	var diags []Diagnostic
	for _, a := range answers {
		for _, v := range a.Values {
			if _, dup := seen[v]; !dup {
				seen[v] = struct{}{}
				c.Observed = append(c.Observed, v)
			}
		}
		st, ds := evalAnswer(rec, siblings, a)
		c.PerResolver[a.Resolver] = st
		diags = append(diags, ds...)
		switch st {
		case RecordOK:
			ok++
		case RecordMissing:
			missing++
		case RecordWrongValue:
			wrong++
		default:
			unknown++
		}
	}
	sort.Strings(c.Observed)
	contradict := missing + wrong

	switch {
	case len(answers) == 0 || (ok == 0 && contradict == 0):
		c.Status = RecordUnknown
	case ok > 0 && contradict == 0:
		c.Status = RecordOK
	case ok > 0:
		c.Status = RecordPropagating
		diags = append(diags, diag(DiagPartialPropag, SeverityInfo, map[string]any{"ok": ok, "pending": contradict + unknown}, ""))
	case wrong > 0:
		c.Status = RecordWrongValue
	default:
		c.Status = RecordMissing
	}
	if recent && (c.Status == RecordMissing || c.Status == RecordWrongValue) {
		c.Status = RecordPropagating
		diags = append(diags, diag(DiagRecentlyApplied, SeverityInfo, nil, ""))
	}
	if c.Status == RecordOK || c.Status == RecordPropagating {
		// Per-resolver detail of a verdict that is not a problem would only
		// confuse; keep the partial/recent notes.
		diags = keepDiags(diags, DiagPartialPropag, DiagRecentlyApplied, DiagUniqueHasExtra)
	}
	c.Diagnostics = dedupeDiags(diags)
	if len(c.PerResolver) == 0 {
		c.PerResolver = nil
	}
	return c
}

func keepDiags(in []Diagnostic, codes ...string) []Diagnostic {
	var out []Diagnostic
	for _, d := range in {
		for _, c := range codes {
			if d.Code == c {
				out = append(out, d)
				break
			}
		}
	}
	return out
}

func dedupeDiags(in []Diagnostic) []Diagnostic {
	seen := map[string]struct{}{}
	var out []Diagnostic
	for _, d := range in {
		k := d.Code + "|" + fmt.Sprint(d.Params)
		if _, dup := seen[k]; dup {
			continue
		}
		seen[k] = struct{}{}
		out = append(out, d)
	}
	return out
}

func evalAnswer(rec Record, siblings []Record, a Answer) (RecordStatus, []Diagnostic) {
	switch {
	case a.failed():
		return RecordUnknown, []Diagnostic{diag(DiagLookupFailed, SeverityWarn, map[string]any{"resolver": a.Resolver}, a.Err.Error())}
	case a.empty():
		return RecordMissing, []Diagnostic{diag(DiagNotFound, SeverityError, map[string]any{"name": rec.Name, "type": string(rec.Type)}, "")}
	}
	want := rec.Canonical()
	matched := false
	for _, v := range a.Values {
		if valueSatisfies(rec, want, v) {
			matched = true
			break
		}
	}
	var diags []Diagnostic

	if rec.Type == TypeTXT && strings.HasPrefix(strings.ToLower(want), "v=spf1") {
		n := 0
		for _, v := range a.Values {
			if strings.HasPrefix(strings.ToLower(canonTXT(v)), "v=spf1") {
				n++
			}
		}
		if n > 1 {
			return RecordWrongValue, append(diags, diag(DiagMultipleSPF, SeverityError, map[string]any{"name": rec.Name, "count": n},
				"several v=spf1 records make SPF evaluation fail (permerror)"))
		}
	}
	if rec.Type == TypeMX {
		if shadow := shadowingMX(rec, siblings, a.Values); shadow != "" {
			return RecordWrongValue, append(diags, diag(DiagMXShadowed, SeverityError, map[string]any{"name": rec.Name, "shadow": shadow},
				"a lower-preference MX sends mail elsewhere"))
		}
	}
	if matched {
		if rec.Unique && len(a.Values) > 1 {
			diags = append(diags, diag(DiagUniqueHasExtra, SeverityWarn, map[string]any{"name": rec.Name, "count": len(a.Values)}, ""))
		}
		return RecordOK, diags
	}
	return RecordWrongValue, append(diags, diag(DiagValueMismatch, SeverityError,
		map[string]any{"name": rec.Name, "type": string(rec.Type), "expected": want, "observed": clip(a.Values, 5)}, ""))
}

func clip(v []string, n int) []string {
	if len(v) <= n {
		return v
	}
	return v[:n]
}

// valueSatisfies tells whether one observed canonical value meets rec.
func valueSatisfies(rec Record, want, got string) bool {
	switch rec.Type {
	case TypeTXT:
		got = canonTXT(got)
		needle := want
		if rec.Needle != "" {
			needle = canonTXT(rec.Needle)
		}
		switch rec.Match {
		case MatchContains:
			return strings.Contains(strings.ToLower(got), strings.ToLower(needle))
		case MatchPrefix:
			return strings.HasPrefix(strings.ToLower(got), strings.ToLower(needle))
		}
		return got == want
	case TypeMX:
		pri, host, ok := splitMX(got)
		return ok && strconv.Itoa(pri)+" "+host == want
	case TypeCNAME:
		return canonHost(got) == want
	}
	return (Record{Type: rec.Type, Value: got}).Canonical() == want
}

func splitMX(v string) (int, string, bool) {
	f := strings.Fields(v)
	if len(f) != 2 {
		return 0, "", false
	}
	p, err := strconv.Atoi(f[0])
	if err != nil {
		return 0, "", false
	}
	return p, canonHost(f[1]), true
}

// shadowingMX returns a foreign MX target (one that is not part of the
// required MX set) with a strictly lower preference number than rec's: it wins
// delivery, so our record is not where mail goes. "" when none.
func shadowingMX(rec Record, siblings []Record, observed []string) string {
	own := map[string]struct{}{}
	for _, s := range siblings {
		own[canonHost(s.Value)] = struct{}{}
	}
	for _, v := range observed {
		p, h, ok := splitMX(v)
		if _, mine := own[h]; ok && !mine && p < rec.Priority {
			return h
		}
	}
	return ""
}

// CheckOptions tune Check.
type CheckOptions struct {
	EvalOptions
	// LookupTimeout bounds each lookup (default 5s).
	LookupTimeout time.Duration
	// Concurrency bounds parallel lookups (default 8).
	Concurrency int
}

// Check resolves every slot of set through every resolver and evaluates. With
// two or more resolvers a record seen by only some of them is "propagating".
func Check(ctx context.Context, resolvers []DNSResolver, set RecordSet, opt CheckOptions) (RecordSetCheck, error) {
	if len(resolvers) == 0 {
		return RecordSetCheck{}, errors.New("domains: Check needs at least one resolver")
	}
	if err := set.Validate(); err != nil {
		return RecordSetCheck{}, err
	}
	timeout := opt.LookupTimeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	conc := opt.Concurrency
	if conc <= 0 {
		conc = 8
	}
	type slot struct {
		key  string
		name string
		typ  RecordType
	}
	var slots []slot
	seen := map[string]struct{}{}
	for _, r := range set.Records {
		if _, dup := seen[r.Key()]; !dup {
			seen[r.Key()] = struct{}{}
			slots = append(slots, slot{r.Key(), r.Name, r.Type})
		}
	}
	obs := make(Observations, len(slots))
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, conc)
	for _, s := range slots {
		for _, r := range resolvers {
			wg.Add(1)
			sem <- struct{}{}
			go func(s slot, r DNSResolver) {
				defer wg.Done()
				defer func() { <-sem }()
				lctx, cancel := context.WithTimeout(ctx, timeout)
				defer cancel()
				vals, err := r.Lookup(lctx, s.name, s.typ)
				mu.Lock()
				obs[s.key] = append(obs[s.key], Answer{Resolver: r.Name(), Values: vals, Err: err})
				mu.Unlock()
			}(s, r)
		}
	}
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return RecordSetCheck{}, err
	}
	for k := range obs { // goroutine order is arbitrary: make results deterministic
		sort.Slice(obs[k], func(i, j int) bool { return obs[k][i].Resolver < obs[k][j].Resolver })
	}
	return Evaluate(set, obs, opt.EvalOptions), nil
}
