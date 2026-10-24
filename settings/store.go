package settings

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/asteby/metacore-kernel/manifest"
)

// Scope kinds persisted in metacore_addon_settings.scope_kind.
const (
	ScopeOrg    = manifest.SettingScopeOrg
	ScopeBranch = manifest.SettingScopeBranch
)

// EventSettingsChanged is the event name published by Store.Write when a
// Publisher is configured.
const EventSettingsChanged = "settings.changed"

// eventEmitter is the addonKey the Store publishes under: the event is
// host-originated, not an addon's (same convention as the bus docs).
const eventEmitter = "kernel"

// Values is a bag of setting values keyed by setting key.
type Values = map[string]any

// Errors. Callers match with errors.Is.
var (
	// ErrConflict: expectedRev did not match the stored revision (or a
	// concurrent writer won). Re-read and retry.
	ErrConflict = errors.New("settings: revision conflict")
	// ErrSecretKey: the key is a secret / write-only setting and must be
	// stored through org_integrations, never here.
	ErrSecretKey = errors.New("settings: secret setting; use org_integrations")
	// ErrReservedKey: keys starting with "_" belong to the system.
	ErrReservedKey = errors.New("settings: reserved key (prefix \"_\")")
	// ErrUnknownKey: the manifest does not declare the key.
	ErrUnknownKey = errors.New("settings: key not declared by the installed manifest")
	// ErrNotBranchScoped: a branch write for a key not declared scope "branch".
	ErrNotBranchScoped = errors.New("settings: key is not branch-scoped in the manifest")
	// ErrInvalidValue: the value does not match the declared type.
	ErrInvalidValue = errors.New("settings: invalid value for declared type")
	// ErrInvalidScope: unknown scope kind or malformed scope id.
	ErrInvalidScope = errors.New("settings: invalid scope")
	// ErrNotInstalled: the DefSource has no manifest for (org, addon).
	ErrNotInstalled = errors.New("settings: addon not installed")
)

// DefSource returns the setting definitions of the manifest INSTALLED for
// (org, addon). It returns ErrNotInstalled (wrapped is fine) when there is none.
// The kernel does not know where hosts keep installed manifests, so the host
// provides this seam.
type DefSource interface {
	Defs(ctx context.Context, org uuid.UUID, addon string) ([]manifest.SettingDef, error)
}

// DefsFunc adapts a function to DefSource.
type DefsFunc func(ctx context.Context, org uuid.UUID, addon string) ([]manifest.SettingDef, error)

// Defs implements DefSource.
func (f DefsFunc) Defs(ctx context.Context, org uuid.UUID, addon string) ([]manifest.SettingDef, error) {
	return f(ctx, org, addon)
}

// Publisher is the subset of *events.Bus the Store needs. *events.Bus
// satisfies it as is.
type Publisher interface {
	Publish(ctx context.Context, addonKey, event string, orgID uuid.UUID, payload any) error
}

// Changed is the payload of the settings.changed event. It carries key NAMES
// only, never values.
type Changed struct {
	OrgID     uuid.UUID `json:"org_id"`
	AddonKey  string    `json:"addon_key"`
	ScopeKind string    `json:"scope_kind"`
	ScopeID   string    `json:"scope_id"`
	Rev       int64     `json:"rev"`
	Keys      []string  `json:"keys"`
	Actor     string    `json:"actor,omitempty"`
}

// Row is the gorm model of metacore_addon_settings (created by migration 0008).
type Row struct {
	OrgID     uuid.UUID  `gorm:"column:org_id;type:uuid;primaryKey"`
	AddonKey  string     `gorm:"column:addon_key;primaryKey"`
	ScopeKind string     `gorm:"column:scope_kind;primaryKey"`
	ScopeID   string     `gorm:"column:scope_id;primaryKey"`
	Values    Values     `gorm:"column:values;serializer:json;type:jsonb"`
	Rev       int64      `gorm:"column:rev"`
	UpdatedBy *uuid.UUID `gorm:"column:updated_by;type:uuid"`
	UpdatedAt time.Time  `gorm:"column:updated_at"`
}

// TableName implements gorm's Tabler.
func (Row) TableName() string { return "metacore_addon_settings" }

