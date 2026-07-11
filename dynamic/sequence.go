package dynamic

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"

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
	var value int64
	err := db.WithContext(ctx).Raw(`
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
// unchanged (validation rejects that shape at publish time).
func FormatSequence(format string, n int64) string {
	if format == "" {
		return strconv.FormatInt(n, 10)
	}
	return seqPlaceholderRe.ReplaceAllStringFunc(format, func(m string) string {
		sub := seqPlaceholderRe.FindStringSubmatch(m)
		if len(sub) == 2 && sub[1] != "" {
			width, _ := strconv.Atoi(sub[1])
			return fmt.Sprintf("%0*d", width, n)
		}
		return strconv.FormatInt(n, 10)
	})
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
func (s *Service) NextSequence(ctx context.Context, user modelbase.AuthUser, model, key string) (string, error) {
	ms, ok := s.resolveSequences(ctx, model)
	if !ok {
		return "", fmt.Errorf("dynamic: model %q declares no sequences", model)
	}
	spec, ok := ms.spec(key)
	if !ok {
		return "", fmt.Errorf("dynamic: model %q has no sequence %q", model, key)
	}
	orgID := orgIDFromUser(user)
	if orgID == uuid.Nil {
		return "", fmt.Errorf("dynamic: sequence_next requires a bound org")
	}
	n, err := nextSequenceValue(ctx, s.db, orgID, scopeValueFor(spec.Scope, user), model, key)
	if err != nil {
		return "", err
	}
	return FormatSequence(spec.Format, n), nil
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

// assignSequences stamps the next folio value onto every sequence-bound column
// that the caller left empty, mutating `input` in place before the create write.
// A column whose value the caller supplied is left untouched (an explicit folio
// wins, e.g. during an ETL import of historical numbers).
func (s *Service) assignSequences(ctx context.Context, model string, user modelbase.AuthUser, input map[string]any) error {
	ms, ok := s.resolveSequences(ctx, model)
	if !ok || len(ms.ColumnBindings) == 0 {
		return nil
	}
	orgID := orgIDFromUser(user)
	for col, key := range ms.ColumnBindings {
		if v, present := input[col]; present && v != nil && v != "" {
			continue
		}
		spec, ok := ms.spec(key)
		if !ok {
			return fmt.Errorf("dynamic: column %q binds unknown sequence %q on %q", col, key, model)
		}
		n, err := nextSequenceValue(ctx, s.db, orgID, scopeValueFor(spec.Scope, user), model, key)
		if err != nil {
			return err
		}
		input[col] = FormatSequence(spec.Format, n)
	}
	return nil
}
