package dynamic

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/asteby/metacore-kernel/manifest"
	"github.com/asteby/metacore-kernel/modelbase"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// SequenceRecord is the kernel-owned table backing declarative folio sequences
// (manifest v3 Model.sequences). One row per (org, scope_value, model, seq_key)
// holds the last-issued value; the next value is produced by an atomic
// UPSERT … RETURNING so concurrent creates never collide on a folio number.
//
// scope_value partitions the counter: "" for an org-scoped sequence (the org is
// already the row's tenant), or the branch id for a branch-scoped one (each
// branch gets its own series). Follows the same lazily-auto-migrated,
// kernel-owned pattern as dynamic.Migration.
type SequenceRecord struct {
	ID         uint64    `gorm:"primaryKey"`
	OrgID      uuid.UUID `gorm:"type:uuid;not null;uniqueIndex:idx_metacore_sequence"`
	ScopeValue string    `gorm:"column:scope_value;size:64;not null;uniqueIndex:idx_metacore_sequence"`
	Model      string    `gorm:"size:200;not null;uniqueIndex:idx_metacore_sequence"`
	SeqKey     string    `gorm:"column:seq_key;size:200;not null;uniqueIndex:idx_metacore_sequence"`
	Value      int64     `gorm:"not null"`
}

// TableName pins the physical table name (public schema).
func (SequenceRecord) TableName() string { return "metacore_sequences" }

// ModelSequences is the resolved folio configuration for one model: the declared
// counters plus the column→sequence-key bindings that drive auto-stamping on
// create. A host builds it straight off the installed manifest.
type ModelSequences struct {
	Sequences []manifest.SequenceDef
	// ColumnBindings maps a column name to the sequence key whose next formatted
	// value is stamped on create when that column is empty.
	ColumnBindings map[string]string
	// Model is the canonical identity the counters are keyed by in
	// metacore_sequences. A host resolver usually answers for several aliases of
	// the same model (model key "SalesOrder", table "sales_orders", qualified
	// "customers.SalesOrder"); without a canonical name each alias would own an
	// independent counter and the CRUD create, the wasm data_mutate stamp and the
	// wasm sequence_next import would hand out duplicate folios. Empty = key the
	// counter by the name the caller passed (legacy behaviour).
	Model string
}

// counterModel is the metacore_sequences.model value for a request made under
// the name `requested`: the resolver's canonical Model when it declares one.
func (ms *ModelSequences) counterModel(requested string) string {
	if ms != nil && ms.Model != "" {
		return ms.Model
	}
	return requested
}

// SequenceResolver returns the folio configuration for a model name. Hosts wire
// it from their addon registry; the kernel owns no global model index.
// Returning (nil, false) — or an empty config — disables folios for that model.
type SequenceResolver func(ctx context.Context, model string) (*ModelSequences, bool)

func (ms *ModelSequences) spec(key string) (manifest.SequenceDef, bool) {
	if ms == nil {
		return manifest.SequenceDef{}, false
	}
	for _, s := range ms.Sequences {
		if s.Key == key {
			return s, true
		}
	}
	return manifest.SequenceDef{}, false
}

var (
	sequenceMu       sync.Mutex
	sequenceMigrated = map[*gorm.DB]bool{}
)

func ensureSequenceTable(db *gorm.DB) error {
	sequenceMu.Lock()
	defer sequenceMu.Unlock()
	if sequenceMigrated[db] {
		return nil
	}
	if err := db.AutoMigrate(&SequenceRecord{}); err != nil {
		return fmt.Errorf("migrate metacore_sequences: %w", err)
	}
	sequenceMigrated[db] = true
	return nil
}

// nextSequenceValue atomically increments and returns the counter for
// (org, scopeValue, model, key). The UPSERT … RETURNING is a single statement,
// so parallel callers are serialized by the unique index without an explicit
// lock. The first call for a tuple returns 1.
func nextSequenceValue(ctx context.Context, db *gorm.DB, orgID uuid.UUID, scopeValue, model, key string) (int64, error) {
	if err := ensureSequenceTable(db); err != nil {
		return 0, err
	}
	return bumpSequence(ctx, db, orgID, scopeValue, model, key)
}