// Store reads and writes scoped settings rows.
type Store struct {
	DB   *gorm.DB
	Defs DefSource
	// Events is optional; when set, Write publishes settings.changed after the
	// row commits. Publish failures are logged, never returned.
	Events Publisher
}

// NewStore builds a Store. events may be nil.
func NewStore(db *gorm.DB, defs DefSource, events Publisher) *Store {
	return &Store{DB: db, Defs: defs, Events: events}
}

func checkScope(kind, id string) error {
	switch kind {
	case ScopeOrg:
		if id != "" {
			return fmt.Errorf("%w: org scope takes no scope id", ErrInvalidScope)
		}
	case ScopeBranch:
		if strings.TrimSpace(id) == "" || len(id) > 128 {
			return fmt.Errorf("%w: branch scope needs a scope id", ErrInvalidScope)
		}
	default:
		return fmt.Errorf("%w: kind %q", ErrInvalidScope, kind)
	}
	return nil
}

// Read returns the values stored for one scope and its revision. A scope with
// no row yields empty values and rev 0 (never an error). The result is a copy.
func (s *Store) Read(ctx context.Context, org uuid.UUID, addon, scopeKind, scopeID string) (Values, int64, error) {
	if err := checkScope(scopeKind, scopeID); err != nil {
		return nil, 0, err
	}
	return readRow(s.DB.WithContext(ctx), org, addon, scopeKind, scopeID)
}

func readRow(db *gorm.DB, org uuid.UUID, addon, kind, id string) (Values, int64, error) {
	return readRowLock(db, org, addon, kind, id, false)
}

// readRowLock is readRow; with lock set it takes SELECT ... FOR UPDATE (only
// meaningful on Postgres; SQLite serialises writers on its own).
func readRowLock(db *gorm.DB, org uuid.UUID, addon, kind, id string, lock bool) (Values, int64, error) {
	var rows []Row
	if lock && db.Dialector.Name() == "postgres" {
		db = db.Clauses(clause.Locking{Strength: "UPDATE"})
	}
	// Only values+rev: updated_at is TIMESTAMPTZ, which the sqlite test driver
	// does not parse into time.Time (it stays a string there).
	err := db.Select("values", "rev").Where("org_id = ? AND addon_key = ? AND scope_kind = ? AND scope_id = ?", org, addon, kind, id).
		Limit(1).Find(&rows).Error
	if err != nil {
		return nil, 0, err
	}
	if len(rows) == 0 {
		return Values{}, 0, nil
	}
	out := make(Values, len(rows[0].Values))
	for k, v := range rows[0].Values {
		out[k] = v
	}
	return out, rows[0].Rev, nil
}

func sensitive(d manifest.SettingDef) bool {
	return d.Secret || d.WriteOnly || d.Type == "secret"
}