// bumpSequence is the UPSERT … RETURNING on an already-migrated counter table.
// `exec` may be a transaction: the wasm data_mutate path passes its open tx so
// the increment commits (or rolls back) together with the row it numbers.
func bumpSequence(ctx context.Context, exec *gorm.DB, orgID uuid.UUID, scopeValue, model, key string) (int64, error) {
	var value int64
	err := exec.WithContext(ctx).Raw(`
		INSERT INTO metacore_sequences (org_id, scope_value, model, seq_key, value)
		VALUES (?, ?, ?, ?, 1)
		ON CONFLICT (org_id, scope_value, model, seq_key)
		DO UPDATE SET value = metacore_sequences.value + 1
		RETURNING value`, orgID, scopeValue, model, key).Scan(&value).Error
	if err != nil {
		return 0, fmt.Errorf("dynamic: sequence_next %s/%s: %w", model, key, err)
	}
	return value, nil
}

var seqPlaceholderRe = regexp.MustCompile(`\{seq(?::0(\d+))?\}`)

// FormatSequence renders a sequence's Format template around the numeric value.
// `{seq}` emits the bare number; `{seq:0N}` zero-pads to width N (e.g.
// "A-{seq:06}" with 42 → "A-000042"). A template with no placeholder is returned
// unchanged (validation rejects that shape at publish time). Period and scope
// tokens are left as written; use FormatSequenceAt to render them.
func FormatSequence(format string, n int64) string {
	return FormatSequenceAt(format, n, time.Time{}, "")
}

// FormatSequenceAt is FormatSequence plus the v2 tokens: {yyyy} {yy} {mm} {dd}
// render the period date `at` (skipped when `at` is zero) and {scope} renders
// the partition's scope value (skipped when empty).
func FormatSequenceAt(format string, n int64, at time.Time, scope string) string {
	if format == "" {
		return strconv.FormatInt(n, 10)
	}
	out := seqPlaceholderRe.ReplaceAllStringFunc(format, func(m string) string {
		sub := seqPlaceholderRe.FindStringSubmatch(m)
		if len(sub) == 2 && sub[1] != "" {
			width, _ := strconv.Atoi(sub[1])
			return fmt.Sprintf("%0*d", width, n)
		}
		return strconv.FormatInt(n, 10)
	})
	if !at.IsZero() {
		out = strings.NewReplacer(
			"{yyyy}", at.Format("2006"), "{yy}", at.Format("06"),
			"{mm}", at.Format("01"), "{dd}", at.Format("02"),
		).Replace(out)
	}
	if scope != "" {
		out = strings.ReplaceAll(out, "{scope}", scope)
	}
	return out
}

// sequenceNow is the clock period-scoped counters read (UTC). Tests swap it.
var sequenceNow = func() time.Time { return time.Now().UTC() }

// periodKey is the counter-partition suffix of a period: "" (none), "2026",
// "2026-11" or "2026-11-19".
func periodKey(period string, at time.Time) string {
	switch period {
	case "year":
		return at.Format("2006")
	case "month":
		return at.Format("2006-01")
	case "day":
		return at.Format("2006-01-02")
	}
	return ""
}

// seqPartition is the resolved counter partition of one issuance: the value
// stored in metacore_sequences.scope_value and the {scope} token text.
type seqPartition struct {
	value string // scope_value column: "<scope>" or "<scope>|<period>" or "<period>"
	scope string // {scope} token
	at    time.Time
}

// resolvePartition derives the partition of `spec` for one issuance at time
// the clock `now` (called only when the sequence has a period; see Service.opClock). `branch` is the
// branch id (user's or row's), `row` the record being numbered (read for
// scope: column). An empty scope falls back to the org-wide partition, EXCEPT
// when the format renders {scope}: a folio like "{scope}-001" must never be
// issued, so that case is a 422 on the scope column.
func resolvePartition(spec manifest.SequenceDef, branch string, row map[string]any, now func() time.Time) (seqPartition, error) {
	p := seqPartition{}
	dated := spec.Period != "" && spec.Period != "none"
	var at time.Time
	if dated {
		// The clock is consulted ONLY for a period-scoped sequence: a host's
		// SequenceClock may hit the database (org timezone). Date tokens in the
		// format ({yyyy} {yy} {mm} {dd}) render only with a period, so a
		// period-less sequence never needs the instant.
		at = now()
		p.at = at
	}
	switch strings.ToLower(spec.Scope) {
	case "branch":
		p.scope = branch
	case "column":
		if v := row[spec.ScopeColumn]; v != nil {
			if str := strings.TrimSpace(fmt.Sprint(v)); str != "" && str != nilUUIDString {
				p.scope = str
			}
		}
	}
	if p.scope == "" && strings.Contains(spec.Format, "{scope}") {
		field := spec.ScopeColumn
		if field == "" {
			field = "branch_id"
		}
		ve := &ValidationError{}
		ve.add(field, codeRequired, map[string]any{"sequence": spec.Key})
		return p, ve
	}
	p.value = p.scope
	if pk := periodKey(spec.Period, at); pk != "" {
		if p.value != "" {
			p.value += "|"
		}
		p.value += pk
	}
	// metacore_sequences.scope_value is varchar(64): SQLite does not enforce
	// it, Postgres does. A longer partition is replaced by a stable hash.
	if len(p.value) > 64 {
		sum := sha256.Sum256([]byte(p.value))
		p.value = "h:" + hex.EncodeToString(sum[:])[:40]
	}
	return p, nil
}

// seqNow is the instant period-scoped counters are cut on. Default: UTC. A
// host wires Config.SequenceClock to cut on the organization's own calendar
// (a fiscal "day"/"year" must follow the org timezone, not UTC).
func (s *Service) seqNow(ctx context.Context, orgID uuid.UUID) time.Time {
	if s.sequenceClock != nil {
		return s.sequenceClock(ctx, orgID)
	}
	return sequenceNow()
}

// opClock returns the instant source of ONE operation (create, update or
// sequence_next): the host clock runs at most once, lazily, however many
// period-scoped sequences the model has; zero times if none needs it.
func (s *Service) opClock(ctx context.Context, orgID uuid.UUID) func() time.Time {
	var (
		done bool
		at   time.Time
	)
	return func() time.Time {
		if !done {
			at, done = s.seqNow(ctx, orgID), true
		}
		return at
	}
}

func userBranch(user modelbase.AuthUser) string {
	if b, ok := user.(interface{ GetBranchID() uuid.UUID }); ok {
		if id := b.GetBranchID(); id != uuid.Nil {
			return id.String()
		}
	}
	return ""
}

// assignsOnCreate reports whether a sequence stamps its columns at create.
func assignsOnCreate(spec manifest.SequenceDef) bool {
	return spec.AssignOn == "" || spec.AssignOn == "create"
}

// assignsAtCreate reports whether the sequence stamps on a CREATE: assign_on
// create, or assign_on transition:<stage> when the row is created directly in
// that stage (no transition will ever fire for it).
func assignsAtCreate(spec manifest.SequenceDef, createStage string) bool {
	return assignsOnCreate(spec) || (createStage != "" && assignsOnStage(spec, createStage))
}

// assignsOnStage reports whether the sequence stamps when the record enters
// `stage` (assign_on: "transition:<stage>").
func assignsOnStage(spec manifest.SequenceDef, stage string) bool {
	st, ok := strings.CutPrefix(spec.AssignOn, "transition:")
	return ok && st == stage
}