// Write merges patch into the scope's values and returns the new revision.
//
//   - A nil value in patch removes the key (clears an override).
//   - expectedRev, when non-nil, must equal the stored revision (0 = "no row
//     yet"); otherwise ErrConflict. A nil expectedRev is last-writer-wins on
//     the merged keys.
//   - A patch that changes nothing is idempotent: no revision bump, no event,
//     the current revision is returned.
//   - The whole patch is validated first; on any error nothing is written.
func (s *Store) Write(ctx context.Context, org uuid.UUID, addon, scopeKind, scopeID string, patch Values, expectedRev *int64, actor uuid.UUID) (int64, error) {
	if err := checkScope(scopeKind, scopeID); err != nil {
		return 0, err
	}
	defs, err := s.Defs.Defs(ctx, org, addon)
	if err != nil {
		return 0, err
	}
	byKey := make(map[string]manifest.SettingDef, len(defs))
	for _, d := range defs {
		byKey[d.Key] = d
	}
	keys := make([]string, 0, len(patch))
	for k, v := range patch {
		if strings.HasPrefix(k, "_") {
			return 0, fmt.Errorf("%w: %q", ErrReservedKey, k)
		}
		def, ok := byKey[k]
		if !ok {
			if v == nil {
				// Clearing an orphan key (no longer declared) is always allowed.
				keys = append(keys, k)
				continue
			}
			return 0, fmt.Errorf("%w: %q", ErrUnknownKey, k)
		}
		if sensitive(def) {
			return 0, fmt.Errorf("%w: %q", ErrSecretKey, k)
		}
		if scopeKind == ScopeBranch && def.Scope != manifest.SettingScopeBranch && v != nil {
			return 0, fmt.Errorf("%w: %q", ErrNotBranchScoped, k)
		}
		if v != nil {
			if err := validateValue(def, v); err != nil {
				return 0, fmt.Errorf("%w: %q: %v", ErrInvalidValue, k, err)
			}
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var (
		newRev  int64
		changed bool
	)
	// expectedRev == nil is true last-writer-wins: writeOnce serialises on the
	// row lock, so no retries are needed. A lost race only happens (and maps
	// to ErrConflict) when expectedRev != nil.
	newRev, changed, err = s.writeOnce(ctx, org, addon, scopeKind, scopeID, patch, expectedRev, actor)
	if errors.Is(err, errRace) {
		err = ErrConflict
	}
	if err != nil {
		return 0, err
	}
	if changed && s.Events != nil && len(keys) > 0 {
		ev := Changed{OrgID: org, AddonKey: addon, ScopeKind: scopeKind, ScopeID: scopeID, Rev: newRev, Keys: keys}
		if actor != uuid.Nil {
			ev.Actor = actor.String()
		}
		if perr := s.Events.Publish(ctx, eventEmitter, EventSettingsChanged, org, ev); perr != nil {
			slog.Warn("settings.changed.publish_failed", "addon_key", addon, "org_id", org.String(), "err", perr.Error())
		}
	}
	return newRev, nil
}

var (
	errRace  = errors.New("settings: lost write race")
	errNoRow = errors.New("settings: nothing to create") // internal rollback sentinel
)

func (s *Store) writeOnce(ctx context.Context, org uuid.UUID, addon, kind, id string, patch Values, expectedRev *int64, actor uuid.UUID) (int64, bool, error) {
	var (
		rev     int64
		changed bool
	)
	err := s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		lww := expectedRev == nil
		if lww {
			// Ensure a row exists to lock on (rev 0 placeholder, invisible
			// until commit), then lock it: concurrent writers queue here and
			// each applies its patch on top of the previous committed state.
			ph := Row{OrgID: org, AddonKey: addon, ScopeKind: kind, ScopeID: id, Values: Values{}, Rev: 0, UpdatedAt: time.Now().UTC()}
			if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&ph).Error; err != nil {
				return err
			}
		}
		cur, curRev, err := readRowLock(tx, org, addon, kind, id, lww)
		if err != nil {
			return err
		}
		if expectedRev != nil && *expectedRev != curRev {
			return ErrConflict
		}
		next := make(Values, len(cur)+len(patch))
		for k, v := range cur {
			next[k] = v
		}
		for k, v := range patch {
			if v == nil {
				delete(next, k)
			} else {
				next[k] = v
			}
		}
		if curRev > 0 && jsonEqual(cur, next) {
			rev = curRev
			return nil
		}
		if curRev == 0 && len(next) == 0 {
			if lww {
				return errNoRow // roll back the placeholder: nothing to create
			}
			return nil // nothing stored, nothing to create
		}
		var by *uuid.UUID
		if actor != uuid.Nil {
			a := actor
			by = &a
		}
		now := time.Now().UTC()
		if curRev == 0 && !lww {
			row := Row{OrgID: org, AddonKey: addon, ScopeKind: kind, ScopeID: id, Values: next, Rev: 1, UpdatedBy: by, UpdatedAt: now}
			res := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&row)
			if res.Error != nil {
				return res.Error
			}
			if res.RowsAffected == 0 {
				return errRace // a concurrent insert of the same PK won
			}
			rev, changed = 1, true
			return nil
		}
		// Struct form + Select so the json serializer runs and a nil
		// updated_by is still written (a map would bypass the serializer).
		res := tx.Model(&Row{}).
			Where("org_id = ? AND addon_key = ? AND scope_kind = ? AND scope_id = ? AND rev = ?", org, addon, kind, id, curRev).
			Select("values", "rev", "updated_by", "updated_at").
			Updates(Row{Values: next, Rev: curRev + 1, UpdatedBy: by, UpdatedAt: now})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return errRace
		}
		rev, changed = curRev+1, true
		return nil
	})
	if errors.Is(err, errNoRow) {
		return 0, false, nil
	}
	if errors.Is(err, errRace) && expectedRev != nil {
		return 0, false, ErrConflict
	}
	return rev, changed, err
}