// scopeValueFor derives the partition value for a sequence's scope. "branch"
// reads an optional GetBranchID() off the user (falling back to org scope when
// the user carries no branch); anything else is org scope ("").
func scopeValueFor(scope string, user modelbase.AuthUser) string {
	if strings.EqualFold(scope, "branch") {
		if b, ok := user.(interface{ GetBranchID() uuid.UUID }); ok {
			if id := b.GetBranchID(); id != uuid.Nil {
				return id.String()
			}
		}
	}
	return ""
}

// NextSequence issues the next formatted folio for (model, key) in the user's
// tenant, honoring the sequence's scope. It is the entry point a host wires into
// the wasm `sequence_next` import and the dynamic engine uses for auto-stamping.
// A `scope: column` sequence needs NextSequenceScoped (the partition value).
func (s *Service) NextSequence(ctx context.Context, user modelbase.AuthUser, model, key string) (string, error) {
	return s.NextSequenceScoped(ctx, user, model, key, "")
}

// NextSequenceScoped is NextSequence for a `scope: column` sequence: scopeValue
// is the partition (the issuer id, the clinic) the folio is drawn from. For
// org/branch sequences scopeValue is ignored. A gapless sequence is refused:
// its guarantee only holds inside the numbered row's transaction, i.e. through
// its bound columns.
func (s *Service) NextSequenceScoped(ctx context.Context, user modelbase.AuthUser, model, key, scopeValue string) (string, error) {
	ms, ok := s.resolveSequences(ctx, model)
	if !ok {
		return "", fmt.Errorf("dynamic: model %q declares no sequences", model)
	}
	spec, ok := ms.spec(key)
	if !ok {
		return "", fmt.Errorf("dynamic: model %q has no sequence %q", model, key)
	}
	if spec.Gapless {
		return "", fmt.Errorf("dynamic: sequence %q on %q is gapless: it is issued only through its bound column", key, model)
	}
	orgID := orgIDFromUser(user)
	if orgID == uuid.Nil {
		return "", fmt.Errorf("dynamic: sequence_next requires a bound org")
	}
	row := map[string]any{}
	if spec.Scope == "column" {
		if strings.TrimSpace(scopeValue) == "" {
			return "", fmt.Errorf("dynamic: sequence %q on %q is scope=column: scope_value is required", key, model)
		}
		row[spec.ScopeColumn] = scopeValue
	}
	part, err := resolvePartition(spec, userBranch(user), row, s.opClock(ctx, orgID))
	if err != nil {
		return "", err
	}
	n, err := nextSequenceValue(ctx, s.db, orgID, part.value, ms.counterModel(model), key)
	if err != nil {
		return "", err
	}
	return FormatSequenceAt(spec.Format, n, part.at, part.scope), nil
}

// StampSequences is the wasm-side twin of assignSequences: it fills every
// sequence-bound column of `row` that the caller left empty, running the counter
// UPSERT on `tx` (the data_mutate transaction) instead of a separate connection.
// An explicit value wins, exactly as on POST /data. There is no AuthUser here,
// so a `scope: branch` sequence takes its partition from the row's own
// `branch_id` (falling back to org scope when the row carries none — the same
// fallback scopeValueFor applies to a branch-less user). Wire it into
// wasm.Host.WithSequenceStamp.
func (s *Service) StampSequences(ctx context.Context, tx *gorm.DB, orgID uuid.UUID, model string, row map[string]any) error {
	ms, ok := s.resolveSequences(ctx, model)
	if !ok || len(ms.ColumnBindings) == 0 {
		return nil
	}
	if orgID == uuid.Nil {
		return fmt.Errorf("dynamic: sequence stamp requires a bound org")
	}
	// The counter table is DDL'd on the service's own handle, never inside the
	// caller's transaction.
	if err := ensureSequenceTable(s.db); err != nil {
		return err
	}
	now := s.opClock(ctx, orgID)
	// Deterministic order: two bound columns must not race for numbers.
	cols := make([]string, 0, len(ms.ColumnBindings))
	for c := range ms.ColumnBindings {
		cols = append(cols, c)
	}
	sort.Strings(cols)
	for _, col := range cols {
		if v, present := row[col]; present && v != nil && v != "" {
			continue
		}
		key := ms.ColumnBindings[col]
		spec, ok := ms.spec(key)
		if !ok {
			return fmt.Errorf("dynamic: column %q binds unknown sequence %q on %q", col, key, model)
		}
		if !assignsAtCreate(spec, s.createStageOf(ctx, model, row)) {
			continue // issued when the record enters its stage
		}
		// There is no AuthUser here, so a `scope: branch` sequence takes its
		// partition from the row's own `branch_id`.
		branch := ""
		if id, err := uuid.Parse(fmt.Sprint(row["branch_id"])); err == nil && id != uuid.Nil {
			branch = id.String()
		}
		part, err := resolvePartition(spec, branch, row, now)
		if err != nil {
			return err
		}
		n, err := bumpSequence(ctx, tx, orgID, part.value, ms.counterModel(model), key)
		if err != nil {
			return err
		}
		row[col] = FormatSequenceAt(spec.Format, n, part.at, part.scope)
	}
	return nil
}

// resolveSequences looks up a model's folio config via the host resolver.
func (s *Service) resolveSequences(ctx context.Context, model string) (*ModelSequences, bool) {
	if s.sequences == nil {
		return nil, false
	}
	ms, ok := s.sequences(ctx, model)
	if !ok || ms == nil || len(ms.Sequences) == 0 {
		return nil, false
	}
	return ms, true
}

// hasGaplessCreateBinding reports whether a create of `model` must number its
// row inside the insert transaction (a gapless sequence bound to a column the
// caller left empty).
func (s *Service) hasGaplessCreateBinding(ctx context.Context, model string, input map[string]any) bool {
	ms, ok := s.resolveSequences(ctx, model)
	if !ok {
		return false
	}
	for col, key := range ms.ColumnBindings {
		if v, present := input[col]; present && v != nil && v != "" {
			continue
		}
		if spec, ok := ms.spec(key); ok && spec.Gapless && assignsAtCreate(spec, s.createStageOf(ctx, model, input)) {
			return true
		}
	}
	return false
}

// hasTransitionSequences reports whether `model` declares a sequence issued on
// entering a stage; Update then runs in a transaction so the number commits
// with the row (gapless) and the stage move.
func (s *Service) hasTransitionSequences(ctx context.Context, model string) bool {
	ms, ok := s.resolveSequences(ctx, model)
	if !ok {
		return false
	}
	for _, key := range ms.ColumnBindings {
		if spec, ok := ms.spec(key); ok && !assignsOnCreate(spec) {
			return true
		}
	}
	return false
}

// assignSequences stamps the next folio value onto every create-bound column
// that the caller left empty, mutating `input` in place before the create write.
// A column whose value the caller supplied is left untouched (an explicit folio
// wins, e.g. during an ETL import of historical numbers). `db` is the handle the
// counter runs on: nil = the service handle (autocommit, gap-tolerant); a
// transaction numbers the row atomically with its insert. gapless selects which
// half runs: false = the non-gapless sequences (before the insert transaction),
// true = the gapless ones (inside it).
func (s *Service) assignSequences(ctx context.Context, db *gorm.DB, model string, user modelbase.AuthUser, input map[string]any, gapless bool) error {
	ms, ok := s.resolveSequences(ctx, model)
	if !ok || len(ms.ColumnBindings) == 0 {
		return nil
	}
	orgID := orgIDFromUser(user)
	now := s.opClock(ctx, orgID)
	cols := make([]string, 0, len(ms.ColumnBindings))
	for c := range ms.ColumnBindings {
		cols = append(cols, c)
	}
	sort.Strings(cols)
	for _, col := range cols {
		key := ms.ColumnBindings[col]
		if v, present := input[col]; present && v != nil && v != "" {
			continue
		}
		spec, ok := ms.spec(key)
		if !ok {
			return fmt.Errorf("dynamic: column %q binds unknown sequence %q on %q", col, key, model)
		}
		if !assignsAtCreate(spec, s.createStageOf(ctx, model, input)) || spec.Gapless != gapless {
			continue
		}
		if err := s.stampOne(ctx, db, ms, spec, model, orgID, user, col, input, input, now); err != nil {
			return err
		}
	}
	return nil
}

// stampOne issues one folio into dest[col]; `row` is the record the scope
// column is read from.
func (s *Service) stampOne(ctx context.Context, db *gorm.DB, ms *ModelSequences, spec manifest.SequenceDef, model string, orgID uuid.UUID, user modelbase.AuthUser, col string, row, dest map[string]any, now func() time.Time) error {
	part, err := resolvePartition(spec, userBranch(user), row, now)
	if err != nil {
		return err
	}
	var n int64
	if db != nil {
		if err = ensureSequenceTable(s.db); err == nil {
			n, err = bumpSequence(ctx, db, orgID, part.value, ms.counterModel(model), spec.Key)
		}
	} else {
		n, err = nextSequenceValue(ctx, s.db, orgID, part.value, ms.counterModel(model), spec.Key)
	}
	if err != nil {
		return err
	}
	dest[col] = FormatSequenceAt(spec.Format, n, part.at, part.scope)
	return nil
}

// assignTransitionSequences issues, on `db` (the update transaction), the
// folios of every column bound to a sequence with assign_on
// "transition:<toStage>" whose current value is empty. merged is the record
// as it will be (persisted ∪ patch); numbers are written into input.
func (s *Service) assignTransitionSequences(ctx context.Context, db *gorm.DB, model string, user modelbase.AuthUser, toStage string, merged, input map[string]any) error {
	ms, ok := s.resolveSequences(ctx, model)
	if !ok {
		return nil
	}
	now := s.opClock(ctx, orgIDFromUser(user))
	cols := make([]string, 0, len(ms.ColumnBindings))
	for c := range ms.ColumnBindings {
		cols = append(cols, c)
	}
	sort.Strings(cols)
	for _, col := range cols {
		spec, ok := ms.spec(ms.ColumnBindings[col])
		if !ok || !assignsOnStage(spec, toStage) {
			continue
		}
		if v := merged[col]; v != nil && v != "" {
			continue // already numbered (re-entering the stage keeps its folio)
		}
		if err := s.stampOne(ctx, db, ms, spec, model, orgIDFromUser(user), user, col, merged, input, now); err != nil {
			return err
		}
		merged[col] = input[col]
	}
	return nil
}

// createStageOf is the stage a row is being created in ("" without a stage
// machine or when the input does not name one).
func (s *Service) createStageOf(ctx context.Context, model string, input map[string]any) string {
	sm := s.resolveStageMachine(ctx, model)
	if sm == nil {
		return ""
	}
	return StageValue(input[sm.Field])
}

// rejectExplicitGapless refuses a client-supplied value in a column bound to a
// GAPLESS sequence: an explicit folio would either leave a hole in the series
// or collide with a later number. The system principal (ETL / historical
// imports) is exempt, like on every other protected path.
func (s *Service) rejectExplicitGapless(ctx context.Context, model string, user modelbase.AuthUser, input map[string]any) error {
	if _, ok := user.(systemPrincipal); ok {
		return nil
	}
	ms, ok := s.resolveSequences(ctx, model)
	if !ok {
		return nil
	}
	ve := &ValidationError{}
	for col, key := range ms.ColumnBindings {
		if v, present := input[col]; !present || v == nil || v == "" {
			continue
		}
		if spec, ok := ms.spec(key); ok && spec.Gapless {
			ve.add(col, codeProtected, map[string]any{"sequence": key})
		}
	}
	if ve.Empty() {
		return nil
	}
	return ve
}
